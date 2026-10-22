// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// saveMu 串行化本地 state.json 落盘（tmp 写入 + rename + fsync 全在锁外做）。
	// 不得与 p.mu 形成固定嵌套顺序获取：saveMu 只包住写盘本身，绝不跨 p.mu 持有。
	saveMu sync.Mutex
	// flusherDone 在 startFlusher 时登记；Close 通过它 join 落盘 goroutine。
	flusherDone chan struct{}
	// persistSeq 快照采集序号（持 p.mu 自增）；persistWritten 已落盘的最高序号
	// （持 saveMu 读写）。用于丢弃「更晚采集、更早落盘」的乱序覆盖。
	persistSeq     uint64
	persistWritten uint64
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// costExploreInterval costTier 条件探索窗口（issue #136 方案 a′，SetCostExploreInterval
	// 注入；默认 defaultCostExploreInterval 30m）。tier 0 垄断 + tier 1 存在且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only（探索=搭车改道，零新增上游请求）。
	// 0 = 关停（完全回到现状行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 多至 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 只在探索事件时写入（tier 1 枯竭期间停走，陈旧无害）；不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（/status 透出）。
	costExploreEvents int64
	// exploreLock 保护 exploreLast/costExploreEvents。候选快照已降为读锁，
	// 探索节流状态改由这把独立小锁保护，避免为低频写入持池写锁。
	exploreLock sync.Mutex
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// creditFloor 积分保底（SetCreditFloor 注入；0 = 关闭，缺省即现状）。
	// 账号 credits < floor 时不再参与选号——防止收费请求把余额打穿、连免费模型都
	// 402 冷却到次日签到。签到回血（SetCreditsDetailed）越过 floor 即自动恢复。
	// 全池触底且无免费模型可接时选号返回 nil（硬语义：宁 503 不打穿）。
	// 收费与否的判据见 floorBlockedForModel：本地实测台账优先，缺失时用上游目录
	// 倍率（modelRateOf 注入）兜底，避免「无观测的高价新模型」绕过保底。
	creditFloor int64
	// dailyTokenLimit / modelDailyTokenLimit 账号级日 Token 护栏（SetDailyLimits
	// 注入；0 = 关闭）。只用于 pick 前过滤，不进入权重/成本分层/冷却状态。
	dailyTokenLimit      int64
	modelDailyTokenLimit int64
	// dailyUsage 今日 token 计数 provider（SetDailyTokenUsageProvider 注入）。
	// nil 或读取失败时护栏 fail-open；仅在持 p.mu 时调用，回调不得反向调用 Pool。
	dailyUsage DailyTokenUsageProvider
	// modelRateOf 按 (realm, 模型) 查上游目录积分倍率（"0.79" / "" = 未知）。
	// 由 main 用 upstream.Client.ModelRate 注入——pool 不依赖 upstream 包（避免
	// 循环依赖与分层破坏），nil 时倍率兜底不生效（退化为仅本地台账判定）。
	// 仅在持 p.mu 时由 floorBlockedForModel 调用；回调不得反向调用 Pool 方法。
	modelRateOf func(realm, model string) string
	// 加权路由的闲置补偿调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// preferExpiring 最早到期优先路由开关（默认 true）。开启且快过期窗口内存在有效
	// 批次时，选号在成本层内先按最早到期排序；关闭后只使用普通加权路由。
	preferExpiring bool
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅持 p.mu 读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// pickSeq 单调递增的选号序号：每次 pick 选中账号时自增并记到 entry.usedSeq，
	// 为 LRU 兜底/防惊群提供与 time.Now() 精度无关的严格全序（Windows ~0.5ms 精度下
	// lastUsed 墙钟会全等）。仅 pick 写锁路径读写，无需 atomic。
	pickSeq uint64
	// lastPrune 全池过期条目清理的最近时刻（unix 纳秒，atomic）。选号热路径
	// 用 CAS + 时间节流决定是否做一次清理，避免写锁全量扫描压住并发选号。
	lastPrune atomic.Int64
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
}

// UIDAvailableForModel 判断 uid 当前是否可参与指定模型的选号。它与会话粘性
// 分配所用的 WeightedAvailableUIDsForModelRealm 同口径（健康、日额度、积分保底、
// 在途上限），但只做只读存在性判定：不排序、不分配切片，供粘性命中热路径
// 逐请求校验绑定号。realm 为空表示不限定域。
func (p *Pool) UIDAvailableForModel(uid, model, realm string) bool {
	if uid == "" {
		return false
	}
	now := time.Now()
	dailyUsage, dailyOK, dailyLimit, modelLimit := p.dailyUsageSnapshot(now)
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok || (realm != "" && e.a.Realm() != realm) {
		return false
	}
	if !e.healthyForModel(now, model) {
		return false
	}
	if dailyTokenBlocked(uid, model, dailyUsage, dailyOK, dailyLimit, modelLimit) {
		return false
	}
	if p.floorBlockedForRealmModel(e, model, realm, now) {
		return false
	}
	return !p.inFlightFull(e)
}

// defaultBreaker* 熔断器默认参数（FreeBuff2API 参考口径）。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		preferExpiring:     true,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口（issue #136）。用户经
		// config 显式 "0" 关停（SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// Close 停止后台落盘 goroutine 并做最后一次落盘（幂等）。
// 进程退出前调用，消除 startFlusher 的 goroutine 泄漏；不调用也不影响正确性
// （进程退出即回收），仅是生命周期卫生。
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	// 等 flusher 真正退出（它可能正处于一轮落盘中间），再由 Flush 做最后一次
	// 同步落盘；否则两者会并发写同一个 tmp 文件或互相覆盖。
	if p.flusherDone != nil {
		<-p.flusherDone
	}
	p.Flush()
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用，
// issue #136）。0 = 关停（完全回到现状行为）；正值覆盖默认 30m。
// 注意：与 SetSoftRateMax「非正值保留默认」不同，0 在这里是**合法值**（关停开关，
// 与 config 的 "0" 关停语义对齐）——不设 0 语义就无法关停探索。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // 负值非法，保留现值
	}
	p.costExploreInterval = d
}

// CostExploreStatus 透出探索台账（/status 用）：累计探索事件数 + 各 (域, 模型)
// 的最近探索时刻（键内 \x1f 分隔符输出为 "|"，与 model_costs 行对照即可读出
// 「探索→毕业」全链路）。RLock 只读遍历；map 大小受「服务过的 (域, 模型)」集合
// 约束（与 modelCost 同界，天然有界）。
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.exploreLock.Lock()
	defer p.exploreLock.Unlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// 键 realm+"\x1f"+model → 输出 "|"（JSON 安全可读；\x1f 不可打印）。
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetWeights 注入加权路由的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetPreferExpiring 注入最早到期优先路由开关（main 从 config 解析后调用）。
func (p *Pool) SetPreferExpiring(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preferExpiring = enabled
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// CreditFloor 透出生效的积分保底值（/status 用）。0 = 关闭。
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// SetCreditFloor 注入积分保底线（main 从 config 解析后调用）。
// 0 = 关闭（缺省即现状，零回归）；负值非法保留原值（0）。
// 语义见 Pool.creditFloor 字段注释。
func (p *Pool) SetCreditFloor(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.creditFloor = n
	}
}

// SetModelRateOf 注入上游目录积分倍率查表（main 用 upstream.Client.ModelRate 装配）。
// 供积分保底兜底判定「未实测过的模型是否收费」——本地台账无观测时，不能因为
// 「没学过」就放行，否则高价新模型会把触底号一次性打穿（kimi-k3-1 实案：
// 全池无观测 → 保底全部放行 → 两笔扣 111 分打穿到 0 并硬冷却到次日 04:00）。
// fn 可为 nil（清注入）；回调只在持 p.mu 时被调用，不得反向调用 Pool 方法。
func (p *Pool) SetModelRateOf(fn func(realm, model string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.modelRateOf = fn
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
// 无快照、快照无 savedAt、或本地不存在/不可读时，都会被判定为"本地优先/跳过快照"，
// 同时打一条恢复来源日志。必须在 SyncToDir 之前调用（SyncToDir 只增删不入值）。
// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	// 条目可能已经删除；此时释放对在途计数是 no-op。
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号，不影响其他账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid, e := range p.byUID {
		if seen[uid] {
			continue
		}
		// 文件仍在磁盘上却没被本次扫描收录 → 该文件解析失败（半写/权限抖动/
		// NFS 瞬断），不是「用户删了账号」。此时绝不能删条目：删除会被立即
		// 持久化，重启后该账号的 credits/冷却/熔断/用量全部丢失且无法自动找回。
		if e != nil && e.a != nil && e.a.FilePath != "" {
			if _, statErr := os.Stat(e.a.FilePath); statErr == nil {
				log.Printf("WARN: [pool] auth 文件存在但解析失败，保留账号运行态 uid=%s file=%s",
					logfmt.UID8(uid), e.a.FilePath)
				continue
			}
		}
		delete(p.byUID, uid)
		changed = true
	}
	var sf stateFile
	var seq uint64
	if changed {
		p.dirty.Store(true)
		sf, seq = p.snapshotLocked()
	}
	p.mu.Unlock()
	if changed {
		// 落盘在锁外做（MarshalIndent + fsync 耗时可观，不得持全池写锁）。
		p.saveSnapshot(sf, seq)
	}
}

// Remove 从池中移除账号并立即落盘（管理面板用）。返回被移除账号的凭证
// （含 FilePath，供调用方删除 auth 文件）；uid 不存在返回 nil。
// 在途请求的 Release 对已删条目是 no-op，无需等待。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.Unlock()
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	sf, seq := p.snapshotLocked()
	p.mu.Unlock()
	// 删除必须立即落盘（否则已删账号会在下次启动被 load() 复活）；写盘在锁外做。
	p.saveSnapshot(sf, seq)
	return e.a
}

// RemoveMany 批量移除账号，返回实际移除的 UID 与不存在的 UID。
//
// 为什么需要单独的批量入口而不再循环调 Remove：Remove 每次都
// snapshotLocked + saveSnapshot，而 saveSnapshot 要整份 state 序列化、fsync、
// rename 并镜像一份到 Redis。470 个号循环 = 470 次全量写盘（O(n²) 字节 + 470
// 次 fsync + 470 次 Redis 镜像），必然超过面板 API 的 15s 超时。这里只在全部
// 删除完成后采集一次快照、落盘一次。
//
// 语义与 Remove 保持一致：删除必须立即落盘（否则已删账号会在下次启动被 load()
// 复活）；在途请求的 Release 对已删条目是 no-op，无需等待。调用方若需 auth
// 文件路径，必须在调用前用 AuthByUID 取：条目删除后不可再查。
func (p *Pool) RemoveMany(uids []string) (removed []string, missing []string) {
	if len(uids) == 0 {
		return nil, nil
	}
	p.mu.Lock()
	seen := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		if _, ok := p.byUID[uid]; !ok {
			missing = append(missing, uid)
			continue
		}
		delete(p.byUID, uid)
		removed = append(removed, uid)
	}
	if len(removed) == 0 {
		p.mu.Unlock()
		return removed, missing
	}
	p.dirty.Store(true)
	sf, seq := p.snapshotLocked()
	p.mu.Unlock()
	// 写盘在锁外做（序列化 + fsync 耗时可观，不得持全池写锁）。
	p.saveSnapshot(sf, seq)
	return removed, missing
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	weight, note, _, _ := a.ProfileSnapshot()
	if e, ok := p.byUID[a.UID]; ok {
		hasFile := a.FilePath != ""
		// 凭证新鲜度守卫：同一个 UID 可能被一份**更旧的**导出文件重新 upsert
		// （批量导入 cockpit 导出、重复登录拿到旧快照）。无条件 e.a = a 会把
		// 运行期 refresh 出来的新 token 覆盖回旧 token——下一次请求 401，且旧
		// refresh token 往往已被 rotate 失效，只能重新登录。
		//
		// 只有「新对象 token 不同、且过期时间更早、且池内 token 仍有效」这一
		// 证据充分的情形才拒绝替换；其余（新号、同 token、池内已过期）照常
		// 覆盖，保持既有语义与「用户显式换凭证」的能力。
		if e.a != nil && e.a.SupersededBy(a) {
			log.Printf("WARN: [pool] 忽略较旧的凭证 upsert uid=%s（保留运行期已刷新的 token）",
				logfmt.UID8(a.UID))
			// 资料仍按新对象同步（weight/note 是面板可编辑的显式意图），
			// 但不碰 e.a 的 token 字段。
			if hasFile {
				e.weight = weight
				e.note = note
			}
			return
		}
		e.a = a // 保留 credits/cooling 状态
		// state.json / Redis 快照恢复出的资料镜像在 auth 重新加载前仍是唯一来源；
		// 来自 state 的 placeholder（FilePath 为空）不能把资料清空。真实 auth
		// 文件则拥有权威值：只有它在场时才允许把 weight 改回 1、note 清空，
		// 否则手工清空后旧镜像会一直粘住。启动顺序是 load(state) -> SyncToDir(auth)，
		// 因此这里最终会以 auth 文件为准。
		if hasFile {
			e.weight = weight
			e.note = note
		} else {
			if weight > 1 {
				e.weight = weight
			}
			if note != "" {
				e.note = note
			}
		}
		return
	}
	p.byUID[a.UID] = &entry{a: a, weight: weight, note: note}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。

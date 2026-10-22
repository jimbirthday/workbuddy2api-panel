// 选号：Pick 簇（healthy 成本分层 + 快过期虚拟实例权重 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知缺省账号级）。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "")
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 挑选策略：healthy 账号中按权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel 模型感知选号：等同 PickExcluding，但对「6004 模型级冷却中的
// 账号」进行模型豁免——请求模型与其 trigger 模型不同时视为可用（issue #31）。
// reqModel 为空时即普通 PickExcluding（不影响既有调用语义）。
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// PickExcludingForRealm 模型感知 + 分池选号：候选集先按 Realm()==realm 过滤
// （realm 空 = 不过滤，退化为 PickExcludingForModel），再按模型健康口径判定。
// 供 handler 在 global/cn 双域下分流（global 模型请求只路由 global 账号）。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// pickCandidate 是选号排序/抽签的中间行，也是跨锁阶段的候选快照。
//
// 选号不再在 p.mu 写锁内做 O(n log n) 排序：锁内只筛选候选并复制排序/抽签
// 所需的标量字段，锁外计算权重与排序，最后再用一次短写锁复核并提交
// lastUsed/usedSeq。快照避免锁外读取 entry 的受保护字段（credits/lastUsed/
// modelCost 等都会与冷却、签到、后台刷新并发）。
type pickCandidate struct {
	uid                   string
	credits               int64
	lastUsedNano          int64
	creditsEarliestNano   int64
	usedSeq               uint64
	creditsExpiring       int64
	creditsEarliestRemain int64
	manualWeight          int
	tier                  int
	cost1k                float64
	w                     float64
}

func candidateFromEntry(uid string, e *entry, tier int, cost1k float64) pickCandidate {
	return pickCandidate{
		uid:                   uid,
		credits:               e.credits,
		lastUsedNano:          e.lastUsed.UnixNano(),
		creditsEarliestNano:   e.creditsEarliestExpiry.UnixNano(),
		usedSeq:               e.usedSeq,
		creditsExpiring:       e.creditsExpiring,
		creditsEarliestRemain: e.creditsEarliestRemaining,
		manualWeight:          e.manualWeight(),
		tier:                  tier,
		cost1k:                cost1k,
	}
}

func (c pickCandidate) lastUsed() time.Time {
	if c.lastUsedNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, c.lastUsedNano)
}

func (c pickCandidate) earliestExpiry() time.Time {
	if c.creditsEarliestNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, c.creditsEarliestNano)
}

type pickScratch struct {
	cands    []pickCandidate
	order    []int32
	eligible []int32
}

// pickScratchPool 为每次选号提供独立的候选切片。不能继续把 scratch 挂在
// Pool 上：权重计算与排序已移到锁外，Pool 级共享缓冲会被并发 Pick 同时改写。
var pickScratchPool = sync.Pool{New: func() any { return &pickScratch{} }}

// HasRotatable 报告本 realm、本模型上是否还存在未 tried 的可用账号。
// 与 Pick 的候选口径一致（healthyForModel + 日额度 + 积分保底 + 在途），
// 但只做存在性判定并在首个命中处返回：限流轮转每次都要问这个问题，旧实现
// 会构造并排序全部可用 UID，账号多时是无谓的 O(n log n) + 整表分配。
// 注意：不能用 Pick 代替——全冷却时 Pick 会捞最早到期的冷却号，而这里要求
// “确实还有一个没试过、现在就能路由的号”。
func (p *Pool) HasRotatable(tried map[string]bool, reqModel, realm string) bool {
	now := time.Now()
	dailyUsage, dailyOK, dailyLimit, modelLimit := p.dailyUsageSnapshot(now)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, reqModel) {
			continue
		}
		if dailyTokenBlocked(uid, reqModel, dailyUsage, dailyOK, dailyLimit, modelLimit) {
			continue
		}
		if p.floorBlockedForRealmModel(e, reqModel, realm, now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		return true
	}
	return false
}

// pick 在 healthy 候选集中按权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域）。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	a, eventLog := p.pickLocked(tried, reqModel, realm)
	// 事件日志在**释放锁之后**打：log.Printf 会写 stderr（可能是慢终端/管道），
	// 持写锁打日志会把整池的选号/状态查询一起堵在这一行上。
	if eventLog != "" {
		log.Print(eventLog)
	}
	return a
}

// pickLocked 是 pick 的持锁主体，返回选中账号与一条待打的事件日志（空串 = 无）。
// 调用方必须**在锁外**打这条日志（见 pick）。
func (p *Pool) pickLocked(tried map[string]bool, reqModel, realm string) (*auth.Auth, string) {
	now := time.Now()
	dailyUsage, dailyOK, dailyLimit, modelLimit := p.dailyUsageSnapshot(now)
	scratch := pickScratchPool.Get().(*pickScratch)
	defer pickScratchPool.Put(scratch)

	// 候选快照后在锁外排序；最终提交前状态若已全部失效，重拍一次快照。
	// 重试有上限，避免账号状态剧烈变化时在选号热路径里死循环。
	// 过期清理从选号热路径移到这里：时间节流 + 写锁一次，避免每跳选号都持写锁
	// 全量扫描 + 逐号清理（写锁序列化是此前并发选号的最大 CPU/尾延迟来源）。
	p.pruneExpiredIfDue(now)
	for attempt := 0; attempt < 3; attempt++ {
		cfg := pickConfig{
			idleWeightPerHour:   p.idleWeightPerHour,
			idleWeightMax:       p.idleWeightMax,
			preferExpiring:      p.preferExpiring,
			randInt64N:          p.randInt64N,
			costExploreInterval: p.costExploreInterval,
		}
		// 候选快照只读，用 RLock：并发选号此前被写锁串行化（持写锁跑 470 号的
		// 全量扫描 + 每号过期清理），现在多个请求可并行读；过期清理移到
		// 时间节流的维护路径（pruneExpiredLocked），不再压在每跳选号上。
		p.mu.RLock()
		cands, bestTier, explored := p.pickSnapshotRLocked(scratch, tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit, &cfg)
		p.mu.RUnlock()
		if len(cands) == 0 {
			// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
			// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
			a, eventLog := p.pickEarliestExpiryLocked(tried, now, realm, reqModel, dailyUsage, dailyOK, dailyLimit, modelLimit)
			return a, eventLog
		}

		// 锁外只读快照字段：成本分层已在锁内按每个候选算好，这里做 O(n) 权重
		// 预计算与排序。旧实现把整段排序放在池写锁内，470 号时所有并发选号被
		// 同一把锁串行化；快照后锁外排序可让多个请求真正并行。
		var maxCredits int64
		for i := range cands {
			if cands[i].credits > maxCredits {
				maxCredits = cands[i].credits
			}
		}
		// order 只存候补下标：按 int32（48 字节/候选）搬运，远小于整条快照
		// 结构体（>100 字节），duffcopy 开销随候选数线性下降。
		order := scratch.order[:0]
		for i := range cands {
			if cands[i].tier == bestTier {
				cands[i].w = routingWeightOfCandidate(cands[i], maxCredits, now, &cfg)
				order = append(order, int32(i))
			}
		}
		scratch.order = order

		// 等权重洗牌：仅当存在权重相等且候选数超过 top5 时，才对 order 做 Fisher-Yates
		// 洗牌（且**不消耗 p.randInt64N 注入源**，避免改变 pickWeighted 的确定性语义，
		// 见 TestPickDeterministicViaSetRandomSource）。权重全等或存在并列时，按字典序
		// 截断会让 uid 靠后的账号永远进不了 top5（等权重账号被字典序饿死、LRU 兜底
		// 又只在 top5 内转——惊群集中单号的根因）。洗牌用独立的 time-seeded 源，
		// 只在截断边界制造等权重随机次序，不影响加权抽签本身的确定性。
		if len(order) > 5 {
			eq := false
			for i := 1; i < len(order); i++ {
				if cands[order[i]].w == cands[order[0]].w {
					eq = true
					break
				}
			}
			if eq {
				shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(order))))
				shuf.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			}
		}
		// slices.SortFunc 是泛型原地排序，不经过 reflect.Swapper；该比较器在
		// (cost1k, 权重, UID) 上已构成严格全序。
		slices.SortFunc(order, func(a, b int32) int {
			ca, cb := &cands[a], &cands[b]
			if ca.cost1k != cb.cost1k {
				if ca.cost1k < cb.cost1k {
					return -1 // 收费层：单价低的在前
				}
				return 1
			}
			if ca.w != cb.w {
				if ca.w > cb.w {
					return -1
				}
				return 1
			}
			return strings.Compare(ca.uid, cb.uid)
		})

		// top5 是短名单；加权抽签只在这 5 个里选。eligible 为空时说明 top5 全部
		// 处于 minPickGap 内，走全候选 usedSeq 最旧的 LRU 兜底（与旧实现一致）。
		top := order
		if len(top) > 5 {
			top = top[:5]
		}
		eligible := scratch.eligible[:0]
		for _, i := range top {
			if now.Sub(cands[i].lastUsed()) >= minPickGap {
				eligible = append(eligible, i)
			}
		}
		scratch.eligible = eligible
		var chosen int32
		if len(eligible) > 0 {
			var weights [5]int64
			chosen = pickWeightedCandidate(cands, eligible, &weights, &cfg)
		} else {
			chosen = order[0]
			for _, i := range order[1:] {
				if cands[i].usedSeq < cands[chosen].usedSeq {
					chosen = i
				}
			}
		}

		// 短写锁复核：minPickGap、healthy、日额度、积分保底、在途上限都在锁内
		// 重新确认。首选失效（锁外窗口内被并发冷却/选走）时按排序顺序回退，
		// 再不行按 usedSeq 从全候选兜底。
		p.mu.Lock()
		e, ok := p.commitLocked(cands, order, chosen, tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit)
		if ok {
			if explored {
				_, _, nickname, _ := e.a.ProfileSnapshot()
				eventLog := fmt.Sprintf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
					reqModel, realm, logfmt.Label(e.a.UID, nickname), cfg.costExploreInterval)
				p.mu.Unlock()
				return e.a, eventLog
			}
			p.mu.Unlock()
			return e.a, ""
		}
		p.mu.Unlock()
	}
	return nil, ""
}

type pickConfig struct {
	idleWeightPerHour   float64
	idleWeightMax       float64
	preferExpiring      bool
	randInt64N          func(n int64) int64
	costExploreInterval time.Duration
}

// pickSnapshotRLocked 在 p.mu 读锁内筛选候选并复制排序/抽签所需的全部标量字段。
// 调用方持有 p.mu.RLock；锁外不得再读 entry 的受保护字段。快照只读：过期清理
// 交给 pruneExpiredLocked 维护路径（时间节流），本函数不再删除任何 entry 字段。
func (p *Pool) pickSnapshotRLocked(scratch *pickScratch, tried map[string]bool, reqModel, realm string, now time.Time, dailyUsage DailyTokenUsageByUID, dailyOK bool, dailyLimit, modelLimit int64, cfg *pickConfig) ([]pickCandidate, int, bool) {
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	floorBlocked := func(e *entry) bool { return p.floorBlockedForRealmModel(e, reqModel, realm, now) }

	if cap(scratch.cands) < len(p.byUID) {
		scratch.cands = make([]pickCandidate, 0, len(p.byUID))
	}
	cands := scratch.cands[:0]
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) {
			continue
		}
		if dailyTokenBlocked(uid, reqModel, dailyUsage, dailyOK, dailyLimit, modelLimit) {
			continue // 今日额度用尽：仅本轮跳过，不 park 账号
		}
		if floorBlocked(e) {
			continue // 积分保底：触底号不接实测收费模型（tier 0/1 不受限）
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		tier, cost1k := 1, 0.0
		if mc, ok := e.modelCostOf(reqModel, now); ok {
			if mc.CostPer1k <= 0 {
				tier = 0
			} else {
				tier, cost1k = 2, mc.CostPer1k
			}
		}
		cands = append(cands, candidateFromEntry(uid, e, tier, cost1k))
	}
	if len(cands) == 0 {
		return nil, 0, false
	}
	scratch.cands = cands

	bestTier := 2
	for _, c := range cands {
		if c.tier < bestTier {
			bestTier = c.tier
		}
	}
	// 条件探索（issue #136 方案 a′）：tier 0 垄断层存在且候选含 tier 1 且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only。timer 同锁写入，并发 pick
	// 只有一个进入者能通过窗口判定（天然防重复探索）。
	explored := false
	if cfg.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		hasTier1 := false
		for _, c := range cands {
			if c.tier == 1 {
				hasTier1 = true
				break
			}
		}
		key := realm + "\x1f" + reqModel
		if hasTier1 {
			// exploreLast 是探索节流状态：快照已降为读锁，这里不能与 byUID 共用
			// p.mu，独立小锁保护（写入频率 ~30m 一次，锁外候选排序零竞争）。
			p.exploreLock.Lock()
			if now.Sub(p.exploreLast[key]) >= cfg.costExploreInterval {
				p.exploreLast[key] = now
				p.costExploreEvents++
				bestTier = 1
				explored = true
			}
			p.exploreLock.Unlock()
		}
	}
	return cands, bestTier, explored
}

// commitCandidateLocked 用短写锁复核候选资格并提交 lastUsed/usedSeq。
// 快照排序只决定偏好顺序；真正提交前必须重新确认账号仍存在、未 tried、仍健康、
// 日额度/积分保底/在途上限未失效，避免锁外窗口内状态变化后返回无效号。
func (p *Pool) commitLocked(cands []pickCandidate, order []int32, chosen int32, tried map[string]bool, reqModel, realm string, now time.Time, dailyUsage DailyTokenUsageByUID, dailyOK bool, dailyLimit, modelLimit int64) (*entry, bool) {
	// 首选：锁外加权抽签结果；若在快照后被别的并发 pick 抢先，按 top5 排序回退。
	if e, ok := p.commitOneLocked(&cands[chosen], tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit); ok {
		return e, true
	}
	limit := len(order)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		if int32(i) == chosen {
			continue
		}
		if e, ok := p.commitOneLocked(&cands[order[i]], tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit); ok {
			return e, true
		}
	}
	// top5 全部失效/刚被用过：先在 top5 之外找仍满足 minPickGap 的候选，避免
	// 一批同快照并发请求在 top5 被逐个提交后集体回落到 LRU、再次撞到刚用过的号。
	for i := limit; i < len(order); i++ {
		if e, ok := p.commitOneLocked(&cands[order[i]], tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit); ok {
			return e, true
		}
	}
	// 全候选都刚被用过（或均失效）：LRU 兜底在所有仍有效候选里选 usedSeq 最小者。
	var best *entry
	var bestSeq uint64
	for i := range order {
		c := &cands[order[i]]
		e, ok := p.pickCandidateValidLocked(c, tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit)
		if !ok {
			continue
		}
		if best == nil || e.usedSeq < bestSeq {
			best, bestSeq = e, e.usedSeq
		}
	}
	if best == nil {
		return nil, false
	}
	p.markPickedLocked(best, now)
	return best, true
}

func (p *Pool) markPickedLocked(e *entry, now time.Time) {
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
}

// commitOneLocked 校验单个候选（含 minPickGap）并提交；不满足返回 false。
func (p *Pool) commitOneLocked(c *pickCandidate, tried map[string]bool, reqModel, realm string, now time.Time, dailyUsage DailyTokenUsageByUID, dailyOK bool, dailyLimit, modelLimit int64) (*entry, bool) {
	e, ok := p.pickCandidateValidLocked(c, tried, reqModel, realm, now, dailyUsage, dailyOK, dailyLimit, modelLimit)
	if !ok {
		return nil, false
	}
	if now.Sub(e.lastUsed) < minPickGap {
		return nil, false
	}
	p.markPickedLocked(e, now)
	return e, true
}

// pickCandidateValidLocked 在持锁状态下复核快照候选是否仍可路由。
func (p *Pool) pickCandidateValidLocked(c *pickCandidate, tried map[string]bool, reqModel, realm string, now time.Time, dailyUsage DailyTokenUsageByUID, dailyOK bool, dailyLimit, modelLimit int64) (*entry, bool) {
	e, ok := p.byUID[c.uid]
	if !ok || tried != nil && tried[c.uid] {
		return nil, false
	}
	if realm != "" && e.a.Realm() != realm {
		return nil, false
	}
	if !e.healthyForModel(now, reqModel) {
		return nil, false
	}
	if dailyTokenBlocked(c.uid, reqModel, dailyUsage, dailyOK, dailyLimit, modelLimit) {
		return nil, false
	}
	if p.floorBlockedForRealmModel(e, reqModel, realm, now) {
		return nil, false
	}
	if p.inFlightFull(e) {
		return nil, false
	}
	return e, true
}

// routingWeightOfCandidate 与 routingWeightOf 同口径，但只吃锁内快照字段：
// 锁外排序阶段不得再读 entry.credits/lastUsed/expiring 等受保护字段。
func routingWeightOfCandidate(c pickCandidate, maxCredits int64, now time.Time, cfg *pickConfig) float64 {
	w := 1.0
	if maxCredits > 0 {
		w += float64(c.credits) / float64(maxCredits) * 10
	}
	if c.lastUsedNano == 0 {
		w += cfg.idleWeightMax
	} else {
		hours := now.Sub(c.lastUsed()).Hours()
		idleW := hours * cfg.idleWeightPerHour
		if idleW > cfg.idleWeightMax {
			idleW = cfg.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0
		}
		w += idleW
	}
	if c.manualWeight > 1 {
		w *= float64(c.manualWeight)
	}
	if cfg.preferExpiring &&
		c.creditsExpiring > 0 &&
		c.creditsEarliestRemain > 0 &&
		c.creditsEarliestNano != 0 &&
		c.earliestExpiry().After(now) {
		w *= expiringVirtualSlots
	}
	return w
}

// pickWeightedCandidate 与 pickWeighted 同语义，但直接使用快照权重字段，
// 不访问 entry。随机源仍遵循 cfg.randInt64N 的测试注入契约。
func pickWeightedCandidate(cands []pickCandidate, eligible []int32, weights *[5]int64, cfg *pickConfig) int32 {
	var total int64
	for i, idx := range eligible {
		weights[i] = int64(cands[idx].w * 1_000_000)
		total += weights[i]
	}
	rnd := rand.Int64N
	if cfg.randInt64N != nil {
		rnd = cfg.randInt64N
	}
	if total <= 0 {
		return eligible[int(rnd(int64(len(eligible))))]
	}
	r := rnd(total)
	var acc int64
	for i, idx := range eligible {
		acc += weights[i]
		if r < acc {
			return idx
		}
	}
	return eligible[len(eligible)-1]
}

// floorBlockedForRealmModel 积分保底拦截判定（pick 普通轮换与 PickByUIDForModel
// 粘性路径的单一事实来源）：floor>0 且账号触底（credits < floor）且该模型**收费**
// 时为真。
//
// 收费判据两级（任一成立即判收费 → 拦）：
//  1. 本地实测台账（e.modelCostOf）：该号在该模型上实测 cost>0（tier 2）。
//  2. 上游目录倍率（p.modelRateOf）：本地无观测/观测过期（tier 1）时的兜底。
//     只看实测会让「无观测」恒等于「放行」——而高价新模型恰恰全池无观测
//     （kimi-k3-1 实案：x1.62、223 分/百万 token，两笔打穿 100 分的号并触发
//     硬冷却到次日 04:00）。倍率由上游随模型目录下发，请求前即已知，不必付学费。
//
// 不拦的情形：
//   - 模型免费：本地实测 cost<=0（tier 0），或目录倍率为 0/"0.00"。保底的目的
//     正是「留余额给免费模型用」——但若账号已归零，上游仍会 402（余额门禁是
//     账号级的，与模型无关），此时由 ErrHardCredit 冷却承接，与本判定无关。
//   - 模型倍率未知（台账无观测且目录未下发该模型）：无法判收费，按放行处理。
//     这是有意的保守选择——目录未覆盖的模型多为内部/别名模型，拦了会让号
//     永久失联；风险由「未知」本身承担，但已知收费的一律拦。
//   - model 为空（无模型上下文）不拦：无成本维度，floor 无从判收费。
//
// 余额用本地插值口径（签到权威值 - 每笔 usage.credit 实扣，见 NoteModelCost）：
// 只会偏低不会偏高（官方对账延迟方向安全），正是保底需要的安全方向。
// 调用方必须已持有 p.mu（读 e.credits / e.modelCost / p.modelRateOf）。
// floorBlockedForRealmModel 同上，但带 realm 上下文（倍率按 (realm, 模型) 分桶，
// 同名模型在 CN / global 两域倍率可不同）。realm 为空表示调用方未限定域（结果会
// 混入两域账号）：此时不能只查空域键（上游会把空域归一成 cn），否则 global 触底号
// 会绕过保底。两个桶分别判定，只有都明确免费/未知才放行；任一边收费即拦。
func (p *Pool) floorBlockedForRealmModel(e *entry, model, realm string, now time.Time) bool {
	if p.creditFloor <= 0 || model == "" || e.credits >= p.creditFloor {
		return false
	}
	// 1) 本地实测台账：最权威（真实扣费证据）。
	if mc, ok := e.modelCostOf(model, now); ok {
		return mc.CostPer1k > 0
	}
	// 2) 上游目录倍率兜底：无实测观测时用牌价判收费，堵住「无观测 = 放行」漏洞。
	if p.modelRateOf == nil {
		return false
	}
	if realm == "" {
		return p.paidRateForAnyRealm(model, "cn", "global")
	}
	return p.isPaidRate(p.modelRateOf(realm, model))
}

// paidRateForAnyRealm 对多个域查同一模型倍率：任一域收费即视为收费。
// 空域/未知倍率仍按「无法判收费 → 放行」处理，与单域判定一致。
func (p *Pool) paidRateForAnyRealm(model string, realms ...string) bool {
	for _, r := range realms {
		if p.isPaidRate(p.modelRateOf(r, model)) {
			return true
		}
	}
	return false
}

func (p *Pool) isPaidRate(rate string) bool {
	if rate == "" {
		return false // 目录未覆盖：未知，放行（见上方注释）
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil {
		return false // 倍率非数值（异常形态）：不据此惩罚账号
	}
	return v > 0
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
// 本函数自取写锁（内部会推进 pickSeq/usedSeq/lastUsed）；调用方不得持锁调用。
//
// 积分保底同样在此生效（model 非空时）：floor 把健康号全部拦掉后 cands 为空会走到
// 这里，若兜底不看保底，触底号会被「捞回来」继续接收费模型——表现为同一条
// floor WARN 反复刷同一个号（实测：credits=1 < floor=150 仍持续中选）。
// 兜底是**最后一道**选号路径，保底在它之前挡不住就等于没挡。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, model string, dailyUsage map[string]DailyTokenUsage, dailyOK bool, dailyLimit, modelLimit int64) (*auth.Auth, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled || e.paused {
			continue // 禁用/暂停选号的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if dailyTokenBlocked(uid, model, dailyUsage, dailyOK, dailyLimit, modelLimit) {
			continue // 兜底路径同口径：今日额度用尽的号不能被捞回来
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue // 积分保底：触底号不接收费模型（兜底路径同判据）
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil, ""
	}
	// 文案在锁内组装、由调用方在锁外打印（同 pick 的 eventLog：持写锁打日志会
	// 把整池选号堵在 stderr 上）。
	eventLog := fmt.Sprintf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.NicknameValue()), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a, eventLog
}

// pruneExpiredEvery 过期条目清理的最小间隔。清理不再是每跳选号的开销：entry 的
// modelCooldowns / modelCost 增长受「服务过的模型名」约束（天然有界），节流到
// 分钟级足够回收内存；读路径（healthyForModel / modelCostOf / status）本就跳过
// 过期项，节流清理不改变任何可观察语义。
var pruneExpiredEvery = 30 * time.Second

// pruneExpiredIfDue 时间节流地回收全池过期的模型级冷却与成本台账。选号热路径
// 只做一次原子时间戳判断；到期才取写锁做一轮全量清理（470 号 ~微秒级）。
func (p *Pool) pruneExpiredIfDue(now time.Time) {
	last := time.Unix(0, p.lastPrune.Load())
	if now.Sub(last) < pruneExpiredEvery {
		return
	}
	// CAS 防并发：只有一个 pick 进入清理，其余看到新时间戳直接跳过。
	if !p.lastPrune.CompareAndSwap(last.UnixNano(), now.UnixNano()) {
		return
	}
	p.mu.Lock()
	for _, e := range p.byUID {
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
	}
	p.mu.Unlock()
}

// inFlightFull 报告账号是否已占满在途名额（上限按 realm 分档，见 inFlightLimit；
// limit=0 不限 → 恒 false）。调用方需已持 p.mu（读锁或写锁均可，本方法只读上限）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// pickWeighted 加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + idleWeight
//
//	  - credits 比例 = 该号 credits / 候选集内最大 credits（避免量纲爆炸）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//
// credits 全 0 时仍按 idleWeight 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
func (p *Pool) pickWeighted(cands []*entry, weights *[5]int64, now time.Time) *entry {
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	var total int64
	for i, e := range cands {
		w := p.routingWeightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf 计算单个账号的普通加权分值。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// 3.（原「成功率 ×3」因子已删，对齐上游 success-ema-review：errTotal 是终身
	// 累计、只增不减，成功率 = successCount/(successCount+errTotal) 会让早期出过错
	// 的号被永久压权且永不恢复；瞬时健康信号已由冷却/熔断/连败降权承接。）
	return w
}

// expiringNow 报告账号是否存在当前仍有效的快过期积分批次。
func expiringNow(e *entry, now time.Time) bool {
	return e.creditsExpiring > 0 &&
		e.creditsEarliestRemaining > 0 &&
		!e.creditsEarliestExpiry.IsZero() &&
		e.creditsEarliestExpiry.After(now)
}

// routingWeightOf 在普通账号权重上叠加快过期虚拟实例数量。prefer_expiring=false
// 或账号无有效快过期批次时，实例数恒为 1，结果与旧 weightOf 完全一致。
func (p *Pool) routingWeightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := p.weightOf(e, maxCredits, now)
	// 手动权重 >1：hub 同义「优先接单」。在普通权重上乘用户设定值，使
	// 2-100 的账号按比例分摊流量；权重 1 完全不改变现有行为。
	if mw := e.manualWeight(); mw > 1 {
		w *= float64(mw)
	}
	if p.preferExpiring && expiringNow(e, now) {
		return w * expiringVirtualSlots
	}
	return w
}

// SetCredits 更新账号余额。

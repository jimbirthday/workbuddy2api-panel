// 持久化：本地 state.json 落盘/加载、Redis 快照镜像（StoreSnapshotter）、
// 后台 flusher、择新恢复（RestoreFromSnapshot）。
package pool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

var flushInterval = 5 * time.Second

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool 账号池。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
//
// 本地**可用**（存在且 JSON 能解析、能装载）时按新旧择一（快照不早于本地 → 采用快照，
// 否则本地优先）；本地**不可用**（state.json 缺失/不可读，或**存在但解析装载失败**，
// 典型为半截写入/损坏文件）时**采用快照**——损坏的本地文件不是可"优先"的状态，若因
// 其 mtime 较新就顶掉更新鲜的 Redis 快照，全池冷却/熔断/成本台账会被清零。
// 分支情形：无快照 / 快照无 savedAt → 本地优先（无判据可比）。
//
// 每种情形都打一条对应的恢复来源日志，便于对账。必须在 SyncToDir 之前调用
// （SyncToDir 只增删不入值：值只能来自本地 load 或本函数采用快照）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	if localErr == nil {
		// 「存在」不等于「可用」：还要能真正解析装载。损坏/半截写入的 state.json
		// （典型为掉电中断）若仅凭 mtime 较新就顶掉更新的 Redis 快照，全池冷却/熔断/
		// 成本台账会被清零。判为不可用后由下面的 adoptSnapshot 分支接管。
		// 只做可用性判定、不重复 apply：本地状态已在 New → load() 时应用过。
		localErr = stateLoadableErr(p.stateFp)
	}
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr != nil {
		// 本地不可用 → 采用快照（本地没有可"优先"的状态）。
		//
		// 旧实现把该情形与「本地较新」合并成同一个 fall-through：既不改内存、不置 dirty
		//（有效快照被静默丢弃），又打出"本地 state.json（较新于 Redis 快照 …）"——一次
		// 从未发生过的比较，把排障引向根本不存在的本地文件；随后 SyncToDir 只增删不入值，
		// 全池运行态（credits/冷却/熔断计数/usedSeq/lastUsed）被清零。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)（本地 state.json 不可用: %v）",
			snap.SavedAt.Format(time.RFC3339), localErr)
		return
	}
	if !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	// 走到这里必然是「本地存在且严格新于快照」，日志结论属实。
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	// 在启动 goroutine 前同步写入（避免与测试对 flushInterval 的恢复写竞争）；
	// stopCh 同步登记，Close 才能可靠停止（New 与 startFlusher 之间无并发窗口）。
	p.stopCh = stopCh
	interval := flushInterval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.persistDirty()
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
func (p *Pool) Flush() {
	p.persistDirty()
}

// readStateFile 读取并解析一份本地 state.json。缺文件、不可读、JSON 解析失败（含字段
// 类型不符）都返回非 nil。json.Unmarshal 的"部分成功"不算成功——它会把能解出的字段写进
// 返回值并同时返回错误，此时语义是脏数据，调用方必须按失败处理（不装载）。
// load 与 stateLoadableErr 共用本函数，保证「能装上」与「判可用」同口径。
func readStateFile(fp string) (stateFile, error) {
	var sf stateFile
	raw, err := os.ReadFile(fp)
	if err != nil {
		return sf, err
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		return stateFile{}, fmt.Errorf("state.json 解析失败: %w", err)
	}
	return sf, nil
}

// load 从本地 state.json 装载账号状态，返回是否成功装载。New 忽略返回值（容错启动，
// 保持既有行为——损坏文件按空池启动，不 panic）。
func (p *Pool) load() bool {
	sf, err := readStateFile(p.stateFp)
	if err != nil {
		return false
	}
	p.applyAccountsLocked(sf.Accounts)
	return true
}

// stateLoadableErr 判定本地 state.json 能否真正装载：与 load 同口径的读+解析，但
// **只判定不 apply**——本地状态已在 New → load() 应用过，RestoreFromSnapshot 只需要
// 「本地可用性」这一个判据，避免重复装载把 Redis 快照的新鲜数据覆盖回旧本地值。
func stateLoadableErr(fp string) error {
	_, err := readStateFile(fp)
	return err
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	now := time.Now()
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		e := &entry{
			a:                        &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:                  s.Credits,
			creditsTotal:             s.CreditsTotal,
			creditsExpiring:          s.CreditsExpiring,
			creditsEarliestExpiry:    s.CreditsEarliestExpiry,
			creditsEarliestRemaining: s.CreditsEarliestRemaining,
			disabled:                 s.Disabled,
			paused:                   s.Paused,
			reason:                   s.Reason,
			until:                    s.Until,
			coolKind:                 s.CoolKind,
			successCount:             s.SuccessCount,
			errTotal:                 errTotal,
			lastErr:                  s.LastErr,
			lastSuccess:              s.LastSuccess,
			lastCheckinDay:           s.LastCheckinDay,
			tokenUsage:               s.TokenUsage,
			softStreak:               s.SoftStreak,
			sessionDeadFails:         s.SessionDeadFails,
			consecutiveFails:         s.ConsecutiveFails,
			weight:                   stateWeight(s.Weight),
			note:                     strings.TrimSpace(s.Note),
		}
		// 到期快照按当前时刻惰性清洗：已过期、零剩余或超出总余额的脏数据不恢复。
		if e.creditsExpiring < 0 {
			e.creditsExpiring = 0
		}
		if e.creditsExpiring > e.credits {
			e.creditsExpiring = e.credits
		}
		if e.creditsEarliestRemaining < 0 || e.creditsEarliestRemaining > e.credits {
			e.creditsEarliestRemaining = 0
		}
		if e.creditsEarliestRemaining == 0 || e.creditsEarliestExpiry.IsZero() || !now.Before(e.creditsEarliestExpiry) {
			e.creditsEarliestExpiry = time.Time{}
			e.creditsEarliestRemaining = 0
		}
		// 熔断器持久化恢复：breakerUntil 未过期才恢复（过期不复活），retryCount 仅在
		// 熔断仍有效时保留（否则归零，不保留无用退避指数）。
		if s.BreakerUntil != nil && now.Before(*s.BreakerUntil) {
			e.breakerUntil = *s.BreakerUntil
			e.retryCount = s.RetryCount
		}
		// 连败降权：未过期才恢复（过期/零值不写不复活）。
		if s.DegradeUntil != nil && now.Before(*s.DegradeUntil) {
			e.degradeUntil = *s.DegradeUntil
		}
		// 模型级独立冷却（6004 重置墙钟 / 11102 负缓存）：惰性过滤已过期条目。
		if len(s.ModelCooldowns) > 0 {
			for m, mc := range s.ModelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if e.modelCooldowns == nil {
					e.modelCooldowns = map[string]modelCooldown{}
				}
				e.modelCooldowns[m] = modelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// 成本账本：惰性过滤过期（modelCostTTL 外不恢复）+ 剔除结构破损条目
		// （负 per1k / 零 LastSeen——上游异常或旧文件手改产生的脏数据）。
		if len(s.ModelCosts) > 0 {
			for m, mc := range s.ModelCosts {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL || mc.CostPer1k < 0 {
					continue
				}
				if e.modelCost == nil {
					e.modelCost = map[string]modelCostEntry{}
				}
				e.modelCost[m] = modelCostEntry(mc)
			}
		}
		p.byUID[uid] = e
	}
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
// adoptSnapshot 采用 Redis 快照为当前池状态，并置 dirty 让下一次落盘把它物化回本地
// state.json（否则快照只在内存生效，下次崩溃恢复又回到旧本地文件）。
func (p *Pool) adoptSnapshot(s snapshot) {
	p.mu.Lock()
	p.applySnapshotLocked(s)
	p.mu.Unlock()
	p.dirty.Store(true)
}
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}

// persistDirty 消费 dirty 标志：有变更则采集快照并在**锁外**落盘。
// 落盘（MarshalIndent + WriteFile + fsync）耗时可观，绝不能在持 p.mu 时做——
// 那会把全池的选号/状态查询一起堵住。故流程是「锁内采集快照 → 解锁 → 锁外写盘」。
func (p *Pool) persistDirty() {
	p.mu.Lock()
	if !p.dirty.Swap(false) {
		p.mu.Unlock()
		return
	}
	sf, seq := p.snapshotLocked()
	p.mu.Unlock()
	p.saveSnapshot(sf, seq)
}

// snapshotLocked 采集一份带序号的落盘快照。调用方必须已持有 p.mu。
// 序号用于在锁外写入时丢弃「更旧的快照晚到」的乱序覆盖（见 saveSnapshot）。
func (p *Pool) snapshotLocked() (stateFile, uint64) {
	p.persistSeq++
	return p.stateOverviewLocked(), p.persistSeq
}

// saveSnapshot 序列化并原子落盘一份快照，再镜像一份到 Redis。**不得持锁调用**
// （内部只做短暂加锁读 store / 更新失败计数）。
func (p *Pool) saveSnapshot(sf stateFile, seq uint64) {
	if p.stateFp == "" {
		return
	}
	// 串行化落盘：并发调用（flusher 与 Remove/运维路径）不得同时写同一个临时文件。
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	if seq < p.persistWritten {
		// 乱序到达的旧快照（更晚采集的快照已落盘）：跳过，避免用旧状态覆盖新状态。
		return
	}
	// compact JSON：state.json 只供程序读取（排障用 jq 同样可读），每 5 秒
	// 一次的全量落盘没必要为缩进付出约 2 倍字节数与额外分配；470 账号实测
	// compact 比 MarshalIndent 少约一半 CPU/内存，磁盘写入也更短。
	raw, err := json.Marshal(sf)
	if err != nil {
		p.recordPersistFail(err)
		return
	}
	if err := writeStateAtomic(p.stateFp, raw); err != nil {
		p.recordPersistFail(err)
		return
	}
	p.persistWritten = seq
	p.mu.Lock()
	recovered := p.persistFails
	p.persistFails = 0
	store := p.store
	p.mu.Unlock()
	if recovered > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", recovered)
	}
	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	if store == nil {
		return
	}
	snapRaw, err := snapshotFromStateJSON(raw, time.Now())
	if err == nil {
		store.SaveState(snapRaw)
	}
}

// snapshotFromStateJSON 由已序列化的 state.json 原文构造 Redis 快照：
//
//	{"accounts":{...}} → {"accounts":{...},"saved_at":"..."}
//
// 旧实现先 MarshalIndent 写本地文件、再对同一 stateFile 做整份 Marshal 镜像 Redis，
// 同一份 470 账号状态被 JSON 编码两遍（实测 46 账号即多 ~56µs / ~48KB 分配）。
// 这里复用第一份字节、只补一个顶层 saved_at 字段；输出仍是合法 JSON，且字段
// （accounts 在前、saved_at 在后）与旧 snapshot 结构一致，RestoreFromSnapshot 的
// 解析路径无需改动。若 raw 不是预期的对象形态（理论上不会发生），回退到完整
// Marshal，绝不让镜像写出一份不可恢复的快照。
func snapshotFromStateJSON(raw []byte, savedAt time.Time) (json.RawMessage, error) {
	trimmed := compactJSONInto(raw, raw)
	if len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' {
		saved, err := json.Marshal(snapshotSavedAt{Time: savedAt})
		if err != nil {
			return nil, err
		}
		out := make([]byte, 0, len(trimmed)+1+len(saved))
		out = append(out, trimmed[:len(trimmed)-1]...)
		body := bytes.TrimSpace(trimmed[1 : len(trimmed)-1])
		if len(body) > 0 {
			out = append(out, ',')
		}
		out = append(out, saved[1:]...)
		return out, nil
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, err
	}
	return json.Marshal(snapshot{stateFile: sf, SavedAt: savedAt})
}

// compactJSON 去掉 JSON 字符串外的空白，输出紧凑 JSON。只做一遍线性扫描，
// 保留字符串内的空白（含转义引号），比 json.Compact 少一次 bytes.Buffer 的增长。
func compactJSON(raw []byte) []byte {
	return compactJSONInto(nil, raw)
}

// compactJSONInto 与 compactJSON 等价，但允许调用方提供可复用的 dst。dst 只在
// 需要独立缓冲时传 nil；若传入 raw 本身，则原地压缩（调用方必须不再使用 raw）。
// 快照路径的 raw 是本地写盘后即弃的副本，原地压缩可省掉一份 ~32KB 的临时缓冲。
func compactJSONInto(dst, raw []byte) []byte {
	out := dst[:0]
	if cap(out) < len(raw)/2 {
		out = make([]byte, 0, len(raw)/2)
	}
	inString, escaped := false, false
	for _, c := range raw {
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case ' ', '\n', '\r', '\t':
			// 结构空白：丢弃
		default:
			out = append(out, c)
		}
	}
	return out
}

// snapshotSavedAt 只用于给已编码的 state.json 追加 "saved_at" 字段。
type snapshotSavedAt struct {
	Time time.Time `json:"saved_at"`
}

// writeStateAtomic 原子落盘：临时文件写入 + fsync + rename + 目录 fsync。
// 目录 fsync 让 rename 本身也持久化（否则掉电后目录项可能未落，只剩旧文件或空目录项）。
// 临时文件名带 pid：多实例共用同一 state.json 路径时不会互相覆盖半成品。
func writeStateAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if dir != "" {
		// 目录 fsync 失败不视为落盘失败：部分文件系统/平台（含部分容器叠加层）不支持。
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}

// recordPersistFail 记录一次本地 state.json 落盘失败：
//  1. 按节流规则打日志——首败（状态成功→失败）打完整错误、每 persistLogEvery 次
//     连续失败打一条提醒、其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
//     恢复成功的日志由 saveSnapshot 在成功路径统一打。
//  2. **把 dirty 置回 true**：调用方已 Swap(false) 消费了脏标志，若不置回，本次失败
//     的变更在下一次内存变更之前再也不会被重试——进程重启即永久丢失。
//
// 与 redisstore 三处异步写的"失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是
// 盲区，故多一层节流（notification）。
func (p *Pool) recordPersistFail(err error) {
	p.mu.Lock()
	firstFail := p.persistFails == 0
	everyNth := p.persistFails%persistLogEvery == 0
	p.persistFails++
	fails := p.persistFails
	p.dirty.Store(true) // 失败重试：下轮 flusher 再试一次
	p.mu.Unlock()
	if firstFail {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if everyNth {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", fails, err)
	}
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。调用方必须已持 p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	now := time.Now()
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		s := stateAccount{
			Credits:                  e.credits,
			CreditsTotal:             e.creditsTotal,
			Disabled:                 e.disabled,
			Paused:                   e.paused,
			Reason:                   e.reason,
			Until:                    e.until,
			CoolKind:                 e.coolKind,
			SuccessCount:             e.successCount,
			ErrTotal:                 e.errTotal,
			LastSuccess:              e.lastSuccess,
			LastErr:                  e.lastErr,
			LastCheckinDay:           e.lastCheckinDay,
			TokenUsage:               e.tokenUsage,
			SoftStreak:               e.softStreak,
			SessionDeadFails:         e.sessionDeadFails,
			ConsecutiveFails:         e.consecutiveFails,
			CreditsExpiring:          e.creditsExpiring,
			CreditsEarliestExpiry:    e.creditsEarliestExpiry,
			CreditsEarliestRemaining: e.creditsEarliestRemaining,
			Weight:                   e.weight,
			Note:                     e.note,
		}
		// 熔断截止：仅未过期才落盘（指针 nil 才能被 omitempty 真省略）。
		if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
			u := e.breakerUntil
			s.BreakerUntil = &u
			s.RetryCount = e.retryCount
		}
		// 连败降权截止：仅未过期才落盘。
		if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
			u := e.degradeUntil
			s.DegradeUntil = &u
		}
		// 模型级独立冷却：惰性过滤已过期条目（Hits 不落盘，重启后 11102 退避从基数重学）。
		if len(e.modelCooldowns) > 0 {
			for m, mc := range e.modelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if s.ModelCooldowns == nil {
					s.ModelCooldowns = map[string]stateModelCooldown{}
				}
				s.ModelCooldowns[m] = stateModelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// 成本账本：惰性过滤过期观测（modelCostTTL 外不写——陈旧价格不复活）。
		if len(e.modelCost) > 0 {
			for m, mc := range e.modelCost {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
					continue
				}
				if s.ModelCosts == nil {
					s.ModelCosts = map[string]stateModelCost{}
				}
				s.ModelCosts[m] = stateModelCost(mc)
			}
		}
		sf.Accounts[uid] = s
	}
	return sf
}

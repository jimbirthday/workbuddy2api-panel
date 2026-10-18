package usage

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// DailyTokenUsage 是单个账号自本地零点以来的 token 用量。
// Models 的键为归一化后的裸模型名（与选号路径的 bareModel 对齐）。
//
// 类型别名而非独立定义：selected 路径直接返回 pool 可消费的只读快照，
// 装配层不再每请求整表复制一遍做类型转换。
type DailyTokenUsage = pool.DailyTokenUsage
type dailyTokenUsageByUID = pool.DailyTokenUsageByUID

// dailyUsageState 是 Recorder 的增量日额度快照。day 为本地自然日零点；
// usage 一旦发布即不可变（含每个 Models 表），读侧可无锁遍历。写入只更新
// 一个「待发布增量」表，读取时按 version 合并后整体发布；不能每笔 Add 都复制
// 整张账号表（470 账号约 41 KiB，会变成选号热路径上最大的固定成本）。
// 用独立锁保护指针，不和分片锁/rollupMu 形成锁序。
//
// 冷启动/跨零点时首次读取要扫全部分片。构建期间到达的 Add 不能只推进版本号：
// 若构建已经扫过对应分片，这笔增量会永久漏掉；若重扫到 gen 稳定，又会在持续
// 流量下饿死。这里在构建期间按分片暂存增量；每个分片扫描完、仍持读锁时清空
// 该分片已经包含在聚合结果里的 pending。扫描后新到的写入会进入新的 pending
// 表，发布时只需叠加剩余项。这样既不丢量、不重复计数，也不会把 pending 扩成
// 逐请求明细。
type dailyUsageState struct {
	mu    sync.Mutex
	cond  *sync.Cond
	day   time.Time
	usage dailyTokenUsageByUID
	ready bool
	// version 在已发布快照之后每次 Add 递增；pendingVersion 是已发布视图对应的版本。
	// 版本一致时读取可直接返回不可变视图；否则读取方只合并一次 delta 并发布新视图。
	// delta 是自上次发布以来的未读增量：只在 ready 且同一自然日时积累，并按账号/
	// 模型合并。它不随每次 Add 递归复制已合并的 overrides，因此大小与「自上次读取
	// 以来的账号/模型组合数」同阶，而不是请求数或历史桶数。
	version        uint64
	pendingVersion uint64
	delta          dailyTokenUsageByUID
	// view 是已发布的不可变合并视图。只在读取到未合并增量时整体重建一次；
	// 命中快路径直接返回它，避免每次 pick 都复制整张账号表（470 号约 42 KiB）。
	// viewVersion 是它对应的 pendingVersion；nil 表示尚未构建。
	view        dailyTokenUsageByUID
	viewVersion uint64
	// building 表示已有 goroutine 在聚合该自然日。其他调用者等待发布，不再
	// 重复全表扫描；epoch 由 resetDailyUsage 递增，用来废弃已经被重置的构建。
	// buildToken 标识当前构建者：reset 或后续构建可能已经取代它，旧构建返回时
	// 只能放弃自己的结果，绝不能清掉别人的 building 标记。
	// 同一时刻只允许一个构建者（即使请求的自然日不同），避免跨零点时两个构建者
	// 互相覆盖标记、同时扫描并发布互相矛盾的快照。
	building     time.Time
	buildToken   uint64
	nextBuildTok uint64
	epoch        uint64
	// pending 按「构建 epoch -> 自然日 -> 分片 -> 账号/模型」组织。分片扫描
	// 完成时整块删除，删除动作与扫描同处一个分片读锁临界区，因此不会和该分片
	// 的新 Add 交错；epoch 防止 reset 后旧构建误删新构建的增量。
	pending map[uint64]map[int64]map[int]map[dailyPendingKey]int64
}

// dailyPendingKey 在单个分片内把构建期间的新增按账号/模型合并，避免高频请求
// 把 pending 变成与请求数同阶的内存。
type dailyPendingKey struct {
	uid   string
	model string
}

// dailyShardScanHook 仅供测试注入扫描前后的确定性写入，生产环境保持 nil。
var dailyShardScanHook func(shard int, after bool)

// dailyBuildRejectHook 仅供测试观察旧构建者放弃所有权，生产环境保持 nil。
var dailyBuildRejectHook func()

// DailyTokenUsageAt 只读聚合自 now 所在本地日期零点起的账号/模型 token 用量。
// 它不修改 Recorder 语义，也不触发折叠或落盘；既有小时桶与日桶都按桶起点入窗。
//
// 返回 ok=false 仅表示记录器不可用（nil）；正常内存记录器即使无数据也返回空 map/true。
// 日额度按本地零点重置：以次日 now 再次调用时，前一天 23:00 的桶不会计入。
// 结果按「自然日 + 增量快照」缓存：pick 是热路径，不能每个请求都扫全部分片；
// 冷启动/跨零点只允许一个 goroutine 做全表聚合，其余调用者等待发布。
func (r *Recorder) DailyTokenUsageAt(now time.Time) (dailyTokenUsageByUID, bool) {
	if r == nil {
		return nil, false
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	r.daily.mu.Lock()
	defer r.daily.mu.Unlock()
	if r.daily.cond == nil {
		r.daily.cond = sync.NewCond(&r.daily.mu)
	}
	for {
		if r.daily.ready && r.daily.day.Equal(start) {
			if r.daily.pendingVersion != r.daily.version {
				r.publishDailyDeltaLocked()
			}
			// view 在发布后不再修改；命中快路径只返回它，调用方只读。
			return r.daily.view, true
		}
		// 已有构建者：等它完成。构建期间到达的 Add 已记入 pending，不会丢；
		// 等待者不重复扫全表。即便请求的是另一个自然日（跨零点）也等待，
		// 保证 building/buildToken 始终只有一个所有者。
		if !r.daily.building.IsZero() {
			r.daily.cond.Wait()
			continue
		}
		r.daily.building = start
		r.daily.nextBuildTok++
		token := r.daily.nextBuildTok
		r.daily.buildToken = token
		epoch := r.daily.epoch
		r.daily.mu.Unlock()

		out := r.aggregateDaily(start, start.AddDate(0, 0, 1), epoch)

		r.daily.mu.Lock()
		if r.daily.epoch != epoch || r.daily.buildToken != token {
			// 测试/load 在构建期间重置了桶表，或本次构建已被新构建者取代：
			// 结果可能来自旧桶，不能发布；也不能清掉现在所有者的标记。
			if dailyBuildRejectHook != nil {
				dailyBuildRejectHook()
			}
			r.daily.cond.Broadcast()
			continue
		}
		if r.daily.ready && r.daily.day.Equal(start) {
			r.daily.building = time.Time{}
			r.daily.buildToken = 0
			r.daily.cond.Broadcast()
			return r.daily.usage, true
		}
		// 合并构建期间新增、但发生在对应分片扫描之后的增量。扫描时已经清掉
		// 该分片此前暂存的 pending，因此这里剩余项都尚未进入 out。
		key := start.Unix()
		for _, byShard := range r.daily.pending[epoch][key] {
			for pk, tokens := range byShard {
				u := out[pk.uid]
				if u.Models == nil {
					u.Models = make(map[string]int64)
				}
				u.Tokens += tokens
				u.Models[pk.model] += tokens
				out[pk.uid] = u
			}
		}
		delete(r.daily.pending, epoch)
		r.daily.day = start
		r.daily.usage = out
		r.daily.ready = true
		// 发布基础快照时同时清空旧增量：增量只属于上一份快照，已全部并入 out，
		// 不能留到下一份快照上重复叠加。版本推进一次即可，供后续 reader 判断
		// 是否需要合成视图。
		r.daily.version++
		r.daily.pendingVersion = r.daily.version
		r.daily.delta = nil
		r.daily.view = out
		r.daily.viewVersion = r.daily.pendingVersion
		r.daily.building = time.Time{}
		r.daily.buildToken = 0
		r.daily.cond.Broadcast()
		return out, true
	}
}

// HasRecordedUsage 报告记录器是否已经承载过任何用量桶（任意日期）。
//
// 它只做每个分片的 len(buckets) 检查，不遍历桶内容，供面板区分：
//   - 今日确实为 0，但历史桶已加载/已写入（快照可信，剩余=限额）；
//   - 全新进程尚未 load 到任何数据（无快照，不能把 0 当成可用额度）。
//
// DailyTokenUsageAt 对正常内存记录器即使无数据也返回 ok=true；因此调用方
// 不能只用 ok 判断“有快照”。本方法不改变锁序、缓存或聚合语义。
func (r *Recorder) HasRecordedUsage() bool {
	if r == nil {
		return false
	}
	for i := range r.shards {
		r.shards[i].mu.RLock()
		n := len(r.shards[i].buckets)
		r.shards[i].mu.RUnlock()
		if n > 0 {
			return true
		}
	}
	return false
}

// addDailyUsage 把一次已计入小时桶的 token 增量同步到日额度快照。调用方持有
// 对应分片锁；本函数只取 daily.mu，锁序固定为 shard -> daily，不能反向。
//
// 快照尚未建立且没有构建者时不暂存：Add 已经写入桶表，后续首次聚合会包含它。
// 只有构建已经开始时才记入 pending，供扫描后的增量合并；这样启动阶段不会被
// 恢复的几十万桶拖慢，也不会在长期无读取时积累逐请求明细。
func (r *Recorder) addDailyUsage(now time.Time, uid, model string, tokens int64, shard int) {
	if tokens == 0 {
		return
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	model = canonicalUsageModel(model)
	r.daily.mu.Lock()
	defer r.daily.mu.Unlock()
	if r.daily.ready && r.daily.day.Equal(start) {
		// 热路径只记录增量，绝不复制已发布快照：DailyTokenUsageAt 会在下一次
		// 读取时按 version 合并并发布新的不可变快照。delta 按账号/模型合并，
		// 内存与「账号 × 模型」同阶；同一模型连续写入只做一次 map 累加。
		if r.daily.delta == nil {
			r.daily.delta = make(dailyTokenUsageByUID)
		}
		u := r.daily.delta[uid]
		if u.Models == nil {
			u.Models = make(map[string]int64, 1)
		}
		u.Tokens += tokens
		u.Models[model] += tokens
		r.daily.delta[uid] = u
		r.daily.version++
		return
	}
	// 只有正在构建的这一天需要暂存。未开始构建时，Add 已经写入桶表，后续
	// aggregateDaily 会按分片版本看到它；此时暂存反而会无限增长。
	if !r.daily.building.Equal(start) {
		return
	}
	if r.daily.pending == nil {
		r.daily.pending = make(map[uint64]map[int64]map[int]map[dailyPendingKey]int64)
	}
	epoch := r.daily.epoch
	if r.daily.pending[epoch] == nil {
		r.daily.pending[epoch] = make(map[int64]map[int]map[dailyPendingKey]int64)
	}
	key := start.Unix()
	if r.daily.pending[epoch][key] == nil {
		r.daily.pending[epoch][key] = make(map[int]map[dailyPendingKey]int64)
	}
	if r.daily.pending[epoch][key][shard] == nil {
		r.daily.pending[epoch][key][shard] = make(map[dailyPendingKey]int64)
	}
	r.daily.pending[epoch][key][shard][dailyPendingKey{uid: uid, model: model}] += tokens
}

// publishDailyDeltaLocked 把自上次发布以来的 delta 合并成新的不可变视图。
// 只在该次读取时复制一次基础表；后续 Add 只累加 delta，不再逐次复制。
func (r *Recorder) publishDailyDeltaLocked() {
	if len(r.daily.delta) == 0 {
		r.daily.pendingVersion = r.daily.version
		if r.daily.view == nil {
			r.daily.view = r.daily.usage
			r.daily.viewVersion = r.daily.pendingVersion
		}
		return
	}
	base := r.daily.view
	if base == nil {
		base = r.daily.usage
	}
	// 顶层 map 只复制一次，但已发布账号的 Models 子表仍共享（视为不可变）。
	// 旧实现为每个账号重新复制 Models，即使该账号本批没有新增；470 账号 ×
	// 多模型时，任意一次 Add 后的首次读取都会分配整张模型表。
	daily := make(dailyTokenUsageByUID, len(base)+len(r.daily.delta))
	for uid, u := range base {
		daily[uid] = u
	}
	for uid, d := range r.daily.delta {
		u := daily[uid]
		models := make(map[string]int64, len(u.Models)+len(d.Models))
		for m, v := range u.Models {
			models[m] = v
		}
		u.Tokens += d.Tokens
		for m, v := range d.Models {
			models[m] += v
		}
		u.Models = models
		daily[uid] = u
	}
	r.daily.view = daily
	r.daily.viewVersion = r.daily.version
	r.daily.delta = nil
	r.daily.pendingVersion = r.daily.version
}

// resetDailyUsage 清空增量快照（load/测试写入桶后调用），下一次读取重建。
func (r *Recorder) resetDailyUsage() {
	r.daily.mu.Lock()
	r.daily.day = time.Time{}
	r.daily.usage = nil
	r.daily.ready = false
	r.daily.building = time.Time{}
	r.daily.buildToken = 0
	r.daily.version = 0
	r.daily.pendingVersion = 0
	r.daily.delta = nil
	r.daily.view = nil
	r.daily.viewVersion = 0
	r.daily.epoch++
	r.daily.pending = nil
	if r.daily.cond != nil {
		r.daily.cond.Broadcast()
	}
	r.daily.mu.Unlock()
}

// aggregateDaily 聚合 [start, end) 内的桶。
func (r *Recorder) aggregateDaily(start, end time.Time, epoch uint64) map[string]DailyTokenUsage {
	out := make(map[string]DailyTokenUsage)
	r.rollupMu.RLock()
	defer r.rollupMu.RUnlock()
	for i := range r.shards {
		if dailyShardScanHook != nil {
			dailyShardScanHook(i, false)
		}
		sh := &r.shards[i]
		sh.mu.RLock()
		for _, b := range sh.buckets {
			if b.TT <= 0 || b.UID == "" {
				continue
			}
			ts, ok := bucketTime(b.Scope)
			if !ok || ts.Before(start) || !ts.Before(end) {
				continue
			}
			u := out[b.UID]
			if u.Models == nil {
				u.Models = make(map[string]int64)
			}
			u.Tokens += b.TT
			model := canonicalUsageModel(b.Model)
			u.Models[model] += b.TT
			out[b.UID] = u
		}
		// 本分片已经复制到 out，构建期间暂存在这里、且发生在复制前的增量
		// 已包含在桶中，可以整块丢弃。保持分片读锁使这个清理与该分片的新
		// Add 互斥：Add 要么在清理前完成（已被遍历），要么在其后暂存。
		r.daily.mu.Lock()
		delete(r.daily.pending[epoch][start.Unix()], i)
		r.daily.mu.Unlock()
		sh.mu.RUnlock()
		if dailyShardScanHook != nil {
			dailyShardScanHook(i, true)
		}
	}
	return out
}

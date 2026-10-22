// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model, rate) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// flushMu 串行化落盘。快照复制只持 rollupMu 读锁的短窗口；JSON 序列化、
// fsync、rename 都在锁外完成，避免面板快照与日额度聚合被磁盘 I/O 阻塞。
var usageFlushMu sync.Mutex

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// fileVersion 是 usage.json 的当前格式版本。版本 2 增加积分观测字段，版本 3
// 增加模型生效倍率分区，版本 4 增加前缀缓存命中/未命中累计，版本 5 增加
// API Key 归属（key_id，身份标签而非密钥）；
// 旧版本缺失字段按零值加载，旧数据不会丢弃。
const fileVersion = 5

// bucket 一个 (时间片, realm, uid, model, rate, key_id) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string  `json:"s"`            // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string  `json:"r"`            // cn / global
	UID   string  `json:"u"`            // 账号 uid
	Model string  `json:"m"`            // 上游裸模型名
	Rate  string  `json:"x,omitempty"`  // 请求时生效积分倍率（规范化数值；旧桶为空）
	Req   int64   `json:"q"`            // 请求数（含失败）
	Err   int64   `json:"e"`            // 失败数
	PT    int64   `json:"p"`            // prompt tokens
	CT    int64   `json:"c"`            // completion tokens
	TT    int64   `json:"t"`            // total tokens（上游给什么用什么的合计）
	LatMs int64   `json:"l"`            // 延迟累计（ms）
	LatN  int64   `json:"ln"`           // 延迟样本数
	TPS   float64 `json:"v"`            // 吐字速率累计
	TPSN  int64   `json:"vn"`           // 速率样本数
	CR    float64 `json:"cr,omitempty"` // usage.credit 累计（仅明确存在的观测）
	CRN   int64   `json:"cn,omitempty"` // usage.credit 样本数（区分缺字段与真实 0）
	CRT   int64   `json:"ct,omitempty"` // 同时具备 credit 与 token 的 Token 合计
	CH    int64   `json:"ch,omitempty"` // 前缀缓存命中 token 累计（上游回该维度才累计）
	CM    int64   `json:"cm,omitempty"` // 前缀缓存未命中 token 累计
	// KeyID 命中的客户端 API Key 身份 id（不是密钥）。旧桶为空 = 未归属；
	// 空串不能与 legacy 合并，否则历史流量会被误记到某个具体客户端名下。
	KeyID string `json:"k,omitempty"`
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// usageShards 是桶表的分片数。快照轮询与请求记账不能争用同一把全局锁：
// 47k 桶的一次窗口聚合若持全局读锁，Go RWMutex 会优先放行等待中的写锁，
// 使面板轮询期间每个 Add 都排在快照后面。分片后每个分片独立加锁，快照只
// 依次短暂读各分片，记账仍可并行进入其它分片。
const usageShards = 64

// usageShard 是一组按 key 分区的桶。mu 同时保护 buckets、dirty 与 version。
type usageShard struct {
	mu      sync.RWMutex
	buckets map[string]*bucket // key: scope|realm|uid|model|rate
	dirty   bool
	// version 在每次桶表变更时递增。落盘复制完某个分片后，只有版本仍与复制时
	// 相同才能清 dirty；否则说明复制后又有 Add，必须留给下一轮落盘。
	version uint64
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	shards [usageShards]usageShard
	// tableVersion 是桶表的全局单调版本：任意 Add/Rollup/load 变更桶表都会
	// 推进一次。缓存命中只需读这一个 atomic，不必逐分片求和；逐分片求和既
	// 有 64 次锁开销，又可能因两次求和的读序不同而让版本变化相互抵消，造成
	// 「Add 后仍命中旧快照」的正确性缺陷。
	tableVersion atomic.Uint64
	// rollupMu 是桶表的跨分片一致性屏障：快照与落盘持读/写锁，Rollup 持写锁
	// 迁移桶。否则 Rollup 在两个分片之间移动时，快照可能看到「源已删、目标
	// 未拷」的中间态；并发 Save 还可能把缺失桶的快照 rename 到最终文件。
	// Add 不经过此锁，只取单个分片锁，请求热路径不受影响。
	rollupMu sync.RWMutex
	path     string
	started  time.Time

	// daily 是选号日额度快照（见 DailyTokenUsageAt）。与 rollupMu/shard.mu
	// 分离：读取快照不应等待落盘写锁；Add 在分片锁内增量更新它。
	daily dailyUsageState

	// snapCache 缓存面板窗口聚合。470 账号、数万桶时一次 SnapshotWindow 约
	// 8ms；浏览器轮询/切维度/导出会把同样窗口反复算一遍。缓存前缀是「所有
	// 全局桶表版本 + 滚动窗口锚点 + 昵称映射」，任意 Add/Rollup/load
	// 都会让缓存失效。聚合阶段持 rollupMu 读锁，因此缓存发布与版本采样都在
	// 同一屏障内完成，不会出现「旧数据配新版本」。
	snapMu    sync.Mutex
	snapCache map[string]snapshotCacheEntry
	// snapFlightMu/snapFlights 按窗口键做单飞。旧实现用一把全局锁，
	// 24 小时或全部历史聚合会阻塞小时窗口、面板刷新与导出，形成尾延迟。
	// 同键请求仍只跑一次聚合，不同窗口可以并行。
	snapFlightMu sync.Mutex
	snapFlights  map[string]*snapshotFlight

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

const snapshotCacheTTL = 2 * time.Second

type snapshotCacheEntry struct {
	snapshot Snapshot
	at       time.Time
	key      string
	// rateRevision 是构建时倍率目录的版本。0 表示本次快照未使用倍率回填；
	// 非零时仅允许同版本命中，目录刷新后自动失效。
	rateRevision uint64
}

// snapshotFlight 是一次进行中的窗口聚合。done 关闭表示 leader 已完成
// （无论是否成功发布缓存）；等待者随后重新检查缓存，未命中则自己接任。
type snapshotFlight struct {
	done chan struct{}
}

// shardIndex 用 FNV-1a 选择桶所属分片。热点键通常只差账号/模型，散列后
// 可让并发请求落到不同分片，避免所有 Add 串行在同一把锁上。
func shardIndex(key string) int {
	h := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % usageShards)
}

func (r *Recorder) shardFor(key string) *usageShard {
	return &r.shards[shardIndex(key)]
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{path: path, started: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	for i := range r.shards {
		r.shards[i].buckets = make(map[string]*bucket)
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				n := 0
				for i := range r.shards {
					r.shards[i].mu.RLock()
					n += len(r.shards[i].buckets)
					r.shards[i].mu.RUnlock()
				}
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	Credit           float64
	HasCredit        bool
	ModelRate        string
	// CacheHitTokens / CacheMissTokens 前缀缓存命中/未命中观测（issue #92）。
	HasCacheTokens  bool
	CacheHitTokens  int64
	CacheMissTokens int64
	LatencyMs       int64
	HasLatency      bool
	TokensPerSecond float64
	HasTPS          bool
	// KeyID 命中的客户端 API Key 身份 id（不是密钥原文）。空 = 未鉴权/历史
	// 旧调用方；聚合时进入独立的“(未归属)”桶，不伪造调用者。
	KeyID string
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	// KeyID 进入桶键：同一账号/模型下不同客户端的用量必须能分开统计，否则
	// 「按 Key 分析」只能看到合并后的总数。身份 id 是短 ASCII，额外一维的
	// 桶数增量与启用多 Key 的客户端数量成正比。
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + d.ModelRate + "|" + d.KeyID

	sh := r.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	shard := shardIndex(key)

	// 允许 var Recorder 零值直接使用（历史实现靠 map 懒写入）：未初始化时
	// 在分片锁内按需建桶表，避免 nil map 写入 panic。
	if sh.buckets == nil {
		sh.buckets = make(map[string]*bucket)
	}
	b := sh.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Rate: d.ModelRate, KeyID: d.KeyID}
		sh.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasCredit {
		b.CR += d.Credit
		b.CRN++
		// 比例只使用同一次请求同时具备 credit 与 token 的样本，避免把
		// 仅 token 的旧记录或仅 credit 的观测混进分母。
		if d.HasTotal {
			b.CRT += d.TotalTokens
		} else if d.HasPromptTokens || d.HasCompletion {
			b.CRT += d.PromptTokens + d.CompletionTokens
		}
	}
	if d.HasCacheTokens {
		b.CH += d.CacheHitTokens
		b.CM += d.CacheMissTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	sh.dirty = true
	sh.version++
	r.tableVersion.Add(1)
	// 选号日额度直接吃这次 token 增量：请求完成后下一次 pick 必须看到最新
	// 计数，同时不能为此重扫全部分片。
	tokens := int64(0)
	if d.HasTotal {
		tokens = d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		tokens = d.PromptTokens + d.CompletionTokens
	}
	r.addDailyUsage(now, uid, model, tokens, shard)
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	r.rollupMu.Lock()
	defer r.rollupMu.Unlock()
	for i := range r.shards {
		if r.shards[i].buckets == nil {
			r.shards[i].buckets = make(map[string]*bucket)
		}
	}
	cutoff := now.Add(-hourlyKeep)
	type move struct{ from, to string }
	totalMoves := 0
	for i := range r.shards {
		var moves []move
		srcShard := &r.shards[i]
		// 直接持写锁收集并迁移：候选与删除必须原子完成。若先 RLock 收集再
		// 放锁迁移，期间并发 Add 可能在同 key 上写入，导致新样本被误删或
		// 与移动后的日桶重复计数。
		srcShard.mu.Lock()
		for k, b := range srcShard.buckets {
			if !strings.HasPrefix(b.Scope, "h:") {
				continue
			}
			ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
			if err != nil || !ts.Before(cutoff) {
				continue
			}
			day := "d:" + ts.Format(dayLayout)
			moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Rate + "|" + b.KeyID})
		}
		for _, m := range moves {
			dstIdx := shardIndex(m.to)
			dstShard := &r.shards[dstIdx]
			// rollupMu 已串行化所有折叠；源分片保持写锁时再取目标分片不会
			// 与另一个 Rollup 形成反向等待。Add 只取单分片锁，也不会反序。
			if dstShard != srcShard {
				dstShard.mu.Lock()
			}
			src := srcShard.buckets[m.from]
			if src != nil {
				dst := dstShard.buckets[m.to]
				if dst == nil {
					cp := *src
					cp.Scope = strings.SplitN(m.to, "|", 2)[0]
					dst = &cp
					dstShard.buckets[m.to] = dst
				} else {
					dst.Req += src.Req
					dst.Err += src.Err
					dst.PT += src.PT
					dst.CT += src.CT
					dst.TT += src.TT
					dst.LatMs += src.LatMs
					dst.LatN += src.LatN
					dst.TPS += src.TPS
					dst.TPSN += src.TPSN
					dst.CR += src.CR
					dst.CRN += src.CRN
					dst.CRT += src.CRT
					dst.CH += src.CH
					dst.CM += src.CM
				}
				delete(srcShard.buckets, m.from)
				srcShard.dirty = true
				dstShard.dirty = true
				srcShard.version++
				if dstShard != srcShard {
					dstShard.version++
				}
				r.tableVersion.Add(1)
				totalMoves++
			}
			if dstShard != srcShard {
				dstShard.mu.Unlock()
			}
		}
		srcShard.mu.Unlock()
	}
	if totalMoves > 0 {
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", totalMoves, hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		key := b.Scope + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Rate + "|" + b.KeyID
		r.shardFor(key).buckets[key] = &b
	}
	// 恢复的桶不是通过 Add 写入的：推进版本让任何已发布的缓存失效，并清掉
	// 可能已建立的日额度增量快照后再读一次全表。
	r.tableVersion.Add(1)
	r.resetDailyUsage()
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(f.Buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	usageFlushMu.Lock()
	defer usageFlushMu.Unlock()

	snap := file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339)}
	anyDirty, copied := r.copyForFlush(&snap, force)
	if !anyDirty {
		return
	}

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	// 唯一临时名（同目录同盘，保证 rename 原子）：面板触发的 Save 与 30s 定时
	// flusher 可能并发进来，固定 ".tmp" 会让两者互相截断/踩 rename 竞态。
	// CreateTemp 默认 0600，与原 os.WriteFile 权限一致。
	f, err := os.CreateTemp(filepath.Dir(r.path), filepath.Base(r.path)+".*.tmp")
	if err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		log.Printf("[usage] 临时文件落盘失败: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		log.Printf("[usage] 关闭临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		os.Remove(tmp)
		log.Printf("[usage] 原子替换失败: %v", err)
		return
	}
	// 只有整份快照成功 rename 才清脏。清理时核对复制时的 version：复制之后
	// 若又有 Add，dirty 必须保留，否则这笔新用量要等到下一次有变更才可能落盘。
	r.clearFlushed(copied)
}

// flushShardCopy 记录一个分片被复制时的版本号，供落盘成功后安全清脏。
type flushShardCopy struct {
	index   int
	version uint64
}

// copyForFlush 在跨分片一致性屏障下复制全部桶。复制只持分片读锁；dirty 仅在
// 写盘成功且分片版本未变化时清除。返回 anyDirty 表示是否应执行写盘。
func (r *Recorder) copyForFlush(snap *file, force bool) (bool, []flushShardCopy) {
	r.rollupMu.RLock()
	defer r.rollupMu.RUnlock()
	// 先只探测 dirty：定时 flusher 每次都会调用这里，长期没有新请求时
	// 不应把全部桶复制一遍再发现无事可做。强制 Save 仍走完整复制。
	if !force {
		anyDirty := false
		for i := range r.shards {
			sh := &r.shards[i]
			sh.mu.RLock()
			dirty := sh.dirty
			sh.mu.RUnlock()
			if dirty {
				anyDirty = true
				break
			}
		}
		if !anyDirty {
			return false, nil
		}
	}
	copied := make([]flushShardCopy, 0, len(r.shards))
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.RLock()
		// 落盘用整体 rename 替换文件：快照必须包含全部分片，不能只拼 dirty
		// 分片，否则一次定时落盘就会在磁盘上抹掉干净分片。dirty 只决定要不要写。
		for _, b := range sh.buckets {
			snap.Buckets = append(snap.Buckets, *b)
		}
		copied = append(copied, flushShardCopy{index: i, version: sh.version})
		sh.mu.RUnlock()
	}
	return true, copied
}

// clearFlushed 清掉本次成功落盘、且复制后未再变更的分片的 dirty。成功写入
// 之后的新 Add 会推进 version；这里不能无条件清 dirty，否则那一笔会丢写。
func (r *Recorder) clearFlushed(copied []flushShardCopy) {
	r.rollupMu.RLock()
	defer r.rollupMu.RUnlock()
	for _, c := range copied {
		sh := &r.shards[c.index]
		sh.mu.Lock()
		if sh.version == c.version {
			sh.dirty = false
		}
		sh.mu.Unlock()
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests           int64   `json:"requests"`
	Errors             int64   `json:"errors"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTok      int64   `json:"completion_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens     int64   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens    int64   `json:"cache_miss_tokens,omitempty"`
	CacheHitRate       float64 `json:"cache_hit_rate,omitempty"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	AvgTPS             float64 `json:"avg_tokens_per_second"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.Credits += b.CR
	g.CreditSamples += b.CRN
	g.CreditTokens += b.CRT
	g.CacheHitTokens += b.CH
	g.CacheMissTokens += b.CM
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	if g.CreditTokens > 0 {
		a.CreditsPer1MTokens = g.Credits / float64(g.CreditTokens) * 1_000_000
	}
	if total := g.CacheHitTokens + g.CacheMissTokens; total > 0 {
		a.CacheHitRate = float64(g.CacheHitTokens) / float64(total) * 100
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// CreditAgg 积分扣除统计的一行。Key 在账号维度是 UID，在模型维度是裸模型名；
// Rate 仅模型维度使用；比例分母只统计与 credit 同时存在的 Token 样本。
type CreditAgg struct {
	Key      string `json:"key"`
	Realm    string `json:"realm,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Rate     string `json:"rate,omitempty"`
	Requests int64  `json:"requests"`
	// CreditRequests 是真正带 usage.credit 观测的请求数（桶的 CRN 之和）。
	// Requests 含「该账号/模型下没有 credit 观测」的请求，两者相减即覆盖率；
	// 直接拿 Requests 当分母会把「无观测」误算成「免费」。
	CreditRequests     int64   `json:"credit_requests"`
	Credits            float64 `json:"credits"`
	CreditSamples      int64   `json:"credit_samples"`
	CreditTokens       int64   `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	CacheHitTokens     int64   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens    int64   `json:"cache_miss_tokens,omitempty"`
	CacheHitRate       float64 `json:"cache_hit_rate,omitempty"`
}

type creditAcc struct {
	CreditAgg
}

func (a *creditAcc) add(b *bucket) {
	a.Requests += b.Req
	a.CreditRequests += b.CRN
	a.Credits += b.CR
	a.CreditSamples += b.CRN
	a.CreditTokens += b.CRT
	a.CacheHitTokens += b.CH
	a.CacheMissTokens += b.CM
}

func (a *creditAcc) finish() CreditAgg {
	out := a.CreditAgg
	if a.CreditTokens > 0 {
		out.CreditsPer1MTokens = a.Credits / float64(a.CreditTokens) * 1_000_000
	}
	if total := a.CacheHitTokens + a.CacheMissTokens; total > 0 {
		out.CacheHitRate = float64(a.CacheHitTokens) / float64(total) * 100
	}
	return out
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals    Agg        `json:"totals"`
	ByRealm   []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel   []KeyedAgg `json:"by_model"`
	// ByKey 按客户端 API Key 身份 id 聚合（key_id，不是密钥原文）。空 key_id
	// 的历史流量进入 Key="" 的单独一行，由前端显示为「未归属」——不能把它
	// 混入某个具体客户端，否则按 Key 计费/排障会归错人。
	ByKey           []KeyedAgg  `json:"by_key"`
	Series          []Point     `json:"series"`
	CreditByAccount []CreditAgg `json:"credit_by_account"`
	CreditByModel   []CreditAgg `json:"credit_by_model"`
	CreditByKey     []CreditAgg `json:"credit_by_key"`
	// CreditByRealm 按域（cn/global）聚合积分扣减。前端「积分」明细的域维度
	// 与用量明细的 by_realm 对齐，否则按域查看消耗时只能看到 token 没有积分。
	CreditByRealm []CreditAgg `json:"credit_by_realm"`
	Buckets       int         `json:"buckets"`
	FileBytes     int64       `json:"file_bytes"`
	Since         string      `json:"since,omitempty"`
	// WindowFrom/WindowTo 本次实际生效的统计区间（本地时间，RFC3339），供面板
	// 回显口径——「自定义」区间下用户必须能确认服务端到底按哪段算的。
	// 空串 = 该侧不设界（全部历史 / 到今天为止）。
	WindowFrom string `json:"window_from,omitempty"`
	WindowTo   string `json:"window_to,omitempty"`
	Generated  string `json:"generated"`
	// version 是构建这份快照时的全局桶表版本，仅用于缓存失效（不序列化）。
	version uint64
}

// Window 用量统计窗口。三种口径按优先级解析（见 bounds）：
//   - From/To 任一非零 → 显式区间 [From, To]（To 零值 = 不设上界）
//   - 否则 Hours>0     → 滚动窗口：当前整点往回 Hours-1 小时
//   - 否则             → 全部历史
//
// 为什么显式区间用「桶起点落在 [From, To] 内」判定而不是求交集：小时桶的粒度
// 就是一小时，用户选到 14:00 时把 14:00 这一小时的桶算进来符合直觉；同时这也
// 让 Hours 口径与历史行为逐位一致（原实现就是 ts.Before(from) 即跳过）。
type Window struct {
	Hours int
	From  time.Time
	To    time.Time
}

// bounds 解析出实际生效的 [from, to]；零值表示该侧不设界。
func (w Window) bounds() (time.Time, time.Time) {
	if !w.From.IsZero() || !w.To.IsZero() {
		return w.From, w.To
	}
	if w.Hours <= 0 {
		return time.Time{}, time.Time{}
	}
	h := w.Hours
	if h > 24*60 {
		h = 24 * 60
	}
	return time.Now().Truncate(time.Hour).Add(-time.Duration(h-1) * time.Hour), time.Time{}
}

// bucketTimeAt 缓存一次 scope 的时间解析结果（ok=false 表示脏 scope）。
type bucketTimeAt struct {
	ts time.Time
	ok bool
}

// bucketTime 把桶 scope 解析成本地时间；脏 scope 返回 false（不进任何口径）。
func bucketTime(scope string) (time.Time, bool) {
	if strings.HasPrefix(scope, "h:") {
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), time.Local)
		return ts, err == nil
	}
	ts, err := time.ParseInLocation(dayLayout, strings.TrimPrefix(scope, "d:"), time.Local)
	return ts, err == nil
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// hours>0：窗口 = [当前整点-(hours-1)小时, now]，卡片汇总/按域/按账号/按模型/
// 时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化（曾长期是"卡片为
// 全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没生效"，已废弃）。
// 小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，故小时窗口
// 天然不含更早的日桶。
// hours<=0：全部历史（含已折叠日桶），供「全部历史」选项看长期趋势。
//
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	return r.SnapshotWithRates(hours, nicks, nil)
}

// SnapshotWithRates 与 Snapshot 相同，但允许为缺少历史倍率的旧桶提供当前
// 模型倍率回填。currentRate 返回空串时该行按“未知倍率”聚合，不伪造价格。
func (r *Recorder) SnapshotWithRates(hours int, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	return r.SnapshotWindow(Window{Hours: hours}, nicks, currentRate)
}

// SnapshotWindow 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// 窗口语义见 Window：滚动窗口（Hours）/ 显式区间（From-To）/ 全部历史。卡片汇总、
// 按域、按账号、按模型、时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化
// （曾长期是"卡片为全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没
// 生效"，已废弃）。小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，
// 故小时窗口天然不含更早的日桶。
func (r *Recorder) SnapshotWindow(w Window, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	return r.SnapshotWindowRates(w, nicks, currentRate, 0)
}

// SnapshotWindowRates 与 SnapshotWindow 相同，并接受倍率目录版本。
//
// 带 currentRate 的聚合结果只依赖「桶表版本 + 窗口 + 昵称 + 倍率版本」：
// 目录未刷新时同窗口查询可命中 2 秒缓存，避免面板轮询/导出反复扫描数万桶。
// rateRevision=0 表示调用方未提供可判定版本，按旧的保守语义不缓存。
func (r *Recorder) SnapshotWindowRates(w Window, nicks map[string]string, currentRate func(realm, model string) string, rateRevision uint64) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	cacheable := currentRate == nil || rateRevision != 0
	windowKey := r.snapshotWindowKey(w, nicks)
	if !cacheable {
		return r.buildSnapshotWindow(w, nicks, currentRate, windowKey, rateRevision, false)
	}
	flightKey := windowKey + "\x00r" + strconv.FormatUint(rateRevision, 10)
	for {
		if snap, ok := r.cachedSnapshotRates(windowKey, rateRevision); ok {
			return snap
		}
		done, release := r.beginSnapshotFlight(flightKey)
		if release == nil {
			<-done
			continue
		}
		var out Snapshot
		func() {
			defer release()
			// 成为 leader 前可能已有同键请求完成并发布，再查一次避免重复聚合。
			if snap, ok := r.cachedSnapshotRates(windowKey, rateRevision); ok {
				out = snap
				return
			}
			out = r.buildSnapshotWindow(w, nicks, currentRate, windowKey, rateRevision, true)
		}()
		return out
	}
}

// beginSnapshotFlight 尝试成为某窗口的聚合 leader。release 非 nil 表示调用方
// 成为 leader，必须调用它结束本轮；否则 done 会在 leader 结束时关闭。
func (r *Recorder) beginSnapshotFlight(key string) (done <-chan struct{}, release func()) {
	r.snapFlightMu.Lock()
	defer r.snapFlightMu.Unlock()
	if r.snapFlights == nil {
		r.snapFlights = make(map[string]*snapshotFlight)
	}
	if f, ok := r.snapFlights[key]; ok {
		return f.done, nil
	}
	f := &snapshotFlight{done: make(chan struct{})}
	r.snapFlights[key] = f
	return f.done, func() {
		r.snapFlightMu.Lock()
		delete(r.snapFlights, key)
		close(f.done)
		r.snapFlightMu.Unlock()
	}
}

// buildSnapshotWindow 执行一次窗口聚合。publish=true 时在桶表版本稳定时
// 发布缓存；版本在聚合期间变化则照常返回本次结果，但不发布（下一次读取
// 会重新聚合，保证 Add 后立即可见）。
func (r *Recorder) buildSnapshotWindow(w Window, nicks map[string]string, currentRate func(realm, model string) string, windowKey string, rateRevision uint64, publish bool) Snapshot {
	// 采样版本必须在聚合前：若聚合期间有 Add/Rollup，构建出的结果可能是
	// 半个事务的视图，不能贴上构建后的新版本号缓存（否则下一次读取会命中
	// 少计的汇总）。版本变化时本次照常返回，但不发布。
	startVersion := r.tableVersionNow()
	snap := r.snapshotWindow(w, nicks, currentRate)
	endVersion := r.tableVersionNow()
	if publish && endVersion == startVersion {
		r.publishSnapshotRates(windowKey, snap, endVersion, rateRevision)
	} else {
		// 本次未发布：把版本带回调用方（仅测试/观测使用），下一次稳定读取会
		// 正常发布。不能让 version 停留在 0 被误当成合法缓存条目。
		snap.version = endVersion
	}
	return snap
}

// snapshotWindowKey 生成窗口键前缀（不含桶表版本）。版本与昵称在缓存条目中
// 单独比对：version 只用于失效判断，不进入 key，避免高并发下键无限增长。
func (r *Recorder) snapshotWindowKey(w Window, nicks map[string]string) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(w.Hours))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(w.From.UnixNano(), 10))
	b.WriteByte('|')
	b.WriteString(strconv.FormatInt(w.To.UnixNano(), 10))
	b.WriteByte('|')
	if w.From.IsZero() && w.To.IsZero() && w.Hours > 0 {
		b.WriteString(strconv.FormatInt(time.Now().Truncate(time.Hour).Unix(), 10))
	}
	b.WriteByte('|')
	keys := make([]string, 0, len(nicks))
	for k := range nicks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(nicks[k])
		b.WriteByte(',')
	}
	return b.String()
}

// tableVersionNow 返回桶表当前全局版本。读一个 atomic 即可；分片 version 仍
// 用于落盘成功后的安全清脏，不参与快照缓存判断。
func (r *Recorder) tableVersionNow() uint64 {
	return r.tableVersion.Load()
}

func (r *Recorder) cachedSnapshot(key string) (Snapshot, bool) {
	return r.cachedSnapshotRates(key, 0)
}

func (r *Recorder) cachedSnapshotRates(key string, rateRevision uint64) (Snapshot, bool) {
	r.snapMu.Lock()
	defer r.snapMu.Unlock()
	entry, ok := r.snapCache[key]
	if !ok || time.Since(entry.at) >= snapshotCacheTTL {
		return Snapshot{}, false
	}
	if entry.rateRevision != rateRevision {
		delete(r.snapCache, key)
		return Snapshot{}, false
	}
	// 桶表在缓存构建后任意一次 Add 都会推进 version。这里仍让调用方
	// 重新聚合，保证“请求完成后下一次读取能看到它”，不能用 TTL 掩盖新增
	// 数据；但不要删除条目：并发读者可能正在使用这份快照，删除只会让他们
	// 同时落入全量聚合。publishSnapshotRates 会在新版本聚合完成后原地覆盖。
	if entry.snapshot.version != r.tableVersionNow() {
		return Snapshot{}, false
	}
	return entry.snapshot, true
}

func (r *Recorder) publishSnapshot(key string, snap Snapshot, version uint64) {
	r.publishSnapshotRates(key, snap, version, 0)
}

func (r *Recorder) publishSnapshotRates(key string, snap Snapshot, version, rateRevision uint64) {
	snap.version = version
	r.snapMu.Lock()
	defer r.snapMu.Unlock()
	if r.snapCache == nil {
		r.snapCache = make(map[string]snapshotCacheEntry)
	}
	// 面板通常只在一两个窗口之间切换；先清掉过期条目，超过 8 个时直接重置，
	// 避免用户频繁切自定义区间后 cache 无界增长。
	for k, e := range r.snapCache {
		if time.Since(e.at) >= snapshotCacheTTL {
			delete(r.snapCache, k)
		}
	}
	if len(r.snapCache) >= 8 {
		r.snapCache = make(map[string]snapshotCacheEntry)
	}
	r.snapCache[key] = snapshotCacheEntry{snapshot: snap, at: time.Now(), key: key, rateRevision: rateRevision}
}

func (r *Recorder) snapshotWindow(w Window, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	from, to := w.bounds()
	explicit := !w.From.IsZero() || !w.To.IsZero()
	windowed := !from.IsZero() || !to.IsZero()

	// 每个分片只在遍历该分片时持读锁并直接聚合，不复制桶表。旧实现复制全部
	// 45k 桶（含窗口外），每轮轮询多分配数 MB；分片还避免了长读锁阻塞记账。
	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	keyAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}
	creditAcctAgg := map[string]*creditAcc{}
	creditModelAgg := map[string]*creditAcc{}
	creditKeyAgg := map[string]*creditAcc{}
	creditRealmAgg := map[string]*creditAcc{}
	rateCache := map[string]string{}

	parsed := make(map[string]bucketTimeAt, 96)
	since := ""
	matched := 0
	// currentRate 是跨包回调（上游目录锁）。必须在释放分片读锁后再调用，
	// 否则外部锁获取顺序与其它路径交叉时会形成锁依赖，且会把慢调用拖进
	// 记账热路径。这里只把缺失倍率的桶记下来，第二遍按需回填。
	type ratePending struct {
		realm, model string
		b            bucket
	}
	var pending []ratePending
	r.rollupMu.RLock()
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.RLock()
		for _, b := range sh.buckets {
			if b.Scope < since || since == "" {
				since = b.Scope
			}
			if windowed {
				// 用二值 ok 判断是否已缓存：脏 scope 的 tbt.ok=false，若拿 tbt.ok
				// 当缓存命中标志，每个脏桶都会在每一轮快照中重复解析。
				tbt, cached := parsed[b.Scope]
				if !cached {
					ts, valid := bucketTime(b.Scope)
					tbt = bucketTimeAt{ts: ts, ok: valid}
					parsed[b.Scope] = tbt
				}
				// 解析失败的脏桶不进窗口聚合（也不该出现在任何口径里）。
				if !tbt.ok {
					continue
				}
				if !from.IsZero() && tbt.ts.Before(from) {
					continue
				}
				if !to.IsZero() && tbt.ts.After(to) {
					continue
				}
			}
			matched++
			total.add(b)

			// 每个维度只做一次 map 查找并复用累加器：窗口聚合会遍历数万桶，
			// 原先「先判 nil 再索引」的双查找会把字符串哈希算两遍。
			ra := realmAgg[b.Realm]
			if ra == nil {
				ra = &aggAcc{}
				realmAgg[b.Realm] = ra
			}
			ra.add(b)

			aa := acctAgg[b.UID]
			if aa == nil {
				aa = &aggAcc{}
				acctAgg[b.UID] = aa
			}
			aa.add(b)
			// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
			// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
			if acctRealm[b.UID] == "" {
				acctRealm[b.UID] = b.Realm
			}

			ma := modelAgg[b.Model]
			if ma == nil {
				ma = &aggAcc{}
				modelAgg[b.Model] = ma
			}
			ma.add(b)

			ka := keyAgg[b.KeyID]
			if ka == nil {
				ka = &aggAcc{}
				keyAgg[b.KeyID] = ka
			}
			ka.add(b)

			if strings.HasPrefix(b.Scope, "h:") {
				scope := b.Scope[2:]
				ha := hourSeries[scope]
				if ha == nil {
					ha = &aggAcc{}
					hourSeries[scope] = ha
				}
				ha.add(b)
			} else {
				scope := strings.TrimPrefix(b.Scope, "d:")
				da := daySeries[scope]
				if da == nil {
					da = &aggAcc{}
					daySeries[scope] = da
				}
				da.add(b)
			}
			if b.CRN > 0 {
				cr := creditRealmAgg[b.Realm]
				if cr == nil {
					cr = &creditAcc{CreditAgg: CreditAgg{Key: b.Realm, Realm: b.Realm}}
					creditRealmAgg[b.Realm] = cr
				}
				cr.add(b)
				ck := creditKeyAgg[b.KeyID]
				if ck == nil {
					ck = &creditAcc{CreditAgg: CreditAgg{Key: b.KeyID}}
					creditKeyAgg[b.KeyID] = ck
				}
				ck.add(b)
				ca := creditAcctAgg[b.UID]
				if ca == nil {
					ca = &creditAcc{CreditAgg: CreditAgg{
						Key:      b.UID,
						Realm:    b.Realm,
						Nickname: nicks[b.UID],
					}}
					creditAcctAgg[b.UID] = ca
				}
				ca.add(b)
				model := canonicalUsageModel(b.Model)
				rate := b.Rate
				if rate == "" && currentRate != nil {
					pending = append(pending, ratePending{realm: b.Realm, model: model, b: *b})
					continue
				}
				modelKey := model + "\x00" + rate
				cm := creditModelAgg[modelKey]
				if cm == nil {
					cm = &creditAcc{CreditAgg: CreditAgg{Key: model, Rate: rate}}
					creditModelAgg[modelKey] = cm
				}
				cm.add(b)
			}
		}
		sh.mu.RUnlock()
	}
	// currentRate 是跨包回调（可能是慢的目录刷新）。它只消费 pending 中的桶
	// 快照副本，不需要 rollupMu；扫描完成后立即释放屏障，避免慢回调阻塞
	// Rollup 写锁与落盘。
	r.rollupMu.RUnlock()
	for _, p := range pending {
		cacheKey := p.realm + "\x00" + p.model
		rate, ok := rateCache[cacheKey]
		if !ok {
			rate = currentRate(p.realm, p.model)
			rateCache[cacheKey] = rate
		}
		modelKey := p.model + "\x00" + rate
		cm := creditModelAgg[modelKey]
		if cm == nil {
			cm = &creditAcc{CreditAgg: CreditAgg{Key: p.model, Rate: rate}}
			creditModelAgg[modelKey] = cm
		}
		cm.add(&p.b)
	}

	snap := Snapshot{
		Totals: total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) {
			return k, ""
		}),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:         keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		ByKey:           keyed(keyAgg, func(k string) (string, string) { return k, "" }),
		CreditByAccount: creditKeyed(creditAcctAgg),
		CreditByModel:   creditKeyed(creditModelAgg),
		CreditByKey:     creditKeyed(creditKeyAgg),
		CreditByRealm:   creditKeyed(creditRealmAgg),
		Buckets:         matched,
		Generated:       time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since 去掉 scope 前缀（"h:2026-09-16T13" → "2026-09-16T13"）给前端展示；
	// 无任何桶时保持空（无数据不伪造起点）。
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	// 回显实际生效的区间：**仅显式区间口径**。滚动窗口由 hours 表达（前端自己
	// 知道选的是哪个预设），全部历史没有区间——两者回显都会变成噪音。
	if explicit {
		if !w.From.IsZero() {
			snap.WindowFrom = w.From.Format(time.RFC3339)
		}
		if !w.To.IsZero() {
			snap.WindowTo = w.To.Format(time.RFC3339)
		}
	}
	return snap
}

func canonicalUsageModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"cn:", "global:"} {
		if strings.HasPrefix(model, prefix) {
			model = strings.TrimPrefix(model, prefix)
			break
		}
	}
	if model == "" {
		return "(unknown)"
	}
	return model
}

func creditKeyed(m map[string]*creditAcc) []CreditAgg {
	out := make([]CreditAgg, 0, len(m))
	for _, v := range m {
		out = append(out, v.finish())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}
		if out[i].CreditTokens != out[j].CreditTokens {
			return out[i].CreditTokens > out[j].CreditTokens
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Rate < out[j].Rate
	})
	return out
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	n := 0
	for i := range r.shards {
		r.shards[i].mu.RLock()
		n += len(r.shards[i].buckets)
		r.shards[i].mu.RUnlock()
	}
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}

package reqlog

import (
	"bufio"
	"container/heap"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 性能分位查询独立于请求列表的 limit：列表只需要最近 N 条供浏览，而分位若也
// 只看最近 1000 条，会在流量高峰时系统性偏乐观。这里流式扫描窗口内原始样本，
// 并为内存与单次查询成本设置独立上限。
const (
	DefaultPerfHours   = 24
	MaxPerfHours       = 24 * 31
	DefaultPerfSamples = 50000
	MaxPerfSamples     = 200000
)

// PerfQuery 是性能分位聚合的查询条件。
//
// From/To 是闭区间；两者都为空时使用 Hours（默认 24h，最多 31 天）。只给一端时，
// 另一端补到 now 或 Hours 窗口起点。显式区间同样裁到最多 31 天，避免一次请求扫描
// 无限历史。Model/Account 沿用 Filter 的大小写不敏感包含匹配；Realm 只接受
// cn/global（空 = 不过滤）。
//
// 归档 Event 没有独立 realm 字段：模型带 cn:/global: 前缀时以前缀为准；裸模型
// 则查 RealmByAccount（面板用池内脱敏账号标签建立映射）；两者都未知时按网关
// 协议归 cn。RealmByAccount 的键形如 "昵称(uid8)"，不会包含凭证。
type PerfQuery struct {
	From           time.Time
	To             time.Time
	Hours          int
	Model          string
	KeyID          string
	Account        string
	Realm          string
	Samples        int
	RealmByAccount map[string]string
	// Now 仅供测试注入；零值使用 time.Now()。
	Now time.Time
}

// PerfBlock 是一组原始样本的最近秩（nearest-rank）分位，不做插值或聚合近似。
type PerfBlock struct {
	Avg     float64 `json:"avg"`
	P50     float64 `json:"p50"`
	P90     float64 `json:"p90"`
	P99     float64 `json:"p99"`
	Max     float64 `json:"max"`
	Samples int     `json:"samples"`
}

// PerfAggregate 是一个维度的请求计数与延迟/吞吐分位。
//
// 延迟分位只接收「OK、outcome=success 且 duration_ms>0」的样本；错误与中断只
// 计入请求数，不污染性能数字。TTFT 还要求 ttfb_ms>0；generation 为
// duration_ms-ttfb_ms（负值钳为 0）；TPS 只使用 completion_tokens>0 且
// generation>0 的成功样本。没有有效样本时 block 为 null，而不是伪造 0。
type PerfAggregate struct {
	Requests     int64      `json:"requests"`
	Success      int64      `json:"success"`
	Errors       int64      `json:"errors"`
	Interrupted  int64      `json:"interrupted"`
	TTFTMs       *PerfBlock `json:"ttft_ms"`
	GenerationMs *PerfBlock `json:"generation_ms"`
	WallMs       *PerfBlock `json:"wall_ms"`
	TokensPerSec *PerfBlock `json:"tokens_per_sec"`
}

// PerfStats 是性能分位接口的响应体。by_model 的键是不带 cn:/global: 前缀的
// 模型名；域维度单独放在 by_model_realm，账号维度放在 by_model_acct。账号键
// 沿用归档里的脱敏标签（昵称(uid8)），不会包含完整 UID 或任何凭证。
type PerfStats struct {
	WindowFrom   time.Time `json:"window_from"`
	WindowTo     time.Time `json:"window_to"`
	SampleLimit  int       `json:"sample_limit"`
	SampleCapped bool      `json:"sample_capped"`
	Sampled      int64     `json:"sampled"`
	PerfAggregate
	ByModel      map[string]PerfAggregate                       `json:"by_model"`
	ByModelRealm map[string]map[string]PerfAggregate            `json:"by_model_realm"`
	ByModelAcct  map[string]map[string]map[string]PerfAggregate `json:"by_model_acct"`

	overallAcc perfAccumulator
	modelAcc   map[string]*perfAccumulator
	realmAcc   map[string]map[string]*perfAccumulator
	acctAcc    map[string]map[string]map[string]*perfAccumulator
}

// PerfStats 从归档中流式聚合性能分位。它不复用 ReadArchive 的 1000 条返回上限，
// 也不会把请求正文、调用来源或凭证带入结果；坏行与不符合筛选的行直接跳过。
// 样本上限按「最新匹配记录」截取，SampleCapped 明确标识是否还有更早的匹配样本。
func (r *Recorder) PerfStats(q PerfQuery) (PerfStats, error) {
	q = q.normalized()
	stats := newPerfStats(q)
	if r == nil || r.archive == nil || !r.archive.enabled() {
		stats.finalize()
		return stats, nil
	}
	sig, entries, err := perfDirSignatureFor(r.archive.cfg.Dir)
	if err != nil {
		return PerfStats{}, err
	}
	cacheKey := perfQueryCacheKey(q)
	now := time.Now()
	r.perfMu.Lock()
	if r.perfCache.hit && r.perfCache.key == cacheKey && r.perfCache.sig == sig.sig && now.Sub(r.perfCache.at) < perfCacheTTL {
		cached := r.perfCache.stats
		r.perfMu.Unlock()
		return cached, nil
	}
	r.perfMu.Unlock()
	filter := Filter{
		Model:   q.Model,
		KeyID:   q.KeyID,
		Account: q.Account,
		From:    q.From,
		To:      q.To,
	}.normalized()

	samples := &perfSampleHeap{}
	heap.Init(samples)
	var matched int64
	for _, name := range archiveFileNamesNewestFirst(entries) {
		f, err := os.Open(filepath.Join(r.archive.cfg.Dir, name))
		if err != nil {
			return PerfStats{}, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			var e Event
			if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.matchNormalized(e) {
				continue
			}
			if q.Realm != "" {
				realm, _ := perfEventModel(e.Model, e.Account, q.RealmByAccount)
				if realm != q.Realm {
					continue
				}
			}
			matched++
			if samples.Len() < q.Samples {
				heap.Push(samples, e)
				continue
			}
			// 已收满上限：只保留事件时间更新的一条，确保上限截取的是「最新
			// 匹配样本」而不是文件名/物理行的偶然顺序。同刻样本保留先出现者，
			// 与归档的稳定排序口径一致。
			if e.Time.After((*samples)[0].Time) {
				heap.Pop(samples)
				heap.Push(samples, e)
			}
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return PerfStats{}, scanErr
		}
	}
	latest := make([]Event, samples.Len())
	copy(latest, *samples)
	sort.SliceStable(latest, func(i, j int) bool { return latest[i].Time.Before(latest[j].Time) })
	stats.SampleCapped = matched > int64(q.Samples)
	for _, e := range latest {
		realm, _ := perfEventModel(e.Model, e.Account, q.RealmByAccount)
		stats.add(e, realm)
	}
	stats.finalize()
	r.perfMu.Lock()
	r.perfCache = perfCacheEntry{key: cacheKey, sig: sig.sig, at: now, stats: stats, hit: true}
	r.perfMu.Unlock()
	return stats, nil
}

// perfDirSignatureFor 读取目录条目并生成稳定签名。entries 保留给后续
// archiveFileNamesNewestFirst 使用，避免同一次查询重复 ReadDir。
func perfDirSignatureFor(dir string) (perfDirSignature, []os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return perfDirSignature{}, nil, err
	}
	files := make([]struct {
		name  string
		size  int64
		mtime int64
	}, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, struct {
			name  string
			size  int64
			mtime int64
		}{entry.Name(), info.Size(), info.ModTime().UnixNano()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f.name)
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(f.size, 10))
		b.WriteByte('|')
		b.WriteString(strconv.FormatInt(f.mtime, 10))
		b.WriteByte(10)
	}
	return perfDirSignature{sig: b.String()}, entries, nil
}

// perfQueryCacheKey 只包含影响聚合结果的规范化查询字段；Now 已被
// normalized() 展开为 From/To，不参与键（同窗口不同调用时刻可复用）。
func perfQueryCacheKey(q PerfQuery) string {
	var b strings.Builder
	b.WriteString(q.From.UTC().Format(time.RFC3339Nano))
	b.WriteByte('|')
	b.WriteString(q.To.UTC().Format(time.RFC3339Nano))
	b.WriteString("|s=")
	b.WriteString(strconv.Itoa(q.Samples))
	b.WriteString("|m=")
	b.WriteString(q.Model)
	b.WriteString("|k=")
	b.WriteString(q.KeyID)
	b.WriteString("|a=")
	b.WriteString(q.Account)
	b.WriteString("|r=")
	b.WriteString(q.Realm)
	if len(q.RealmByAccount) > 0 {
		keys := make([]string, 0, len(q.RealmByAccount))
		for k := range q.RealmByAccount {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteByte('|')
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(q.RealmByAccount[k])
		}
	}
	return b.String()
}

// perfSampleHeap 是「按事件时间保留最新 N 条」的最小堆（堆顶为窗口内最旧者）。
// 堆内只做时间选择；最终按时间升序展开后聚合，保证同刻样本的稳定顺序。
type perfSampleHeap []Event

func (h perfSampleHeap) Len() int           { return len(h) }
func (h perfSampleHeap) Less(i, j int) bool { return h[i].Time.Before(h[j].Time) }
func (h perfSampleHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *perfSampleHeap) Push(v any)        { *h = append(*h, v.(Event)) }
func (h *perfSampleHeap) Pop() any {
	old := *h
	n := len(old)
	v := old[n-1]
	*h = old[:n-1]
	return v
}

func newPerfStats(q PerfQuery) PerfStats {
	return PerfStats{
		WindowFrom:   q.From,
		WindowTo:     q.To,
		SampleLimit:  q.Samples,
		modelAcc:     map[string]*perfAccumulator{},
		realmAcc:     map[string]map[string]*perfAccumulator{},
		acctAcc:      map[string]map[string]map[string]*perfAccumulator{},
		ByModel:      map[string]PerfAggregate{},
		ByModelRealm: map[string]map[string]PerfAggregate{},
		ByModelAcct:  map[string]map[string]map[string]PerfAggregate{},
	}
}

func (q PerfQuery) normalized() PerfQuery {
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	if q.Samples <= 0 {
		q.Samples = DefaultPerfSamples
	}
	if q.Samples > MaxPerfSamples {
		q.Samples = MaxPerfSamples
	}
	q.Hours = normalizePerfHours(q.Hours)
	q.Model = strings.TrimSpace(q.Model)
	q.KeyID = strings.TrimSpace(q.KeyID)
	q.Account = strings.TrimSpace(q.Account)
	q.Realm = strings.ToLower(strings.TrimSpace(q.Realm))

	switch {
	case q.From.IsZero() && q.To.IsZero():
		q.From = now.Add(-time.Duration(q.Hours) * time.Hour)
		q.To = now
	case q.From.IsZero():
		q.From = q.To.Add(-time.Duration(q.Hours) * time.Hour)
	case q.To.IsZero():
		q.To = now
	}
	if q.From.After(q.To) {
		// 反向区间没有业务含义。退化为瞬时窗口而不静默交换端点，避免把拼错
		// 的参数解释成另一段时间。
		q.To = q.From
	}
	if q.To.Sub(q.From) > MaxPerfHours*time.Hour {
		q.From = q.To.Add(-MaxPerfHours * time.Hour)
	}
	return q
}

func normalizePerfHours(v int) int {
	if v <= 0 {
		return DefaultPerfHours
	}
	if v > MaxPerfHours {
		return MaxPerfHours
	}
	return v
}

// perfEventModel 返回事件所属域与裸模型名。模型前缀是请求侧权威路由信息；
// 裸模型再按账号标签映射到池内 realm；未知账号回落 cn（与网关裸名协议一致）。
func perfEventModel(model, account string, realmByAccount map[string]string) (realm, bare string) {
	model = strings.TrimSpace(model)
	realm = "cn"
	bare = model
	explicit := false
	if i := strings.IndexByte(model, ':'); i >= 0 {
		prefix := model[:i]
		if prefix == "cn" || prefix == "global" {
			realm, bare, explicit = prefix, model[i+1:], true
		}
	}
	if !explicit {
		if r := strings.ToLower(strings.TrimSpace(realmByAccount[strings.TrimSpace(account)])); r == "cn" || r == "global" {
			realm = r
		}
	}
	return realm, bare
}

type perfAccumulator struct {
	requests    int64
	success     int64
	errors      int64
	interrupted int64
	ttft        []float64
	generation  []float64
	wall        []float64
	tps         []float64
}

func (a *perfAccumulator) add(e Event) {
	a.requests++
	success := e.OK && e.Outcome == OutcomeSuccess
	if success {
		a.success++
	} else if e.Outcome == OutcomeInterrupted {
		a.interrupted++
	} else {
		a.errors++
	}
	if !success || e.DurationMs <= 0 {
		return
	}
	duration := float64(e.DurationMs)
	a.wall = append(a.wall, duration)
	if e.TTFBMs <= 0 {
		return
	}
	a.ttft = append(a.ttft, float64(e.TTFBMs))
	gen := duration - float64(e.TTFBMs)
	if gen < 0 {
		gen = 0
	}
	a.generation = append(a.generation, gen)
	if e.CompletionTokens > 0 && gen > 0 {
		a.tps = append(a.tps, float64(e.CompletionTokens)*1000/gen)
	}
}

func (a *perfAccumulator) result() PerfAggregate {
	return PerfAggregate{
		Requests:     a.requests,
		Success:      a.success,
		Errors:       a.errors,
		Interrupted:  a.interrupted,
		TTFTMs:       perfBlock(a.ttft),
		GenerationMs: perfBlock(a.generation),
		WallMs:       perfBlock(a.wall),
		TokensPerSec: perfBlock(a.tps),
	}
}

// perfBlock 使用 nearest-rank 分位（ceil(p*n)-1，无插值）。同一批原始样本
// 排序一次后同时给出 p50/p90/p99/max，保证各分位来自同一口径。
func perfBlock(values []float64) *PerfBlock {
	if len(values) == 0 {
		return nil
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	n := len(ordered)
	rank := func(p float64) float64 {
		idx := int(math.Ceil(p/100*float64(n))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		return roundPerf(ordered[idx])
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return &PerfBlock{
		Avg:     roundPerf(sum / float64(n)),
		P50:     rank(50),
		P90:     rank(90),
		P99:     rank(99),
		Max:     roundPerf(ordered[n-1]),
		Samples: n,
	}
}

func roundPerf(v float64) float64 {
	return math.Round(v*10) / 10
}

func (s *PerfStats) add(e Event, realm string) {
	s.Sampled++
	_, model := perfEventModel(e.Model, e.Account, nil)
	model = strings.TrimSpace(model)
	if model == "" {
		model = "-"
	}
	account := strings.TrimSpace(e.Account)
	if account == "" {
		account = "-"
	}

	s.overallAcc.add(e)
	modelAgg := s.modelAcc[model]
	if modelAgg == nil {
		modelAgg = &perfAccumulator{}
		s.modelAcc[model] = modelAgg
	}
	modelAgg.add(e)
	if s.realmAcc[model] == nil {
		s.realmAcc[model] = map[string]*perfAccumulator{}
	}
	realmAgg := s.realmAcc[model][realm]
	if realmAgg == nil {
		realmAgg = &perfAccumulator{}
		s.realmAcc[model][realm] = realmAgg
	}
	realmAgg.add(e)
	if s.acctAcc[model] == nil {
		s.acctAcc[model] = map[string]map[string]*perfAccumulator{}
	}
	if s.acctAcc[model][realm] == nil {
		s.acctAcc[model][realm] = map[string]*perfAccumulator{}
	}
	acctAgg := s.acctAcc[model][realm][account]
	if acctAgg == nil {
		acctAgg = &perfAccumulator{}
		s.acctAcc[model][realm][account] = acctAgg
	}
	acctAgg.add(e)
}

func (s *PerfStats) finalize() {
	s.PerfAggregate = s.overallAcc.result()
	for model, agg := range s.modelAcc {
		s.ByModel[model] = agg.result()
	}
	for model, realms := range s.realmAcc {
		if s.ByModelRealm[model] == nil {
			s.ByModelRealm[model] = map[string]PerfAggregate{}
		}
		for realm, agg := range realms {
			s.ByModelRealm[model][realm] = agg.result()
		}
	}
	for model, realms := range s.acctAcc {
		if s.ByModelAcct[model] == nil {
			s.ByModelAcct[model] = map[string]map[string]PerfAggregate{}
		}
		for realm, accounts := range realms {
			if s.ByModelAcct[model][realm] == nil {
				s.ByModelAcct[model][realm] = map[string]PerfAggregate{}
			}
			for account, agg := range accounts {
				s.ByModelAcct[model][realm][account] = agg.result()
			}
		}
	}
	// 聚合完成后释放原始样本切片，响应对象只保留聚合结果。归档事件可能包含
	// 来源 IP/UA（按部署开关），不应在返回给 handler 的对象里额外延长生命周期。
	s.modelAcc = nil
	s.realmAcc = nil
	s.acctAcc = nil
}

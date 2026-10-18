package reqlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func perfTestRecorder(t *testing.T, events []Event) *Recorder {
	t.Helper()
	r := New(Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 8 << 20, RetentionDays: 7})
	for _, e := range events {
		r.Record(e)
	}
	r.Close()
	return r
}

func perfEvent(at time.Time, model, account string, ok bool, outcome string, duration, ttfb int64) Event {
	return Event{
		Time:             at,
		RequestID:        "req-" + model + "-" + account,
		Path:             "/v1/chat/completions",
		Model:            model,
		Account:          account,
		Status:           200,
		OK:               ok,
		Outcome:          outcome,
		DurationMs:       duration,
		TTFBMs:           ttfb,
		CompletionTokens: 10,
	}
}

// 空归档/未启用归档必须返回稳定的空结构，而不是 nil map 或 500。
func TestPerfStatsEmptyArchive(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	r := New(Config{})
	got, err := r.PerfStats(PerfQuery{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 0 || got.Requests != 0 || got.SampleLimit != DefaultPerfSamples {
		t.Fatalf("empty perf = %+v", got)
	}
	if got.WindowTo != now || got.WindowFrom != now.Add(-24*time.Hour) {
		t.Fatalf("default window = %v..%v", got.WindowFrom, got.WindowTo)
	}
	if got.ByModel == nil || got.ByModelRealm == nil || got.ByModelAcct == nil {
		t.Fatalf("maps must be non-nil: %+v", got)
	}
	if got.TTFTMs != nil || got.GenerationMs != nil || got.WallMs != nil || got.TokensPerSec != nil {
		t.Fatalf("empty blocks must be null: %+v", got.PerfAggregate)
	}
}

// 延迟只统计成功、有效 duration；TTFT 只收 >0；generation 钳零；TPS 只收
// completion_tokens>0 且 generation>0。错误/中断只进请求计数。
func TestPerfStatsSuccessAndMetricBoundaries(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	ok := func(duration, ttfb, completion int64) Event {
		e := perfEvent(base, "glm-5.3", "alice(12345678)", true, OutcomeSuccess, duration, ttfb)
		e.CompletionTokens = completion
		return e
	}
	events := []Event{
		ok(1000, 200, 10), // wall 1000, ttft 200, gen 800, tps 12.5
		ok(500, 0, 10),    // wall 500，无 TTFT → 不进 ttft/gen/tps
		ok(600, 900, 10),  // wall 600，gen 钳 0 → ttft 900，无 TPS
		ok(0, 100, 10),    // duration 无效 → 只计成功请求
		ok(800, 100, 0),   // 无 completion → 不进 TPS
		ok(900, 100, -1),  // 非法 completion → 不进 TPS
		{Time: base, Model: "glm-5.3", Account: "alice(12345678)", Status: 200, OK: false, Outcome: OutcomeHTTPError, DurationMs: 50},
		{Time: base, Model: "glm-5.3", Account: "alice(12345678)", Status: 499, OK: false, Outcome: OutcomeInterrupted, DurationMs: 50},
	}
	r := perfTestRecorder(t, events)
	got, err := r.PerfStats(PerfQuery{From: base.Add(-time.Hour), To: base.Add(time.Hour), Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 8 || got.Requests != 8 || got.Success != 6 || got.Errors != 1 || got.Interrupted != 1 {
		t.Fatalf("counts = %+v", got)
	}
	if got.WallMs == nil || got.WallMs.Samples != 5 || got.WallMs.P50 != 800 || got.WallMs.Max != 1000 {
		t.Fatalf("wall = %+v", got.WallMs)
	}
	if got.TTFTMs == nil || got.TTFTMs.Samples != 4 || got.TTFTMs.P50 != 100 || got.TTFTMs.P90 != 900 || got.TTFTMs.Max != 900 {
		t.Fatalf("ttft = %+v", got.TTFTMs)
	}
	if got.GenerationMs == nil || got.GenerationMs.Samples != 4 || got.GenerationMs.P50 != 700 || got.GenerationMs.Max != 800 {
		t.Fatalf("generation = %+v", got.GenerationMs)
	}
	// 仅 duration=1000/ttfb=200/completion=10 同时满足 TTFT>0、gen>0、
	// completion>0；负 completion 不得被当成有效吞吐样本。
	if got.TokensPerSec == nil || got.TokensPerSec.Samples != 1 || got.TokensPerSec.P50 != 12.5 {
		t.Fatalf("tps samples = %+v", got.TokensPerSec)
	}
}

// 分位用 nearest-rank：6 个样本的 p50=第 3 个，p90=第 6 个；p99 也落在末项。
func TestPerfBlockNearestRank(t *testing.T) {
	values := []float64{10, 20, 30, 40, 50, 60}
	got := perfBlock(values)
	if got == nil {
		t.Fatal("nil block")
	}
	if got.Samples != 6 || got.P50 != 30 || got.P90 != 60 || got.P99 != 60 || got.Max != 60 {
		t.Fatalf("block = %+v", got)
	}
	if got.Avg != 35 {
		t.Fatalf("avg = %v", got.Avg)
	}
	// 输入顺序不能被原地打乱（调用方仍可能读取原序列）。
	if values[0] != 10 || values[len(values)-1] != 60 {
		t.Fatalf("perfBlock mutated input: %v", values)
	}
}

// 时间窗、模型、账号、realm 过滤必须同时生效；裸模型按账号映射判域，
// 显式前缀优先。归档中的 uid 是脱敏标签，响应也不能出现完整凭证。
func TestPerfStatsFiltersAndRealmMapping(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	events := []Event{
		perfEvent(base.Add(-2*time.Hour), "cn:glm-5.3", "alicenick(12345678)", true, OutcomeSuccess, 100, 10),
		perfEvent(base.Add(-time.Hour), "global:kimi-k3", "bob(87654321)", true, OutcomeSuccess, 300, 20),
		perfEvent(base.Add(-30*time.Minute), "glm-5.3", "bob(87654321)", true, OutcomeSuccess, 500, 30),
		perfEvent(base.Add(-time.Minute), "glm-5.3", "alicenick(12345678)", false, OutcomeHTTPError, 50, 0),
		{Time: base.Add(-3 * time.Hour), Model: "old-model", Account: "old(00000000)", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 999},
	}
	r := perfTestRecorder(t, events)
	query := PerfQuery{
		From:           base.Add(-2 * time.Hour),
		To:             base,
		RealmByAccount: map[string]string{"bob(87654321)": "global", "alicenick(12345678)": "cn"},
		Now:            base,
	}
	got, err := r.PerfStats(query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 4 {
		t.Fatalf("sampled = %d, want 4 (old row outside window)", got.Sampled)
	}
	if got.ByModel["glm-5.3"].Requests != 3 || got.ByModel["kimi-k3"].Requests != 1 {
		t.Fatalf("by_model = %+v", got.ByModel)
	}
	if got.ByModelRealm["glm-5.3"]["cn"].Requests != 2 || got.ByModelRealm["glm-5.3"]["global"].Requests != 1 {
		t.Fatalf("by_model_realm = %+v", got.ByModelRealm)
	}
	if got.ByModelAcct["glm-5.3"]["cn"]["alicenick(12345678)"].Requests != 2 ||
		got.ByModelAcct["glm-5.3"]["cn"]["alicenick(12345678)"].Errors != 1 {
		t.Fatalf("by_model_acct = %+v", got.ByModelAcct)
	}

	filtered, err := r.PerfStats(PerfQuery{From: query.From, To: query.To, Model: "KIMI", Realm: "global", Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Sampled != 1 || filtered.ByModel["kimi-k3"].Requests != 1 {
		t.Fatalf("model/realm filter = %+v", filtered)
	}
	byAccount, err := r.PerfStats(PerfQuery{From: query.From, To: query.To, Account: "alice", Realm: "cn", RealmByAccount: query.RealmByAccount, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if byAccount.Sampled != 2 || byAccount.Errors != 1 {
		t.Fatalf("account filter = %+v", byAccount)
	}
}

// 样本上限按最新匹配记录截取；超过上限必须明确 sample_capped，且不把截断样本
// 计入分位。上限本身也做合理裁剪，避免恶意参数撑爆内存。
func TestPerfStatsSampleCap(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	events := make([]Event, 0, 5)
	for i := 0; i < 5; i++ {
		events = append(events, perfEvent(base.Add(time.Duration(i)*time.Minute), "m", "a(12345678)", true, OutcomeSuccess, int64(100+i), 10))
	}
	r := perfTestRecorder(t, events)
	got, err := r.PerfStats(PerfQuery{From: base.Add(-time.Hour), To: base.Add(time.Hour), Samples: 3, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 3 || !got.SampleCapped || got.SampleLimit != 3 {
		t.Fatalf("cap = sampled:%d capped:%v limit:%d", got.Sampled, got.SampleCapped, got.SampleLimit)
	}
	if got.WallMs == nil || got.WallMs.Samples != 3 || got.WallMs.Max != 104 {
		t.Fatalf("latest samples not kept: %+v", got.WallMs)
	}

	all, err := r.PerfStats(PerfQuery{From: base.Add(-time.Hour), To: base.Add(time.Hour), Samples: 99, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if all.Sampled != 5 || all.SampleCapped {
		t.Fatalf("uncapped = %+v", all)
	}
}

// 文件名较新的归档里也可能含较早事件（备份恢复/补写）；上限必须按事件时间
// 选最新 N 条，不能按文件顺序提前停止。
func TestPerfStatsSampleCapUsesEventTimeAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	newerFile := `{"time":"2026-10-18T09:00:00Z","model":"m","account":"a(12345678)","status":200,"ok":true,"outcome":"success","duration_ms":100,"ttfb_ms":10}
`
	olderFile := `{"time":"2026-10-18T11:00:00Z","model":"m","account":"a(12345678)","status":200,"ok":true,"outcome":"success","duration_ms":900,"ttfb_ms":10}
`
	if err := os.WriteFile(filepath.Join(dir, "requests-2026-10-18.1.jsonl"), []byte(newerFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requests-2026-10-17.jsonl"), []byte(olderFile), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	got, err := r.PerfStats(PerfQuery{From: base.Add(-6 * time.Hour), To: base, Samples: 1, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 1 || !got.SampleCapped || got.WallMs == nil || got.WallMs.Max != 900 {
		t.Fatalf("latest across files = %+v", got.WallMs)
	}
}

// 归档里损坏的 JSONL 行不能把整个接口打成 500；其余有效行仍应聚合。
func TestPerfStatsSkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "requests-"+base.Format("2006-01-02")+".jsonl")
	lines := `{"time":"2026-10-18T11:00:00Z","model":"m","account":"a(12345678)","status":200,"ok":true,"outcome":"success","duration_ms":100,"ttfb_ms":10}
not-json
{"time":"2026-10-18T11:01:00Z","model":"m","account":"a(12345678)","status":200,"ok":true,"outcome":"success","duration_ms":300,"ttfb_ms":20}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	got, err := r.PerfStats(PerfQuery{From: base.Add(-2 * time.Hour), To: base, Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 2 || got.WallMs == nil || got.WallMs.P50 != 100 {
		t.Fatalf("malformed-line handling = %+v", got)
	}
}

// 性能分位缓存必须只在归档签名不变时命中；新写入应立即可见，不能被 2 秒
// TTL 遮住。这里按文件大小/mtime 变化驱动失效。
func TestPerfStatsCacheInvalidatesOnArchiveChange(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 8 << 20, RetentionDays: 7})
	r.Record(perfEvent(base, "glm-5.3", "a(12345678)", true, OutcomeSuccess, 1000, 200))
	r.Close()

	q := PerfQuery{From: base.Add(-time.Hour), To: base.Add(time.Hour), Now: base}
	first, err := r.PerfStats(q)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sampled != 1 {
		t.Fatalf("first sampled=%d want 1", first.Sampled)
	}
	second, err := r.PerfStats(q)
	if err != nil {
		t.Fatal(err)
	}
	if second.Sampled != 1 {
		t.Fatalf("cache hit sampled=%d want 1", second.Sampled)
	}

	// 另起一个 writer 向同一目录追加，必须让缓存失效。
	w := newArchiveWriter(Config{Enabled: true, Dir: dir, MaxBytes: 8 << 20, RetentionDays: 7})
	w.enqueue(perfEvent(base.Add(time.Second), "glm-5.3", "b(87654321)", true, OutcomeSuccess, 900, 100))
	w.close()
	third, err := r.PerfStats(q)
	if err != nil {
		t.Fatal(err)
	}
	if third.Sampled != 2 {
		t.Fatalf("archive change not invalidated: sampled=%d want 2", third.Sampled)
	}
}

package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, Credit: 1.5, HasCredit: true, ModelRate: "0.05", LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.Credits != 1.5 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 150 || s.Totals.CreditsPer1MTokens != 10000 {
		t.Fatalf("credit totals = %+v, want credits=1.5 samples=1 tokens=150 ratio=10000", s.Totals)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
	if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "uid1" ||
		s.CreditByAccount[0].CreditSamples != 1 || s.CreditByAccount[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_account = %+v, want one uid1 row", s.CreditByAccount)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" ||
		s.CreditByModel[0].Rate != "0.05" || s.CreditByModel[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_model = %+v, want one glm-5.2 rate=0.05 row", s.CreditByModel)
	}
}

// 按 Key 聚合必须把同一账号、同一模型下的不同客户端拆开；匿名/历史桶必须
// 单独成行，不能伪装成某个具体 Key。credits 也只能归到实际携带该观测的 Key。
func TestByKeyDimensions(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{
		PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true,
		Credit: 0.2, HasCredit: true, KeyID: "team-a",
	}, true)
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{
		PromptTokens: 20, HasPromptTokens: true, CompletionTokens: 10, HasCompletion: true,
		Credit: 0.4, HasCredit: true, KeyID: "team-b",
	}, true)
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{
		PromptTokens: 30, HasPromptTokens: true, CompletionTokens: 15, HasCompletion: true,
	}, false)

	s := r.Snapshot(24, nil)
	if len(s.ByKey) != 3 {
		t.Fatalf("by_key=%+v want three rows (team-a, team-b, unattributed)", s.ByKey)
	}
	rows := map[string]KeyedAgg{}
	for _, row := range s.ByKey {
		rows[row.Key] = row
	}
	if rows["team-a"].Requests != 1 || rows["team-a"].TotalTokens != 15 {
		t.Fatalf("team-a=%+v", rows["team-a"])
	}
	if rows["team-b"].Requests != 1 || rows["team-b"].TotalTokens != 30 {
		t.Fatalf("team-b=%+v", rows["team-b"])
	}
	if rows[""].Requests != 1 || rows[""].Errors != 1 {
		t.Fatalf("unattributed=%+v (must stay separate from named keys)", rows[""])
	}
	if len(s.CreditByKey) != 2 {
		t.Fatalf("credit_by_key=%+v want team-a/team-b only", s.CreditByKey)
	}
	for _, row := range s.CreditByKey {
		if row.Key != "team-a" && row.Key != "team-b" {
			t.Fatalf("credit row attributed to unknown key: %+v", row)
		}
	}
}

// TestZeroRecorderAdd 兼容历史零值用法：不调用 New 也能记录，不能因分片
// 初始化为 nil map 而 panic。
func TestZeroRecorderAdd(t *testing.T) {
	var r Recorder
	r.Add(time.Now(), "cn", "u", "m", Delta{}, true)
	if got := r.Snapshot(24, nil).Totals.Requests; got != 1 {
		t.Fatalf("zero recorder requests=%d want 1", got)
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1（窗口外日桶不进聚合）", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want 仅当前小时 1 个点", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("全部历史 totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != fileVersion || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// 版本 1 文件没有积分字段：按零值恢复，旧 Token 数据保持可见且不产生伪比例。
func TestLoadLegacyWithoutCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := `{"version":1,"saved":"2026-09-28T00:00:00+08:00","buckets":[{"s":"h:2026-09-28T10","r":"cn","u":"u1","m":"glm-5.2","q":1,"p":42,"t":42}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(0, nil)
	if s.Totals.TotalTokens != 42 || s.Totals.CreditSamples != 0 || s.Totals.CreditsPer1MTokens != 0 {
		t.Fatalf("legacy totals = %+v, want token-only history", s.Totals)
	}
	if len(s.CreditByAccount) != 0 || len(s.CreditByModel) != 0 {
		t.Fatalf("legacy credit dimensions = %+v / %+v, want none", s.CreditByAccount, s.CreditByModel)
	}
}

// 小时桶折叠为日桶时必须保留积分、样本数和匹配 Token，比例不能因 Rollup 漂移。
func TestCreditSurvivesRollup(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 1.25, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(old.Add(2*time.Hour), "cn", "u", "m", Delta{PromptTokens: 300, HasPromptTokens: true, TotalTokens: 300, HasTotal: true, Credit: 3.75, HasCredit: true, ModelRate: "0.5"}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	if s.Totals.Credits != 5 || s.Totals.CreditSamples != 2 || s.Totals.CreditTokens != 400 || s.Totals.CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit totals = %+v, want credits=5 tokens=400 ratio=12500", s.Totals)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.5" || s.CreditByModel[0].CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit_by_model = %+v, want one rate-preserving row", s.CreditByModel)
	}
}

// 模型维度按“裸模型名 + 生效倍率”合并；同倍率跨账号/时间合并，不同倍率拆行。
func TestCreditDimensionsRateGrouping(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(2*time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 300, HasTotal: true, Credit: 6, HasCredit: true, ModelRate: "0.8"}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByAccount) != 2 || len(s.CreditByModel) != 2 {
		t.Fatalf("dimensions accounts=%+v models=%+v, want 2 accounts and 2 model-rate rows", s.CreditByAccount, s.CreditByModel)
	}
	if s.CreditByModel[0].Key != "glm-5.2" || s.CreditByModel[0].Rate != "0.8" ||
		s.CreditByModel[0].Credits != 6 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("first model row = %+v, want rate=0.8 credits=6 tokens=300", s.CreditByModel[0])
	}
	if s.CreditByModel[1].Rate != "0.5" || s.CreditByModel[1].Credits != 3 || s.CreditByModel[1].CreditTokens != 300 {
		t.Fatalf("merged model row = %+v, want rate=0.5 credits=3 tokens=300", s.CreditByModel[1])
	}
}

// 旧桶缺倍率时由当前目录倍率回填，并与新桶同倍率记录合并；目录缺失时保留未知行。
func TestCreditLegacyRateFallback(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now.Add(time.Hour), "cn", "u1", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.79"}, true)

	s := r.SnapshotWithRates(24, nil, func(realm, model string) string {
		if realm == "cn" && model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.79" ||
		s.CreditByModel[0].Credits != 3 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("fallback model rows = %+v, want legacy merged into rate=0.79", s.CreditByModel)
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/3（48h 前的数据应被过滤）", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want 仅窗口内 1 个小时点", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（窗口内命中桶数）", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("全部历史 totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// 显式区间（「今天」/「自定义」）与滚动窗口走同一套全口径过滤；区间是闭区间
// （桶起点落在 [From, To] 内即命中），且 From/To 会回显给面板确认口径。
func TestSnapshotExplicitWindow(t *testing.T) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := 0; i < 6; i++ {
		r.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u", "m",
			Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	// 只取中间两小时（base+2h、base+3h）。
	s := r.SnapshotWindow(Window{
		From: base.Add(2 * time.Hour),
		To:   base.Add(3 * time.Hour),
	}, nil, nil)
	if s.Totals.Requests != 2 || s.Totals.PromptTokens != 20 {
		t.Fatalf("显式区间 totals = %d/%d, want 2/20", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 2 || s.Buckets != 2 {
		t.Fatalf("显式区间 series/buckets = %d/%d, want 2/2", len(s.Series), s.Buckets)
	}
	if s.WindowFrom == "" || s.WindowTo == "" {
		t.Fatalf("显式区间应回显 window_from/window_to: %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.WindowFrom); err != nil {
		t.Fatalf("window_from 不是 RFC3339: %q", s.WindowFrom)
	}

	// 只有 From（「今天」的形态）：从该点起到最新，全量命中。
	only := r.SnapshotWindow(Window{From: base.Add(4 * time.Hour)}, nil, nil)
	if only.Totals.Requests != 2 {
		t.Fatalf("仅 From 的 totals = %d, want 2", only.Totals.Requests)
	}
	if only.WindowFrom == "" || only.WindowTo != "" {
		t.Fatalf("仅 From 时 window_to 应为空: %+v", only)
	}

	// 空窗口（From/To 全零且 Hours<=0）= 全部历史，与 Snapshot(0) 等价。
	all := r.SnapshotWindow(Window{}, nil, nil)
	if all.Totals.Requests != 6 {
		t.Fatalf("全零窗口 totals = %d, want 6（全部历史）", all.Totals.Requests)
	}
	if all.WindowFrom != "" || all.WindowTo != "" {
		t.Fatalf("全部历史不应回显区间: %+v", all)
	}
}

// 滚动窗口的上限仍是 60 天，且与显式区间互不干扰（From/To 优先）。
func TestWindowBounds(t *testing.T) {
	// From/To 优先于 Hours。
	from := time.Now().Add(-2 * time.Hour)
	gotFrom, gotTo := Window{Hours: 720, From: from}.bounds()
	if !gotFrom.Equal(from) || !gotTo.IsZero() {
		t.Fatalf("From 应优先于 Hours: from=%v to=%v", gotFrom, gotTo)
	}
	// 只有 Hours：起点 = 当前整点往回 Hours-1 小时。
	f, to := Window{Hours: 24}.bounds()
	want := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	if !f.Equal(want) || !to.IsZero() {
		t.Fatalf("24h bounds = %v/%v, want %v/零值", f, to, want)
	}
	// Hours<=0 且无 From/To = 全部历史。
	if f, to := (Window{}).bounds(); !f.IsZero() || !to.IsZero() {
		t.Fatalf("空窗口 bounds = %v/%v, want 零值/零值", f, to)
	}
	// 上限 60 天。
	f60, _ := Window{Hours: 100000}.bounds()
	want60 := time.Now().Truncate(time.Hour).Add(-(24*60 - 1) * time.Hour)
	if !f60.Equal(want60) {
		t.Fatalf("超限 Hours 未被夹到 60 天: %v want %v", f60, want60)
	}
}

// 脏 scope（解析失败）不进任何口径，也不会让整次快照失败。
func TestBucketTimeRejectsGarbage(t *testing.T) {
	if _, ok := bucketTime("h:not-a-time"); ok {
		t.Fatal("脏小时 scope 应判定失败")
	}
	if _, ok := bucketTime("d:2026-13-45"); ok {
		t.Fatal("脏日 scope 应判定失败")
	}
	if ts, ok := bucketTime("h:2026-09-30T13"); !ok || ts.Hour() != 13 {
		t.Fatalf("合法小时 scope 解析失败: %v %v", ts, ok)
	}
	if ts, ok := bucketTime("d:2026-09-30"); !ok || ts.Day() != 30 {
		t.Fatalf("合法日 scope 解析失败: %v %v", ts, ok)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// TestCacheHitRateAccumulation（issue #92）：桶累计 hit/miss，credit_by_model
// 与 totals 输出命中率；无缓存观测的桶不参与（rate 零值省略）。
func TestCacheHitRateAccumulation(t *testing.T) {
	r := New(t.TempDir() + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "glm-5.3", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 1, HasCredit: true, TotalTokens: 1005, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 900, CacheMissTokens: 100,
	}, true)
	r.Add(base.Add(time.Minute), "cn", "u1", "glm-5.3", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
		Credit: 2, HasCredit: true, TotalTokens: 1004, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 100, CacheMissTokens: 900,
	}, true)
	snap := r.Snapshot(0, nil)
	if got := snap.CreditByModel[0].CacheHitRate; got != 50 {
		t.Fatalf("cache_hit_rate=%v want 50", got)
	}
	if snap.Totals.CacheHitRate != 50 {
		t.Fatalf("totals cache_hit_rate=%v want 50", snap.Totals.CacheHitRate)
	}
	if snap.CreditByModel[0].CacheHitTokens != 1000 || snap.CreditByModel[0].CacheMissTokens != 1000 {
		t.Fatalf("hit/miss 累计错误: %+v", snap.CreditByModel[0])
	}

	// 无缓存观测的桶：rate 零值。
	r2 := New(t.TempDir() + "/usage2.json")
	r2.Add(base, "cn", "u1", "m", Delta{PromptTokens: 10, HasPromptTokens: true, Credit: 1, HasCredit: true, TotalTokens: 12, HasTotal: true}, true)
	if got := r2.Snapshot(0, nil).CreditByModel[0].CacheHitRate; got != 0 {
		t.Fatalf("无缓存观测应 rate=0, got %v", got)
	}
}

// TestCacheTokensPersistRoundtrip：ch/cm 落盘 v4 并无损恢复。
func TestCacheTokensPersistRoundtrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir + "/usage.json")
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	r.Add(base, "cn", "u1", "m", Delta{
		Credit: 1, HasCredit: true, TotalTokens: 100, HasTotal: true,
		HasCacheTokens: true, CacheHitTokens: 75, CacheMissTokens: 25,
	}, true)
	r.Save()
	r2 := New(dir + "/usage.json")
	snap := r2.Snapshot(0, nil)
	if snap.CreditByModel[0].CacheHitRate != 75 {
		t.Fatalf("恢复后 cache_hit_rate=%v want 75", snap.CreditByModel[0].CacheHitRate)
	}
}

// TestConcurrentSaveNoTmpCollision：并发 Save 使用唯一临时名，不发生互相截断；
// 最终文件必须仍是完整合法 JSON（旧实现固定 ".tmp" 会让并发写踩 rename 竞态）。
func TestConcurrentSaveNoTmpCollision(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/usage.json"
	r := New(path)
	r.Add(time.Now(), "cn", "u1", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Save()
		}()
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘文件失败: %v", err)
	}
	var f struct {
		Buckets []json.RawMessage `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("并发 Save 后文件损坏（非法 JSON）: %v", err)
	}
	if len(f.Buckets) == 0 {
		t.Fatal("并发 Save 后应仍有桶数据")
	}
}

// BenchmarkSnapshotWindowRealistic 模拟生产形态：470 账号 × 若干个模型 × 24 小时
// 桶，都是窗口内数据。面板 5 秒轮询会反复调用它，用来守住「不复制全量桶、不重复
// 解析 scope」的优化。
func BenchmarkSnapshotWindowRealistic(b *testing.B) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	for a := 0; a < 470; a++ {
		for m := 0; m < 4; m++ {
			for h := 0; h < 24; h++ {
				r.Add(base.Add(time.Duration(h)*time.Hour), "cn", "u"+strconv.Itoa(a), "m"+strconv.Itoa(m),
					Delta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 12, HasTotal: true}, true)
			}
		}
	}
	// 预热一次，避免把「首轮无缓存 + 预热」算进命中路径。
	if s := r.SnapshotWindow(Window{Hours: 24}, nil, nil); s.Totals.Requests == 0 {
		b.Fatal("empty snapshot")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s := r.SnapshotWindow(Window{Hours: 24}, nil, nil); s.Totals.Requests == 0 {
			b.Fatal("empty snapshot")
		}
	}
}

// BenchmarkSnapshotWindowRatesCached 对照 Realistic：面板生产形态会注入
// currentRate 回填旧桶倍率。目录版本未变时必须命中同一缓存，否则每次轮询
// 都会重复聚合数万桶（约 5ms / 312KiB）。
func BenchmarkSnapshotWindowRatesCached(b *testing.B) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	for a := 0; a < 470; a++ {
		for m := 0; m < 4; m++ {
			for h := 0; h < 24; h++ {
				r.Add(base.Add(time.Duration(h)*time.Hour), "cn", "u"+strconv.Itoa(a), "m"+strconv.Itoa(m),
					Delta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 12, HasTotal: true}, true)
			}
		}
	}
	rate := func(realm, model string) string { return "0.5" }
	if s := r.SnapshotWindowRates(Window{Hours: 24}, nil, rate, 1); s.Totals.Requests == 0 {
		b.Fatal("empty snapshot")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if s := r.SnapshotWindowRates(Window{Hours: 24}, nil, rate, 1); s.Totals.Requests == 0 {
			b.Fatal("empty snapshot")
		}
	}
}

// BenchmarkSnapshotWindowCold 关闭缓存命中（每轮换窗口），测量聚合本身的成本。
func BenchmarkSnapshotWindowCold(b *testing.B) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	for a := 0; a < 470; a++ {
		for m := 0; m < 4; m++ {
			for h := 0; h < 24; h++ {
				r.Add(base.Add(time.Duration(h)*time.Hour), "cn", "u"+strconv.Itoa(a), "m"+strconv.Itoa(m),
					Delta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 12, HasTotal: true}, true)
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 显式区间随 i 改变，保证每次都是未命中缓存的新窗口。
		from := base.Add(time.Duration(i%24) * time.Hour)
		to := from.Add(24 * time.Hour)
		if s := r.SnapshotWindow(Window{From: from, To: to}, nil, nil); s.Totals.Requests == 0 {
			b.Fatal("empty snapshot")
		}
	}
}

// TestSnapshotDirtyScopeParsedOnce 回归：缓存命中不能用 tbt.ok 判断。
// 脏 scope 的解析结果 ok=false，也必须缓存，否则每个脏桶每轮都会重复解析。
func TestSnapshotDirtyScopeParsedOnce(t *testing.T) {
	r := New("")
	now := time.Now().Truncate(time.Hour)
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 1, HasTotal: true}, true)
	// 直接注入脏 scope 桶，模拟历史文件中的异常数据。
	sh := r.shardFor("dirty")
	sh.mu.Lock()
	sh.buckets["dirty"] = &bucket{Scope: "dirty", Realm: "cn", UID: "bad", Model: "m", TT: 1, Req: 1}
	sh.mu.Unlock()
	// 两轮窗口快照都不能把脏桶计入，且第二轮仍应正常返回。
	for i := 0; i < 2; i++ {
		s := r.SnapshotWindow(Window{Hours: 1}, nil, nil)
		if s.Totals.Requests == 0 {
			t.Fatal("valid bucket should still aggregate")
		}
		if s.Totals.TotalTokens != 1 {
			t.Fatalf("round %d totals = %+v, dirty scope must be excluded", i, s.Totals)
		}
	}
}

// TestSnapshotConcurrentWithAdd 快照与记账并发：读锁聚合不能丢样本，也不能让
// 快照看到半更新（-race 下同时验证锁类型正确）。
func TestSnapshotConcurrentWithAdd(t *testing.T) {
	r := New("")
	const workers, per = 4, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				r.Add(time.Now(), "cn", "u"+strconv.Itoa(w), "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
			}
		}(w)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				r.SnapshotWindow(Window{Hours: 1}, nil, nil)
			}
		}
	}()
	wg.Wait()
	close(done)
	s := r.SnapshotWindow(Window{Hours: 1}, nil, nil)
	if got, want := s.Totals.Requests, int64(workers*per); got != want {
		t.Fatalf("requests=%d want %d", got, want)
	}
}

// TestAddLatencyDuringSnapshot 验证分片锁的核心收益：持续快照时，记账仍能
// 进入其它分片，而不是整段聚合期间排队。断言用宽松上界，避免 CI 抖动。
func TestAddLatencyDuringSnapshot(t *testing.T) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	for a := 0; a < 120; a++ {
		for m := 0; m < 4; m++ {
			for h := 0; h < 24; h++ {
				r.Add(base.Add(time.Duration(h)*time.Hour), "cn", "u"+strconv.Itoa(a), "m"+strconv.Itoa(m),
					Delta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 12, HasTotal: true}, true)
			}
		}
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.SnapshotWindow(Window{Hours: 24}, nil, nil)
			}
		}
	}()
	start := time.Now()
	for i := 0; i < 2000; i++ {
		r.Add(time.Now(), "cn", "u"+strconv.Itoa(i%40), "m0", Delta{}, true)
	}
	elapsed := time.Since(start)
	close(stop)
	wg.Wait()
	if elapsed > 2*time.Second {
		t.Fatalf("2000 Adds during snapshots took %v; snapshot must not hold the whole table lock", elapsed)
	}
}

// 面板聚合缓存：同一窗口在 TTL 内复用；任意 Add 后下一次读取必须
// 立即看到新数据，不能被 TTL 掩盖（缓存版本检查保证这一点）。
func TestSnapshotCacheInvalidatedByAdd(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	r.SnapshotWindow(Window{Hours: 1}, nil, nil)
	second := r.SnapshotWindow(Window{Hours: 1}, nil, nil)
	if second.version == 0 {
		t.Fatalf("second snapshot was not published to cache: version=%d", second.version)
	}
	third := r.SnapshotWindow(Window{Hours: 1}, nil, nil)
	if third.version != second.version {
		t.Fatalf("cached snapshot version changed: second=%d third=%d", second.version, third.version)
	}
	r.Add(now, "cn", "u1", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	fourth := r.SnapshotWindow(Window{Hours: 1}, nil, nil)
	if fourth.Totals.Requests != 2 || fourth.Totals.PromptTokens != 15 {
		t.Fatalf("add did not invalidate snapshot cache: %+v", fourth.Totals)
	}
}

// 并发同窗口查询走全局单飞：结果一致且不会把不同窗口的数据串到一起。
func TestSnapshotCacheConcurrentSameWindow(t *testing.T) {
	r := New("")
	now := time.Now()
	for i := 0; i < 50; i++ {
		r.Add(now, "cn", "u"+strconv.Itoa(i%5), "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	}
	const workers = 16
	got := make(chan int64, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got <- r.SnapshotWindow(Window{Hours: 1}, nil, nil).Totals.Requests
		}()
	}
	wg.Wait()
	close(got)
	for n := range got {
		if n != 50 {
			t.Fatalf("concurrent snapshot requests=%d want 50", n)
		}
	}
}

// 带倍率回填的快照在目录版本不变时应命中缓存；目录刷新（版本变化）后
// 必须重新聚合并采用新倍率，不能把旧价格固化成 2 秒假数据。
func TestSnapshotRateRevisionControlsCache(t *testing.T) {
	rate := "0.5"
	rateCalls := 0
	cb := func(realm, model string) string { rateCalls++; return rate }

	grid := time.Now().Truncate(time.Hour)
	r := New("")
	now := grid.Add(5 * time.Hour)
	r.Add(now, "cn", "u1", "m", Delta{
		Credit: 1, HasCredit: true, TotalTokens: 100, HasTotal: true,
	}, true)
	first := r.SnapshotWindowRates(Window{Hours: 1}, nil, cb, 1)
	if len(first.CreditByModel) != 1 || first.CreditByModel[0].Rate != "0.5" {
		t.Fatalf("first snapshot = %+v", first.CreditByModel)
	}
	second := r.SnapshotWindowRates(Window{Hours: 1}, nil, cb, 1)
	if rateCalls != 1 {
		t.Fatalf("same rate revision rebuilt snapshot: callback calls=%d want 1", rateCalls)
	}
	if second.Totals.Requests != first.Totals.Requests || len(second.CreditByModel) != len(first.CreditByModel) {
		t.Fatalf("cached rate snapshot differs: first=%+v second=%+v", first, second)
	}

	rate = "0.8"
	third := r.SnapshotWindowRates(Window{Hours: 1}, nil, cb, 2)
	if rateCalls != 2 {
		t.Fatalf("rate revision change did not rebuild: calls=%d", rateCalls)
	}
	if len(third.CreditByModel) != 1 || third.CreditByModel[0].Rate != "0.8" {
		t.Fatalf("new rate snapshot = %+v, want 0.8", third.CreditByModel)
	}
}

// 无倍率版本时保持旧保守语义：不从缓存读取，也不发布缓存，避免未知倍率
// 的结果在目录刷新后被误用。
func TestSnapshotWithoutRateRevisionStaysUncached(t *testing.T) {
	r := New("")
	r.Add(time.Now(), "cn", "u1", "m", Delta{
		Credit: 1, HasCredit: true, TotalTokens: 10, HasTotal: true,
	}, true)
	calls := 0
	cb := func(realm, model string) string { calls++; return "" }
	r.SnapshotWindowRates(Window{Hours: 1}, nil, cb, 0)
	r.SnapshotWindowRates(Window{Hours: 1}, nil, cb, 0)
	if calls != 2 {
		t.Fatalf("uncached callback calls=%d want 2", calls)
	}
}

// TestSnapshotFlightPerWindow 单飞必须按窗口键隔离：不同窗口可以并行聚合，
// 不能因一个大窗口正在计算就阻塞小时窗口；相同窗口的并发请求仍只跑一次。
func TestSnapshotFlightPerWindow(t *testing.T) {
	r := New("")
	now := time.Now().Truncate(time.Hour)
	for i := 0; i < 8; i++ {
		r.Add(now, "cn", "u"+strconv.Itoa(i), "m", Delta{
			Credit: 1, HasCredit: true, TotalTokens: 100, HasTotal: true,
		}, true)
	}

	var callsMu sync.Mutex
	calls := 0
	started := make(chan string, 2)
	release := make(chan struct{})
	callback := func(realm, model string) string {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		started <- realm
		<-release
		return "0.5"
	}

	type result struct {
		hours int
		snap  Snapshot
	}
	done := make(chan result, 2)
	for _, hours := range []int{1, 24} {
		hours := hours
		go func() {
			done <- result{hours: hours, snap: r.SnapshotWindowRates(Window{Hours: hours}, nil, callback, 9)}
		}()
	}

	// 两个窗口都必须进入聚合（各自调用一次倍率回调），证明它们没有被全局锁串行。
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("different windows were serialized: only %d/2 entered aggregation", i)
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case got := <-done:
			if got.snap.Totals.Requests != 8 {
				t.Fatalf("window %dh requests=%d want 8", got.hours, got.snap.Totals.Requests)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("snapshot did not finish")
		}
	}

	// 同窗口再并发请求：缓存仍有效时直接命中；清掉缓存后同键只允许一个 leader。
	r.snapMu.Lock()
	r.snapCache = nil
	r.snapMu.Unlock()
	enter := make(chan struct{}, 4)
	finish := make(chan struct{})
	blocked := func(realm, model string) string {
		enter <- struct{}{}
		<-finish
		return "0.5"
	}
	for i := 0; i < 4; i++ {
		go func() { r.SnapshotWindowRates(Window{Hours: 2}, nil, blocked, 11) }()
	}
	select {
	case <-enter:
	case <-time.After(2 * time.Second):
		t.Fatal("same-window leader did not start")
	}
	select {
	case <-enter:
		t.Fatal("same-window requests must share one aggregation")
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	time.Sleep(20 * time.Millisecond)
}

// TestRollupConcurrentWithAddAndSnapshot 折叠与记账/快照并发：折叠会跨分片
// 移动桶，必须保证不丢失、不重复、不死锁。用 200ms 超时捕获 ABBA 自锁。
func TestRollupConcurrentWithAddAndSnapshot(t *testing.T) {
	r := New("")
	// Add 使用当前小时，必须让 Rollup 折叠这些桶才能制造跨分片移动；
	// 若用 100 天前的时间，桶不会移动，测试就失去意义。
	old := time.Now().Truncate(time.Hour).Add(-100 * 24 * time.Hour)
	for i := 0; i < 64; i++ {
		r.Add(old.Add(time.Duration(i)*time.Minute), "cn", "u"+strconv.Itoa(i%8), "m"+strconv.Itoa(i%4),
			Delta{PromptTokens: 1, HasPromptTokens: true}, true)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			r.SnapshotWindow(Window{}, nil, nil)
		}
		close(done)
	}()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r.Add(time.Now(), "cn", "u"+strconv.Itoa(i%8), "m"+strconv.Itoa(i%4),
				Delta{PromptTokens: 1, HasPromptTokens: true}, true)
		}
	}()
	rollDone := make(chan struct{})
	go func() {
		r.Rollup(time.Now())
		close(rollDone)
	}()
	select {
	case <-rollDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Rollup deadlocked with Add/Snapshot")
	}
	wg.Wait()
	<-done
	// 时钟可能刚好跨整点：再次折叠当前小时之外的桶，验证日桶没有重复。
	r.Rollup(time.Now().Add(time.Hour))
	s := r.SnapshotWindow(Window{}, nil, nil)
	if s.Totals.Requests != 64+200 {
		t.Fatalf("requests=%d want 264 (no lost/duplicated buckets)", s.Totals.Requests)
	}
}

// TestRollupFlushNoLostBuckets 折叠与落盘并发后必须能从文件完整恢复；
// 这是跨分片迁移与 flush 交错时最容易出现的「两边都没拷到」回归。
func TestRollupFlushNoLostBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	old := time.Now().Truncate(time.Hour).Add(-100 * 24 * time.Hour)
	const n = 120
	for i := 0; i < n; i++ {
		r.Add(old.Add(time.Duration(i)*time.Minute), "cn", "u"+strconv.Itoa(i), "m", Delta{TotalTokens: 1, HasTotal: true}, true)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Save()
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.Rollup(time.Now())
	}()
	wg.Wait()
	r.Save()

	restored := New(path).SnapshotWindow(Window{}, nil, nil)
	if restored.Totals.Requests != n || restored.Totals.TotalTokens != n {
		t.Fatalf("restored totals=%+v want requests=%d tokens=%d (buckets lost during rollup/flush)", restored.Totals, n, n)
	}
}

// TestFlushWritesAllShards 非 force 定时落盘也必须写全部分片。dirty 仅控制
// 是否发生写盘，不能把快照裁剪成「本次变更的分片」——文件是整体 replace。
func TestFlushWritesAllShards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	now := time.Now()
	for i := 0; i < 200; i++ {
		r.Add(now, "cn", "u"+strconv.Itoa(i), "m", Delta{}, true)
	}
	r.flush(false)
	saved := New(path).SnapshotWindow(Window{}, nil, nil)
	if saved.Totals.Requests != 200 {
		t.Fatalf("non-forced flush persisted %d requests, want 200 (clean shards dropped)", saved.Totals.Requests)
	}

	r.Add(now, "cn", "u0", "m", Delta{}, true)
	r.flush(false)
	again := New(path).SnapshotWindow(Window{}, nil, nil)
	if again.Totals.Requests != 201 {
		t.Fatalf("incremental flush persisted %d requests, want 201", again.Totals.Requests)
	}
}

// 落盘失败必须保留 dirty，下一轮自动重试；旧实现先清 dirty，失败后静默丢用量。
func TestUsageFlushFailureKeepsDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	r := New(path)
	r.Add(time.Now(), "cn", "u1", "m", Delta{TotalTokens: 7, HasTotal: true}, true)

	// 把目标路径变成目录，让 rename 必定失败（CreateTemp 在同目录仍可成功）。
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	r.flush(false)
	dirty := 0
	for i := range r.shards {
		r.shards[i].mu.RLock()
		if r.shards[i].dirty {
			dirty++
		}
		r.shards[i].mu.RUnlock()
	}
	if dirty == 0 {
		t.Fatal("flush failure cleared dirty; data will never retry")
	}

	// 恢复可写并再 flush 一次，必须真正落盘。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r.flush(false)
	saved := New(path).SnapshotWindow(Window{}, nil, nil)
	if saved.Totals.TotalTokens != 7 {
		t.Fatalf("retry persisted tokens=%d want 7", saved.Totals.TotalTokens)
	}
}

// 复制快照后、成功清脏前并发 Add 的分片必须继续保留 dirty。旧实现无条件在
// rename 成功后清 dirty，会把「写盘期间新增的用量」标记一并抹掉：若之后没有
// 新请求，这笔量要等重启前最后一次写盘才可能落下，定时落盘路径则会漏掉它。
func TestUsageFlushNoLostDirtyDuringWrite(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m", Delta{TotalTokens: 1, HasTotal: true}, true)

	var snap file
	_, copied := r.copyForFlush(&snap, false)

	// 模拟 json.Marshal/Sync/Rename 期间到达的新请求。
	r.Add(now, "cn", "u1", "m", Delta{TotalTokens: 2, HasTotal: true}, true)
	r.clearFlushed(copied)

	dirty := false
	for i := range r.shards {
		r.shards[i].mu.RLock()
		if r.shards[i].dirty {
			dirty = true
		}
		r.shards[i].mu.RUnlock()
	}
	if !dirty {
		t.Fatal("flush cleared dirty for a shard modified after snapshot; new usage would be lost")
	}

	// 下一轮正常 flush 必须把两笔一起写进去。
	path := filepath.Join(t.TempDir(), "usage.json")
	r.path = path
	r.flush(false)
	saved := New(path).SnapshotWindow(Window{}, nil, nil)
	if saved.Totals.TotalTokens != 3 {
		t.Fatalf("persisted tokens=%d want 3 (first snapshot + concurrent add)", saved.Totals.TotalTokens)
	}
}

// TestSnapshotCreditByRealmAndSeries 积分必须能按域聚合、也能从时序里读到：
// 前端「积分」曲线与「按域」表都直接依赖这两个字段，此前后端完全没有
// credit_by_realm，series 里的 credits 也从未被任何测试覆盖。
func TestSnapshotCreditByRealmAndSeries(t *testing.T) {
	r := New("")
	now := time.Now().Add(-2 * time.Hour) // 落在当前小时桶内即可（同一窗口）
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{
		PromptTokens: 600, HasPromptTokens: true, CompletionTokens: 400, HasCompletion: true,
		Credit: 3, HasCredit: true, ModelRate: "0.05",
	}, true)
	r.Add(now, "global", "uid2", "claude-4.6", Delta{
		PromptTokens: 500, HasPromptTokens: true, CompletionTokens: 500, HasCompletion: true,
		Credit: 7, HasCredit: true, ModelRate: "0.1",
	}, true)
	// 没有 credit 观测的请求（同一倍率桶）：只计入请求数与 token，不污染积分。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{
		PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 100, HasCompletion: true,
		ModelRate: "0.05",
	}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByRealm) != 2 {
		t.Fatalf("credit_by_realm=%+v, want 2 realms", s.CreditByRealm)
	}
	byRealm := map[string]CreditAgg{}
	for _, row := range s.CreditByRealm {
		byRealm[row.Key] = row
	}
	if got := byRealm["cn"]; got.Credits != 3 || got.CreditRequests != 1 {
		t.Errorf("cn credit=%v credit_requests=%d, want 3/1", got.Credits, got.CreditRequests)
	}
	if got := byRealm["global"]; got.Credits != 7 || got.CreditRequests != 1 {
		t.Errorf("global credit=%v credit_requests=%d, want 7/1", got.Credits, got.CreditRequests)
	}
	// CreditAgg.Requests 含无积分观测的请求（cn 共 2 次），CreditRequests 只算
	// 真正带 credit 的那次——两者相减就是覆盖率的分母/分子，不能混用。
	if got := byRealm["cn"]; got.Requests != 2 {
		t.Errorf("cn requests=%d, want 2（含无 credit 观测的那次）", got.Requests)
	}

	// 时序点必须带积分：这是「积分消耗趋势」图的唯一数据源。
	var credSum float64
	pts := 0
	for _, p := range s.Series {
		credSum += p.Credits
		pts++
	}
	if pts == 0 {
		t.Fatal("series should not be empty")
	}
	if credSum != 10 {
		t.Errorf("series credits sum=%v, want 10", credSum)
	}
}

// TestCreditByKeyAndAccountCarryCoverage credit_requests 必须按维度正确填充，
// 前端用它与 requests 计算覆盖率，否则「0 消耗」与「无观测」无法区分。
func TestCreditByKeyAndAccountCarryCoverage(t *testing.T) {
	r := New("")
	now := time.Now()
	for i := 0; i < 3; i++ {
		r.Add(now, "cn", "uid1", "glm-5.2", Delta{
			PromptTokens: 10, HasPromptTokens: true, TotalTokens: 20, HasTotal: true,
			Credit: 1, HasCredit: true, KeyID: "k1",
		}, true)
	}
	// 同一 Key、同一账号，但上游没回 credit。
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{
		PromptTokens: 10, HasPromptTokens: true, TotalTokens: 20, HasTotal: true, KeyID: "k1",
	}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByAccount) != 1 {
		t.Fatalf("credit_by_account=%+v", s.CreditByAccount)
	}
	row := s.CreditByAccount[0]
	if row.Requests != 4 || row.CreditRequests != 3 {
		t.Fatalf("requests=%d credit_requests=%d, want 4/3", row.Requests, row.CreditRequests)
	}
	if row.Credits != 3 {
		t.Fatalf("credits=%v want 3", row.Credits)
	}
	// 按 Key 同口径。
	if len(s.CreditByKey) != 1 || s.CreditByKey[0].CreditRequests != 3 || s.CreditByKey[0].Requests != 4 {
		t.Fatalf("credit_by_key=%+v", s.CreditByKey)
	}
	// by_account 明细行也带同样的积分字段（前端明细表直接渲染这些列）。
	if len(s.ByAccount) != 1 || s.ByAccount[0].CreditSamples != 3 || s.ByAccount[0].Credits != 3 {
		t.Fatalf("by_account row must carry credit fields: %+v", s.ByAccount)
	}
}

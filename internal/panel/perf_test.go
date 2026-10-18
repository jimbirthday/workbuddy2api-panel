package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

func perfPanelRecorder(t *testing.T, events []reqlog.Event) *reqlog.Recorder {
	t.Helper()
	r := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	for _, e := range events {
		r.Record(e)
	}
	r.Close()
	return r
}

func perfPanelGet(t *testing.T, p *Panel, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	var out map[string]any
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, rr.Body)
		}
	}
	return rr, out
}

// 未装配 RequestLog → 501；未带 Bearer → 401。不能把不可用接口伪装成空数据。
func TestPerfUsageAvailabilityAndAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	rr, _ := perfPanelGet(t, p, "/panel/api/usage/perf")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("no recorder -> %d %s", rr.Code, rr.Body)
	}
	rec := perfPanelRecorder(t, nil)
	p = New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/panel/api/usage/perf", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth -> %d %s", rr.Code, rr.Body)
	}
}

// 空归档必须回稳定空结构（而非 null），并回显默认时间窗。
func TestPerfUsageEmptyArchive(t *testing.T) {
	rec := perfPanelRecorder(t, nil)
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	rr, out := perfPanelGet(t, p, "/panel/api/usage/perf")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if out["sampled"].(float64) != 0 {
		t.Fatalf("sampled = %v", out["sampled"])
	}
	if _, ok := out["window_from"]; !ok {
		t.Fatalf("missing window_from: %+v", out)
	}
	for _, key := range []string{"by_model", "by_model_realm", "by_model_acct"} {
		m, ok := out[key].(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("%s should be empty object, got %#v", key, out[key])
		}
	}
	if out["ttft_ms"] != nil || out["wall_ms"] != nil {
		t.Fatalf("empty blocks must be null: %+v", out)
	}
}

// from/to 必须真正作用到聚合；model/account/realm 过滤沿用日志页口径。
func TestPerfUsageFilters(t *testing.T) {
	base := time.Now().Truncate(time.Second).Add(-time.Hour)
	events := []reqlog.Event{
		{Time: base, Model: "cn:glm-5.3", Account: "alice(12345678)", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 100, TTFBMs: 10, CompletionTokens: 10},
		{Time: base.Add(10 * time.Minute), Model: "global:kimi-k3", Account: "bob(87654321)", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 200, TTFBMs: 20, CompletionTokens: 10},
		{Time: base.Add(20 * time.Minute), Model: "glm-5.3", Account: "alice(12345678)", Status: 502, OK: false, Outcome: reqlog.OutcomeHTTPError, DurationMs: 300},
	}
	rec := perfPanelRecorder(t, events)
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})

	from := strconv.FormatInt(base.Add(-time.Minute).Unix(), 10)
	to := strconv.FormatInt(base.Add(15*time.Minute).Unix(), 10)
	rr, out := perfPanelGet(t, p, "/panel/api/usage/perf?from="+from+"&to="+to)
	if rr.Code != http.StatusOK || out["sampled"].(float64) != 2 {
		t.Fatalf("time filter code=%d out=%+v", rr.Code, out)
	}

	rr, out = perfPanelGet(t, p, "/panel/api/usage/perf?model=KIMI&realm=global")
	if rr.Code != http.StatusOK || out["sampled"].(float64) != 1 {
		t.Fatalf("model/realm filter code=%d out=%+v", rr.Code, out)
	}
	byModel := out["by_model"].(map[string]any)
	if _, ok := byModel["kimi-k3"]; !ok {
		t.Fatalf("by_model should use bare model key: %+v", byModel)
	}

	rr, out = perfPanelGet(t, p, "/panel/api/usage/perf?account=alice")
	if rr.Code != http.StatusOK || out["sampled"].(float64) != 2 || out["errors"].(float64) != 1 {
		t.Fatalf("account filter code=%d out=%+v", rr.Code, out)
	}
}

// hours/samples/realm 的非法值应 400；超限值按服务端上限收敛，且响应回显
// 实际 sample_limit，避免调用方误以为拿到了无限样本。
// key_id 筛选必须与 prompt 的归档 key_id 对齐；空 key_id 的历史事件不参与过滤。
func TestPerfUsageFiltersByKeyID(t *testing.T) {
	base := time.Now().Truncate(time.Second).Add(-time.Hour)
	events := []reqlog.Event{
		{Time: base, Model: "glm-5.3", KeyID: "team-a", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 100},
		{Time: base.Add(time.Minute), Model: "glm-5.3", KeyID: "team-b", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 200},
		{Time: base.Add(2 * time.Minute), Model: "glm-5.3", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 300},
	}
	rec := perfPanelRecorder(t, events)
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	rr, out := perfPanelGet(t, p, "/panel/api/usage/perf?key_id=TEAM-A")
	if rr.Code != http.StatusOK || out["sampled"].(float64) != 1 || out["requests"].(float64) != 1 {
		t.Fatalf("key_id filter code=%d out=%+v", rr.Code, out)
	}
}

func TestPerfUsageParameterBounds(t *testing.T) {
	rec := perfPanelRecorder(t, nil)
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	for _, path := range []string{
		"/panel/api/usage/perf?hours=abc",
		"/panel/api/usage/perf?hours=-1",
		"/panel/api/usage/perf?samples=-1",
		"/panel/api/usage/perf?realm=eu",
	} {
		rr, _ := perfPanelGet(t, p, path)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d %s", path, rr.Code, rr.Body)
		}
	}

	rr, out := perfPanelGet(t, p, "/panel/api/usage/perf?hours="+strconv.Itoa(reqlog.MaxPerfHours+1000)+"&samples="+strconv.Itoa(reqlog.MaxPerfSamples+1000))
	if rr.Code != http.StatusOK {
		t.Fatalf("clamped params -> %d %s", rr.Code, rr.Body)
	}
	if int(out["sample_limit"].(float64)) != reqlog.MaxPerfSamples {
		t.Fatalf("sample_limit = %v, want %d", out["sample_limit"], reqlog.MaxPerfSamples)
	}
	from, _ := time.Parse(time.RFC3339, out["window_from"].(string))
	to, _ := time.Parse(time.RFC3339, out["window_to"].(string))
	if to.Sub(from) > reqlog.MaxPerfHours*time.Hour {
		t.Fatalf("window = %v..%v exceeds cap", from, to)
	}

	// 用量页「全部历史」显式发 hours=0：必须按服务端最大窗口返回 200，
	// 而不是让性能卡稳定报 400。
	rr, out = perfPanelGet(t, p, "/panel/api/usage/perf?hours=0")
	if rr.Code != http.StatusOK {
		t.Fatalf("hours=0 -> %d %s", rr.Code, rr.Body)
	}
	from, _ = time.Parse(time.RFC3339, out["window_from"].(string))
	to, _ = time.Parse(time.RFC3339, out["window_to"].(string))
	if got := to.Sub(from); got < reqlog.MaxPerfHours*time.Hour-time.Minute || got > reqlog.MaxPerfHours*time.Hour+time.Minute {
		t.Fatalf("hours=0 window = %v, want max %v", got, reqlog.MaxPerfHours*time.Hour)
	}
}

// 裸模型必须通过池内脱敏标签映射到 global；映射不能依赖完整 Pool.List 的
// 展示字段，否则 470 账号的性能查询会为每个账号构建整份 Status。
func TestPerfUsageBareModelRealmUsesPoolLabels(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	rec := perfPanelRecorder(t, []reqlog.Event{{
		Time: base, Model: "kimi-k3", Account: "alice(12345678)",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		DurationMs: 100, TTFBMs: 10, CompletionTokens: 10,
	}})
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "12345678-aaaa-bbbb", Nickname: "alice", Domain: "www.workbuddy.ai"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, RequestLog: rec})
	rr, out := perfPanelGet(t, p, "/panel/api/usage/perf?realm=global")
	if rr.Code != http.StatusOK || out["sampled"].(float64) != 1 {
		t.Fatalf("realm mapping code=%d out=%+v", rr.Code, out)
	}
}

// 响应与错误不得泄露凭证、调用来源或归档路径。账号键是既有的脱敏展示标签
// （昵称(uid8)），与运行日志页一致；完整 UID 从不进入归档。
func TestPerfUsageResponseDoesNotLeakSecrets(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	rec := perfPanelRecorder(t, []reqlog.Event{{
		Time: base, Model: "cn:glm-5.3", Account: "alice(12345678)",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		DurationMs: 100, TTFBMs: 10, CompletionTokens: 10,
		ClientIP: "203.0.113.7", UserAgent: "secret-client/1.0",
	}})
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	rr, _ := perfPanelGet(t, p, "/panel/api/usage/perf")
	body := rr.Body.String()
	for _, secret := range []string{"203.0.113.7", "secret-client", "Authorization", "access_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "alice(12345678)") {
		t.Fatalf("masked account label missing: %s", body)
	}
	if strings.Contains(body, "12345678-aaaa-bbbb-cccc-1234567890ab") {
		t.Fatalf("response leaked full uid: %s", body)
	}
}

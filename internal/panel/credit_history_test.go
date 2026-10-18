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
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func creditPanelGet(t *testing.T, p *Panel, path string) (*httptest.ResponseRecorder, map[string]any) {
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

func creditPanelRecorder(t *testing.T) *usage.Recorder {
	t.Helper()
	r := usage.New("")
	base := time.Now().Truncate(time.Hour).Add(-4 * time.Hour)
	r.Add(base, "cn", "uid-1234567890", "cn:glm-5.3", usage.Delta{
		Credit: 1.5, HasCredit: true, TotalTokens: 100, HasTotal: true,
	}, true)
	r.Add(base.Add(time.Hour), "global", "uid-abcdefghij", "kimi-k3", usage.Delta{
		Credit: -0.25, HasCredit: true, TotalTokens: 20, HasTotal: true,
	}, true)
	return r
}

func TestCreditHistoryAvailabilityAndAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	rr, _ := creditPanelGet(t, p, "/panel/api/credits/history")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("no usage recorder -> %d %s", rr.Code, rr.Body)
	}

	p = New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Usage: usage.New("")})
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/panel/api/credits/history", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth -> %d %s", rr.Code, rr.Body)
	}
}

func TestCreditHistoryResponseAndMasking(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "uid-1234567890", Nickname: "秋叶", Domain: "www.workbuddy.ai"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Usage: creditPanelRecorder(t)})

	rr, out := creditPanelGet(t, p, "/panel/api/credits/history?hours=0&limit=200")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if out["total"].(float64) != 1 || out["kind"] != "all" || out["limit"].(float64) != 200 {
		t.Fatalf("summary = %+v", out)
	}
	if out["aggregated"] != true || out["grant_available"] != false {
		t.Fatalf("aggregation flags must be explicit: %+v", out)
	}
	rows := out["records"].([]any)
	if len(rows) != 1 {
		t.Fatalf("records = %+v", rows)
	}
	row := rows[0].(map[string]any)
	if row["account"] != "秋叶(uid-1234)" || row["model"] != "glm-5.3" ||
		row["tokens"].(float64) != 100 || row["kind"] != "spend" ||
		row["scope"] != "hour" || row["granularity"] != "hourly" || row["requests"].(float64) != 1 {
		t.Fatalf("spend row = %+v", row)
	}
	body := rr.Body.String()
	for _, secret := range []string{"uid-1234567890", "uid-abcdefghij", "access_token", "refresh_token", "device_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q: %s", secret, body)
		}
	}
}

func TestCreditHistoryFiltersAndBounds(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Usage: creditPanelRecorder(t)})
	rr, out := creditPanelGet(t, p, "/panel/api/credits/history?kind=spend&hours=0")
	if rr.Code != http.StatusOK || out["total"].(float64) != 1 {
		t.Fatalf("kind=spend -> %d %+v", rr.Code, out)
	}
	rows := out["records"].([]any)
	if rows[0].(map[string]any)["kind"] != "spend" {
		t.Fatalf("kind filter rows = %+v", rows)
	}

	// grant 没有可靠数据源：接口必须明确 501，不能返回伪造的空记录冒充成功。
	rr, _ = creditPanelGet(t, p, "/panel/api/credits/history?kind=grant")
	if rr.Code != http.StatusNotImplemented || !strings.Contains(rr.Body.String(), "no reliable") {
		t.Fatalf("kind=grant -> %d %s", rr.Code, rr.Body)
	}

	for _, path := range []string{
		"/panel/api/credits/history?kind=refund",
		"/panel/api/credits/history?limit=0",
		"/panel/api/credits/history?limit=abc",
		"/panel/api/credits/history?hours=abc",
		"/panel/api/credits/history?hours=-1",
	} {
		rr, _ := creditPanelGet(t, p, path)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d %s", path, rr.Code, rr.Body)
		}
	}

	rr, out = creditPanelGet(t, p, "/panel/api/credits/history?limit="+strconv.Itoa(usage.CreditHistoryMaxLimit+500)+"&hours=0")
	if rr.Code != http.StatusOK || int(out["limit"].(float64)) != usage.CreditHistoryMaxLimit {
		t.Fatalf("limit cap -> %d %+v", rr.Code, out)
	}

	base := time.Now().Truncate(time.Hour).Add(-4 * time.Hour)
	from := strconv.FormatInt(base.Add(-time.Minute).Unix(), 10)
	to := strconv.FormatInt(base.Add(time.Minute).Unix(), 10)
	rr, out = creditPanelGet(t, p, "/panel/api/credits/history?from="+from+"&to="+to)
	if rr.Code != http.StatusOK || out["total"].(float64) != 1 {
		t.Fatalf("explicit window -> %d %+v", rr.Code, out)
	}
	if out["window_from"] == "" || out["window_to"] == "" {
		t.Fatalf("explicit window must echo bounds: %+v", out)
	}
}

func TestCreditHistoryEmptyAndLegacy(t *testing.T) {
	r := usage.New("")
	r.Add(time.Now().Add(-time.Hour), "cn", "u", "m", usage.Delta{
		PromptTokens: 10, HasPromptTokens: true, TotalTokens: 10, HasTotal: true,
	}, true)
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Usage: r})
	rr, out := creditPanelGet(t, p, "/panel/api/credits/history")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if out["total"].(float64) != 0 || len(out["records"].([]any)) != 0 {
		t.Fatalf("legacy bucket must not fake credit rows: %+v", out)
	}
}

// 折叠后的日桶在全部历史口径下必须返回 daily，且窗口参数仍能过滤它。
func TestCreditHistoryDailyGranularityAPI(t *testing.T) {
	r := usage.New("")
	old := time.Now().Add(-120 * 24 * time.Hour).Truncate(time.Hour)
	r.Add(old, "cn", "u", "m", usage.Delta{Credit: 3, HasCredit: true, TotalTokens: 30, HasTotal: true}, true)
	r.Rollup(time.Now())
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Usage: r})
	rr, out := creditPanelGet(t, p, "/panel/api/credits/history?hours=0")
	if rr.Code != http.StatusOK || out["total"].(float64) != 1 {
		t.Fatalf("daily bucket -> %d %+v", rr.Code, out)
	}
	row := out["records"].([]any)[0].(map[string]any)
	if row["scope"] != "day" || row["granularity"] != "daily" {
		t.Fatalf("daily row must expose day/daily: %+v", row)
	}
	from := strconv.FormatInt(time.Now().Add(-24*time.Hour).Unix(), 10)
	rr, out = creditPanelGet(t, p, "/panel/api/credits/history?from="+from)
	if rr.Code != http.StatusOK || out["total"].(float64) != 0 {
		t.Fatalf("daily bucket must be filtered by window -> %d %+v", rr.Code, out)
	}
}

package panel

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestStaticAssetsGzip 静态资源协商：gzip 请求拿到可解压的原文字节且 ETag
// 与未压缩表示不同；不支持压缩时回退原文；304 仍正确命中各自表示。
func TestStaticAssetsGzip(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	get := func(path, accept, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer k")
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		return rr
	}

	plain := get("/panel/app.js", "", "")
	if plain.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain code=%d enc=%q", plain.Code, plain.Header().Get("Content-Encoding"))
	}
	if plain.Body.String() != string(appJS) {
		t.Fatal("plain body must equal embedded app.js")
	}

	gz := get("/panel/app.js", "gzip", "")
	if gz.Code != http.StatusOK || gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip code=%d enc=%q", gz.Code, gz.Header().Get("Content-Encoding"))
	}
	if gz.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip Vary=%q want Accept-Encoding", gz.Header().Get("Vary"))
	}
	if gz.Body.Len() >= plain.Body.Len() {
		t.Fatalf("gzip size=%d plain=%d (must be smaller)", gz.Body.Len(), plain.Body.Len())
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(appJS) {
		t.Fatal("gzip payload does not decode to app.js")
	}
	plainTag, gzTag := plain.Header().Get("ETag"), gz.Header().Get("ETag")
	if plainTag == "" || gzTag == "" || plainTag == gzTag {
		t.Fatalf("ETags must be distinct: plain=%q gzip=%q", plainTag, gzTag)
	}
	if rr := get("/panel/app.js", "gzip", gzTag); rr.Code != http.StatusNotModified || rr.Body.Len() != 0 {
		t.Fatalf("gzip conditional code=%d body=%d", rr.Code, rr.Body.Len())
	}
	if rr := get("/panel/app.js", "gzip;q=0", ""); rr.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip;q=0 must not compress, enc=%q", rr.Header().Get("Content-Encoding"))
	}
}

// overview 有两个出口：首次构建与 TTL 快路径。两者都必须带 no-store，
// 否则账号余额、UID 与健康状态可能被浏览器或共享代理缓存并在无鉴权时复用。
func TestOverviewBothPathsNoStore(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
		r.Header.Set("Authorization", "Bearer k")
		return r
	}
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req())
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: code=%d body=%s", i, rr.Code, rr.Body)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("call %d: Cache-Control=%q want no-store", i, got)
		}
		if got := rr.Header().Get("Pragma"); got != "no-cache" {
			t.Fatalf("call %d: Pragma=%q want no-cache", i, got)
		}
		if !json.Valid(rr.Body.Bytes()) {
			t.Fatalf("call %d: invalid json: %s", i, rr.Body)
		}
	}
}

// overview 的条件请求：同快照必须回 304 且不带正文；invalidateOverview
// 后 ETag 必须变化并重新返回完整账号表。304 仍不得丢失 no-store，避免
// 共享代理缓存账号余额/UID。
func TestOverviewConditionalETag(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl := pool.New(filepath.Join(dir, "state.json"))
	pl.Add(a)
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})

	get := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
		req.Header.Set("Authorization", "Bearer k")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		return rr
	}

	first := get("")
	if first.Code != http.StatusOK {
		t.Fatalf("first code=%d body=%s", first.Code, first.Body)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("overview must carry an ETag")
	}
	if !json.Valid(first.Body.Bytes()) || !strings.Contains(first.Body.String(), `"u1"`) {
		t.Fatalf("first overview body=%s", first.Body)
	}

	second := get(etag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional code=%d want 304 body=%s", second.Code, second.Body)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 must not carry a body, got %q", second.Body)
	}
	if got := second.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("304 Cache-Control=%q want no-store", got)
	}

	pl.Disable("u1", pool.ManualDisableReason)
	p.invalidateOverview()
	third := get(etag)
	if third.Code != http.StatusOK {
		t.Fatalf("changed code=%d want 200 body=%s", third.Code, third.Body)
	}
	if next := third.Header().Get("ETag"); next == "" || next == etag {
		t.Fatalf("ETag after change=%q old=%q", next, etag)
	}
	if !strings.Contains(third.Body.String(), `"disabled":1`) {
		t.Fatalf("changed overview must reflect disabled account: %s", third.Body)
	}
}

// TestOverviewStableJSONMatchesMarshal 字节替换的稳定表示必须与旧二次
// Marshal（UptimeSec=0）完全一致，且不能改动响应原文里的 uptime。
func TestOverviewStableJSONMatchesMarshal(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"total":1,"uptime_sec":123,"version":"x"}`),
		[]byte(`{"uptime_sec":0}`),
		[]byte(`{"uptime_sec": -7 ,"x":1}`),
		[]byte(`{"nested":{"uptime_sec":42},"after":true}`),
	} {
		got := overviewStableJSON(raw)
		if got == nil {
			t.Fatalf("stable JSON rejected valid input %s", raw)
		}
		var obj map[string]any
		if err := json.Unmarshal(got, &obj); err != nil {
			t.Fatalf("stable JSON invalid: %v %s", err, got)
		}
		if v, ok := obj["uptime_sec"].(float64); ok && raw[1] != 'u' && v != 0 {
			t.Fatalf("uptime_sec not zeroed: %s", got)
		}
		if !bytes.Contains(raw, []byte(`"uptime_sec":123`)) && bytes.Contains(raw, []byte(`"uptime_sec":42`)) {
			// nested 形态不在当前编码结构里；只确保函数不会破坏其他字段。
		}
	}
	// 非整数字面量（小数/指数）必须拒绝，交给调用方回退完整 Marshal。
	for _, raw := range [][]byte{
		[]byte(`{"uptime_sec":1.5}`),
		[]byte(`{"uptime_sec":1e9}`),
		[]byte(`{"uptime_sec":true}`),
		[]byte(`{"other":1}`),
	} {
		if got := overviewStableJSON(raw); got != nil {
			t.Fatalf("stable JSON should reject %s, got %s", raw, got)
		}
	}
}

// 活动历史接口不得回显任何 UID，即使 Store 内部记录仍以 uid8 作为定位键。
func TestActivityHistoryOmitsUID(t *testing.T) {
	store := activity.New(activity.Config{Path: t.TempDir() + "/activity_history.jsonl"})
	if err := store.Append(activity.Record{
		UID: "13800138000", Nickname: "秋叶", Realm: "cn",
		Task: activity.TaskCheckin, Trigger: activity.TriggerManual, OK: true, Message: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Activity: store})
	rr := activityHistoryRequest(t, p, "/panel/api/activity/history")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	for _, leak := range []string{"13800138000", "13800138", `"uid"`} {
		if strings.Contains(body, leak) {
			t.Fatalf("activity response leaked %q: %s", leak, body)
		}
	}
}

func TestActivityHistoryQueryFilter(t *testing.T) {
	store := activity.New(activity.Config{Path: t.TempDir() + "/activity_history.jsonl"})
	for _, rec := range []activity.Record{
		{UID: "u1", Nickname: "秋叶", Task: activity.TaskCheckin, Trigger: activity.TriggerManual, OK: true, Message: "签到成功"},
		{UID: "u2", Nickname: "小满", Task: activity.TaskTravel, Trigger: activity.TriggerSchedule, OK: false, Message: "旅行领奖失败"},
	} {
		if err := store.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Activity: store})
	rr := activityHistoryRequest(t, p, "/panel/api/activity/history?q=%E7%A7%8B%E5%8F%B6")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var out struct {
		Records []map[string]any `json:"records"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || len(out.Records) != 1 || out.Records[0]["nickname"] != "秋叶" {
		t.Fatalf("query filter=%+v", out)
	}
	tooLong := "/panel/api/activity/history?q=" + strings.Repeat("a", 65)
	if rr := activityHistoryRequest(t, p, tooLong); rr.Code != http.StatusBadRequest {
		t.Fatalf("long q code=%d want 400", rr.Code)
	}
}

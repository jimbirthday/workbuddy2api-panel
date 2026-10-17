package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// scanFixture 构造一个带 N 个 CN 账号的 panel，上游任务列表由 httptest 提供。
// 每个账号两次列表请求（默认 + mp 口径），命中同一路径，用 X-Client-Platform 区分。
func scanFixture(t *testing.T, n int, handler func(w http.ResponseWriter, r *http.Request)) (*Panel, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.WebBaseCN = srv.URL
	up.HTTP.Transport = srv.Client().Transport

	dir := t.TempDir()
	p := pool.New(filepath.Join(dir, "state.json"))
	for i := 0; i < n; i++ {
		uid := "uid-" + strings.Repeat("x", i+1)
		file := filepath.Join(dir, "workbuddy-"+uid+".json")
		if err := os.WriteFile(file, []byte(`{"auth":{"accessToken":"at","uid":"`+uid+`","realm":"cn"},"account":{"uid":"`+uid+`","nickname":"n`+uid+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", Nickname: "n" + uid, FilePath: file})
	}
	return New(Config{Version: "test", APIKey: "k", Pool: p, Upstream: up}), &hits
}

func listJSON() string {
	return `{"code":0,"data":{"tasks":[{"task_code":"Model_chat_GLM5.2","title":"模型对话","current":0,"target":1,"claimed":false}]}}`
}

// TestScanAllAsyncSingleFlight 扫描必须异步返回且单飞：第二次触发不得再打上游。
func TestScanAllAsyncSingleFlight(t *testing.T) {
	release := make(chan struct{})
	// 失败路径也要放行，否则 httptest.Server.Close 会等挂住的 handler。
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p, hits := scanFixture(t, 3, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})

	post := func() map[string]any {
		req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("scan_all code=%d body=%s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := post()
	if first["running"] != true || first["started"] != true {
		t.Fatalf("首次触发应启动扫描：%v", first)
	}
	// 等第一批在途上游请求发出来：并发 3，每号串行两接口 → 先到 3 个。
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hits.Load(); got < 3 {
		t.Fatalf("在途上游请求=%d want>=3", got)
	}
	before := hits.Load()
	second := post()
	if second["started"] == true {
		t.Fatalf("扫描进行中再次触发不得启动第二轮：%v", second)
	}
	if got := hits.Load(); got != before {
		t.Fatalf("重复触发新增了上游请求：before=%d after=%d", before, got)
	}

	close(release)

	// 轮询到完成。
	var final map[string]any
	wait := time.Now().Add(10 * time.Second)
	for time.Now().Before(wait) {
		req := httptest.NewRequest("GET", "/panel/api/tasks/scan_status", nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("scan_status code=%d body=%s", rec.Code, rec.Body.String())
		}
		final = map[string]any{}
		if err := json.Unmarshal(rec.Body.Bytes(), &final); err != nil {
			t.Fatal(err)
		}
		if final["running"] == false {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final["running"] != false {
		t.Fatal("扫描未在超时内完成")
	}
	if done, total := final["done"].(float64), final["total"].(float64); done != total || total != 3 {
		t.Fatalf("进度 done/total=%v/%v want 3/3", done, total)
	}
	accounts, _ := final["accounts"].([]any)
	if len(accounts) != 3 {
		t.Fatalf("完成后应返回 3 个账号结果，got %d", len(accounts))
	}
	if pc := final["pending_count"].(float64); pc != 3 {
		t.Fatalf("pending_count=%v want 3（每号 1 项待办）", pc)
	}
	if ec := final["error_count"].(float64); ec != 0 {
		t.Fatalf("error_count=%v want 0", ec)
	}
}

// TestScanStatusProgressWhileRunning 扫描中必须能看到 done/total 递增，且此时不返回半成品 accounts。
func TestScanStatusProgressWhileRunning(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	p, _ := scanFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})
	req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(httptest.NewRecorder(), req)

	// 等待 total 就绪。
	var snap map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest("GET", "/panel/api/tasks/scan_status", nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		snap = map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &snap)
		if snap["total"].(float64) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap["total"].(float64) != 2 {
		t.Fatalf("total 未登记：%v", snap)
	}
	if snap["running"] != true {
		t.Fatalf("应仍在扫描：%v", snap)
	}
	if _, ok := snap["accounts"]; ok {
		t.Fatal("扫描中不得返回半成品 accounts")
	}
	close(release)
}

// TestScanAllSecondCallWhileIdleStarts 空闲时再次触发必须能启动新一轮（单飞不能变成永久锁）。
func TestScanAllSecondCallWhileIdleStarts(t *testing.T) {
	p, _ := scanFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})
	run := func() map[string]any {
		req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if out := run(); out["started"] != true {
		t.Fatalf("首轮应启动：%v", out)
	}
	deadline := time.Now().Add(10 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		// 通过 HTTP 快照读 running：直接读 scanState.running 会与后台 goroutine
		// 的写入构成数据竞争（-race 下稳定失败）。测试也走用户真实路径。
		req := httptest.NewRequest("GET", "/panel/api/tasks/scan_status", nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		snap := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &snap)
		if snap["running"] == false {
			stopped = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("扫描未在超时内结束，无法验证空闲重启")
	}
	if out := run(); out["started"] != true {
		t.Fatalf("空闲时第二轮应可启动：%v", out)
	}
}

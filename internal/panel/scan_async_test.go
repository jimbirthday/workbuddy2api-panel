package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// countingTransport 统计预扫实际发出的上游请求数；业务响应由 round 提供。
type countingTransport struct {
	calls *atomic.Int64
	reply string
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(t.reply)),
		Request:    r,
	}, nil
}

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

// TestGrowthQueueReusesFreshScan 扫描刚完成时判定可复用；TTL 过期或账号集合
// 变化时必须回退预扫。执行队列的完整链路由 autotask 测试覆盖，这里不跑 12s/项
// 的真实节流，避免把回归套件拖成分钟级。
func TestGrowthQueueReusesFreshScan(t *testing.T) {
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
	for time.Now().Before(time.Now().Add(5 * time.Second)) {
		if scanStatusForTest(t, p)["total"].(float64) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	waitScanDone(t, p)

	states := p.cfg.Pool.TaskAccounts(true)
	accts, items, ok := p.reuseFreshScan(states)
	if !ok || len(items) != 2 || len(accts) != 2 {
		t.Fatalf("fresh scan should reuse: ok=%v accounts=%d items=%d", ok, len(accts), len(items))
	}

	p.scanMu.Lock()
	p.scanCacheAt = time.Now().Add(-time.Hour)
	p.scanMu.Unlock()
	if _, _, ok := p.reuseFreshScan(states); ok {
		t.Fatal("expired scan must not be reused")
	}

	p.scanMu.Lock()
	p.scanCacheAt = time.Now()
	p.scanMu.Unlock()
	if _, _, ok := p.reuseFreshScan(states[:1]); ok {
		t.Fatal("account set mismatch must not be reused")
	}

	p.scan().mu.Lock()
	p.scan().errors = 1
	p.scan().mu.Unlock()
	if _, _, ok := p.reuseFreshScan(states); ok {
		t.Fatal("scan with query errors must not be reused")
	}
}

func scanStatusForTest(t *testing.T, p *Panel) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/tasks/scan_status", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestQueuePreScanPhaseProgress 预扫阶段必须可观测：账号多时前端只能拿到
// tasks/queue 的 phase/pre_done/pre_total，不能只看到按钮停在「启动中…」。
func TestQueuePreScanPhaseProgress(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock() // 断言失败时也要放行 httptest handler，否则 Server.Close 会挂住
	p, _ := scanFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.startGrowthQueue(1, true)
	}()

	deadline := time.Now().Add(5 * time.Second)
	var snap map[string]any
	for time.Now().Before(deadline) {
		req := httptest.NewRequest("GET", "/panel/api/tasks/queue", nil)
		req.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("queue status=%d %s", rr.Code, rr.Body)
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		// pre_total 在 phase 置位之后、账号 goroutine 启动之前写入；两个字段要在
		// 同一快照里就绪，否则会读到 pre_total=0 的瞬时状态。
		if snap["phase"] == "scan" && snap["running"] == true && snap["pre_total"].(float64) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap["phase"] != "scan" || snap["running"] != true {
		t.Fatalf("pre-scan phase not observable: %v", snap)
	}
	if got := int(snap["pre_total"].(float64)); got != 2 {
		t.Fatalf("pre_total=%d want 2", got)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("queue start did not finish")
	}
}

// TestQueueSetUsesUIDIndex 队列按 UID 更新状态时只应命中该账号的条目；
// setItemsLocked 必须重建索引，避免上一轮残留的 UID 映射到新条目。
func TestQueueSetUsesUIDIndex(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	q := p.queue()
	q.mu.Lock()
	q.setItemsLocked([]queueItem{
		{UID: "a", Code: "one", Status: "pending"},
		{UID: "b", Code: "two", Status: "pending"},
		{UID: "a", Code: "three", Status: "pending"},
	})
	q.mu.Unlock()
	p.queueSet(q, "a", func(it *queueItem) { it.Status = "done" })
	q.mu.Lock()
	got := []string{q.items[0].Status, q.items[1].Status, q.items[2].Status}
	q.mu.Unlock()
	if got[0] != "done" || got[1] != "pending" || got[2] != "done" {
		t.Fatalf("statuses=%v want [done pending done]", got)
	}

	q.mu.Lock()
	q.setItemsLocked([]queueItem{{UID: "b", Code: "later", Status: "pending"}})
	q.mu.Unlock()
	p.queueSet(q, "a", func(it *queueItem) { it.Status = "error" })
	q.mu.Lock()
	status := q.items[0].Status
	q.mu.Unlock()
	if status != "pending" {
		t.Fatalf("stale uid index updated new item: %q", status)
	}
}

func waitScanDone(t *testing.T, p *Panel) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if scanStatusForTest(t, p)["running"] == false {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scan did not finish")
}

// TestQueueStartInvalidatesScanCache 两个队列启动分支（复用快照 / 正常预扫）
// 都必须在 go runQueueItems 前使扫描缓存失效，否则同一批待办会被重复排队。
func TestQueueStartInvalidatesScanCache(t *testing.T) {
	// 新鲜的「空待办」扫描快照必须让下一次启动重新预扫，而不是在 TTL 内
	// 反复返回旧的空结果。这里用无待办响应模拟扫描完成后的状态。
	var calls atomic.Int64
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	up := upstream.New()
	up.HTTP.Transport = countingTransport{calls: &calls, reply: `{"code":0,"data":{"tasks":[]}}`}
	up.ChatHTTP = up.HTTP
	p.cfg.Upstream = up
	cn, err := auth.Parse([]byte(`{"uid":"cn1","accessToken":"at","realm":"cn"}`))
	if err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.Add(cn)
	st := p.scan()
	st.mu.Lock()
	st.items = []scanAccountItem{{UID: "cn1"}}
	st.covered = map[string]bool{"cn1": true}
	st.running = false
	st.errors = 0
	st.mu.Unlock()
	p.scanMu.Lock()
	p.scanCacheAt = time.Now()
	p.scanMu.Unlock()
	started, _, seq, _ := p.startGrowthQueue(1, true)
	if started || seq == -1 {
		t.Fatalf("empty fresh scan should stop without queue: started=%v seq=%d", started, seq)
	}
	p.scanMu.Lock()
	fresh := p.scanCacheAt
	p.scanMu.Unlock()
	if !fresh.IsZero() {
		t.Fatal("empty reused scan must invalidate cache")
	}

	// 预扫失败/无待办后也要失效：下一次启动必须重新请求上游，不能复用旧快照。
	p.scanMu.Lock()
	p.scanCacheAt = time.Now().Add(-time.Hour) // 过期 → 必须回落预扫
	p.scanMu.Unlock()
	st.mu.Lock()
	st.items = nil
	st.covered = map[string]bool{"cn1": true}
	st.mu.Unlock()
	started, _, _, _ = p.startGrowthQueue(1, true)
	if started {
		t.Fatal("no tasks should not start queue")
	}
	if calls.Load() == 0 {
		t.Fatal("fresh pre-scan did not hit upstream")
	}
	p.scanMu.Lock()
	fresh = p.scanCacheAt
	p.scanMu.Unlock()
	if !fresh.IsZero() {
		t.Fatal("pre-scan must leave scan cache empty")
	}
}

// TestReuseFreshScanCoverageIncludesGated 扫描覆盖集合必须包含被门控的
// global/企业版账号；只从 items（有任务的号）反推会在账号属性变化时误判复用。
func TestReuseFreshScanCoverageIncludesGated(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: upstream.New()})
	cn, err := auth.Parse([]byte(`{"uid":"cn1","accessToken":"at","realm":"cn"}`))
	if err != nil {
		t.Fatal(err)
	}
	g, err := auth.Parse([]byte(`{"uid":"g1","accessToken":"at","realm":"global"}`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := auth.Parse([]byte(`{"uid":"e1","accessToken":"at","realm":"cn","enterpriseId":"ent"}`))
	if err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.Add(cn)
	p.cfg.Pool.Add(g)
	p.cfg.Pool.Add(e)
	states := p.cfg.Pool.TaskAccounts(true)
	st := p.scan()
	st.mu.Lock()
	st.items = []scanAccountItem{}
	st.covered = map[string]bool{}
	for _, s := range states {
		if !s.Disabled {
			st.covered[s.UID] = true
		}
	}
	st.seq = 1
	st.running = false
	st.mu.Unlock()
	p.scanMu.Lock()
	p.scanCacheAt = time.Now()
	p.scanMu.Unlock()
	if _, items, ok := p.reuseFreshScan(states); !ok || len(items) != 0 {
		t.Fatalf("gated account set should be reusable: ok=%v items=%d", ok, len(items))
	}
}

// runScan 会为禁用账号保留 UID 为空的槽位。复用必须跳过这些槽位，否则
// AuthByUID("") 返回 nil 会让整个复用失败，池里只要有一个禁用号就退化为
// 每次重新预扫（470 账号约两分钟）。
func TestReuseFreshScanSkipsDisabledScanSlots(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: upstream.New()})
	cn, err := auth.Parse([]byte(`{"uid":"cn1","accessToken":"at","realm":"cn"}`))
	if err != nil {
		t.Fatal(err)
	}
	off, err := auth.Parse([]byte(`{"uid":"off1","accessToken":"at","realm":"cn"}`))
	if err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.Add(cn)
	p.cfg.Pool.Add(off)
	p.cfg.Pool.Disable("off1", "test")

	states := p.cfg.Pool.TaskAccounts(true)
	st := p.scan()
	st.mu.Lock()
	st.items = []scanAccountItem{{UID: "cn1"}, {}}
	st.covered = map[string]bool{"cn1": true}
	st.running = false
	st.errors = 0
	st.mu.Unlock()
	p.scanMu.Lock()
	p.scanCacheAt = time.Now()
	p.scanMu.Unlock()

	accts, items, ok := p.reuseFreshScan(states)
	if !ok || len(items) != 0 {
		t.Fatalf("disabled empty slot must be skipped: ok=%v accounts=%d items=%d", ok, len(accts), len(items))
	}
}

// TestRunScanCoveredUsesActualUIDs 覆盖集合必须来自 runScan 的真实纳入账号，
// 而不是 items 里可能为空 UID 的槽位。AuthByUID 返回 nil 的号也必须保留在
// covered 中，避免复用把它误判成「账号集合一致」。
func TestRunScanCoveredUsesActualUIDs(t *testing.T) {
	p, _ := scanFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})
	victim := p.cfg.Pool.List()[0].UID
	req := httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil)
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(httptest.NewRecorder(), req)
	waitScanDone(t, p)

	st := p.scan()
	st.mu.Lock()
	covered := make(map[string]bool, len(st.covered))
	for uid := range st.covered {
		covered[uid] = true
	}
	st.mu.Unlock()
	if !covered[victim] {
		t.Fatalf("covered=%v must retain scanned uid %q even when AuthByUID is nil", covered, victim)
	}
	if covered[""] {
		t.Fatalf("covered must not contain empty uid: %v", covered)
	}
	// 扫描后账号被移除：复用必须拒绝旧快照，不能按旧清单排队。
	p.cfg.Pool.Remove(victim)
	if _, _, ok := p.reuseFreshScan(p.cfg.Pool.TaskAccounts(true)); ok {
		t.Fatalf("removed uid %q must invalidate fresh scan reuse", victim)
	}
}

// TestQueueRunHTTPReturnsBeforePreScan 队列入口必须立即返回，而不是等 470 个
// 账号预扫完：慢上游下用户先看到「启动中…」直到反代/浏览器超时是原始故障。
// 同时校验 HTTP 返回的 seq 与随后 tasks/queue 的快照一致，否则前端会误判为
// 「别的队列接管」而停掉本轮轮询。
func TestQueueRunHTTPReturnsBeforePreScan(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()

	p, _ := scanFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})

	start := time.Now()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue code=%d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("run_queue blocked on pre-scan: elapsed=%s", elapsed)
	}
	var started map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started["started"] != true || started["phase"] != "scan" {
		t.Fatalf("start response=%v", started)
	}
	startSeq := int(started["seq"].(float64))

	var snap map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rq := httptest.NewRequest("GET", "/panel/api/tasks/queue", nil)
		rq.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, rq)
		if rr.Code != http.StatusOK {
			t.Fatalf("queue status code=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap["phase"] == "scan" && snap["pre_total"].(float64) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap["running"] != true || snap["phase"] != "scan" {
		t.Fatalf("pre-scan not observable: %v", snap)
	}
	if got := int(snap["seq"].(float64)); got != startSeq {
		t.Fatalf("start seq=%d queue seq=%d (frontend would stop polling)", startSeq, got)
	}
	// 放行预扫后只确认状态仍可继续读取；执行阶段每项含真实节流，不在本测试
	// 内等待完整队列（否则会把回归套件拖到分钟级）。
	unblock()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rq := httptest.NewRequest("GET", "/panel/api/tasks/queue", nil)
		rq.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, rq)
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap["phase"] == "exec" || snap["running"] == false {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue did not leave scan phase after unblock: %v", snap)
}

// TestQueuePreScanFailureNotReportedAsEmpty 预扫时上游列表失败且没有任何可用
// 条目，不能返回「全部账号没有待办」——那会把「没查清」伪装成「全做完了」。
// 队列应保持未启动，并给出可重试的明确提示。
func TestQueuePreScanFailureNotReportedAsEmpty(t *testing.T) {
	p, _ := scanFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":500,"msg":"upstream down"}`, http.StatusInternalServerError)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue code=%d body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	var snap map[string]any
	for time.Now().Before(deadline) {
		rq := httptest.NewRequest("GET", "/panel/api/tasks/queue", nil)
		rq.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, rq)
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap["running"] == false {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap["running"] != false {
		t.Fatalf("failed pre-scan must not start queue: %v", snap)
	}
	msg, _ := snap["message"].(string)
	if !strings.Contains(msg, "预扫不完整") || !strings.Contains(msg, "查询失败") {
		t.Fatalf("message=%q should explain incomplete pre-scan", msg)
	}
}

// TestQueuePreScanPartialFailureWarns 预扫部分账号失败、部分账号仍有待办时，
// 队列可以继续执行，但必须返回非阻塞 pre_warn；否则用户会把“队列已启动”
// 误读成“所有账号都已纳入”，漏扫无处可见。
func TestQueuePreScanPartialFailureWarns(t *testing.T) {
	failing := ""
	p, _ := scanFixture(t, 2, func(w http.ResponseWriter, r *http.Request) {
		// 第二个账号（listJSON 成功号之外的）返回 500：按 Authorization 分流，
		// 避免依赖并发调度顺序。
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if failing != "" && token == failing {
			http.Error(w, `{"code":500,"msg":"down"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON()))
	})
	failing = "bad-token"
	a := p.cfg.Pool.AuthByUID("uid-xx")
	if a == nil {
		t.Fatal("fixture account uid-xx missing")
	}
	a.AccessToken = "bad-token"
	// 预扫失败分支要求至少一个账号成功、另一个查询失败；先确保扫描缓存为空，
	// 否则同一个 fixture 内先跑过的扫描会让本轮直接复用旧待办、根本不打上游。
	p.invalidateScanCache()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	req.Header.Set("Authorization", "Bearer k")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue code=%d body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	var snap map[string]any
	for time.Now().Before(deadline) {
		rq := httptest.NewRequest("GET", "/panel/api/tasks/queue", nil)
		rq.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, rq)
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if snap["pre_warn"] != nil && snap["pre_warn"] != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	warn, _ := snap["pre_warn"].(string)
	if !strings.Contains(warn, "查询失败") || !strings.Contains(warn, "漏扫") {
		t.Fatalf("pre_warn=%q snap=%v", warn, snap)
	}
}

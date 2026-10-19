package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestEnterpriseTaskEndpointsDoNotReachUpstream 企业号无个人成长体系：即使旧前端/
// 外部脚本直接调任务端点，网关也不能把注定 403 的上游请求打出去，更不能罚号。
// 同时放置一个个人号作对照，证明“零请求”不是 fixture 没接上。
func TestEnterpriseTaskEndpointsDoNotReachUpstream(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-enterprise-user-usage") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"credit":812.45,"limitNum":2000,"cycleEndTime":"2099-10-26 23:59:59"}`))
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.WebBaseCN = srv.URL
	up.HTTP.Transport = srv.Client().Transport

	pl := pool.New("")
	ent, err := auth.Parse([]byte(`{"uid":"ent1","accessToken":"ent-at","realm":"cn","enterpriseId":"e1"}`))
	if err != nil {
		t.Fatal(err)
	}
	pl.Add(ent)
	pl.Add(&auth.Auth{UID: "per1", AccessToken: "per-at"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})

	cases := []struct {
		method, path, body string
		wantStatus         int
		wantMarker         string
	}{
		{"GET", "/panel/api/accounts/ent1/tasks", "", http.StatusOK, `"enterprise":true`},
		{"POST", "/panel/api/accounts/ent1/tasks/accept", `{"task_codes":["x"]}`, http.StatusBadRequest, "企业账号"},
		{"POST", "/panel/api/accounts/ent1/tasks/accept_all", `{}`, http.StatusOK, `"enterprise":true`},
		{"POST", "/panel/api/accounts/ent1/tasks/claim", `{"task_code":"x"}`, http.StatusBadRequest, "企业账号"},
		{"POST", "/panel/api/accounts/ent1/tasks/auto", `{"task_code":"x"}`, http.StatusBadRequest, "企业账号"},
		{"POST", "/panel/api/accounts/ent1/tasks/auto_all", `{}`, http.StatusOK, `"enterprise":true`},
		{"POST", "/panel/api/accounts/ent1/checkin", `{}`, http.StatusOK, "无签到体系"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != c.wantStatus || !strings.Contains(rr.Body.String(), c.wantMarker) {
			t.Errorf("%s %s: code=%d body=%s want %d/%q", c.method, c.path, rr.Code, rr.Body.String(), c.wantStatus, c.wantMarker)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("enterprise-only requests must not reach upstream, hits=%d", hits.Load())
	}

	// 个人号对照：列表端点必须真的访问上游（否则上面的 0 没有证明力）。
	req := httptest.NewRequest("GET", "/panel/api/accounts/per1/tasks", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || hits.Load() == 0 {
		t.Fatalf("personal account must query upstream: code=%d hits=%d body=%s", rr.Code, hits.Load(), rr.Body)
	}
}

// TestEnterpriseQueueExecutionSkipsBeforeUpstream 队列状态可能来自旧快照或外部
// 请求；即使企业号混进 accts，runQueueItems 也必须在 accept/growth/claim 之前
// 跳过，不能把注定 403 的请求打给上游。这里直接调用执行主体，断言零请求且
// 该账号条目被标成 skipped。
func TestEnterpriseQueueExecutionSkipsBeforeUpstream(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.WebBaseCN = srv.URL
	up.HTTP.Transport = srv.Client().Transport

	pl := pool.New("")
	ent, err := auth.Parse([]byte(`{"uid":"ent1","accessToken":"ent-at","realm":"cn","enterpriseId":"e1"}`))
	if err != nil {
		t.Fatal(err)
	}
	pl.Add(ent)
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})
	q := p.queue()
	q.mu.Lock()
	q.running = true
	q.setItemsLocked([]queueItem{{UID: "ent1", Kind: "growth", Code: "chat_5", Status: "pending"}})
	q.mu.Unlock()

	p.runQueueItems([]queueAccount{{a: ent}}, []queueItem{{UID: "ent1", Kind: "growth", Code: "chat_5"}}, 1)

	if hits.Load() != 0 {
		t.Fatalf("enterprise queue must not reach upstream, hits=%d", hits.Load())
	}
	q.mu.Lock()
	got := q.items[0]
	q.mu.Unlock()
	if got.Status != "skipped" || !strings.Contains(got.Message, "企业") {
		t.Fatalf("enterprise queue item=%+v want skipped/企业", got)
	}
	if q.running {
		t.Fatal("queue must release running flag after execution")
	}
}

// TestQueueExecutionToleratesAccountRemoved 队列启动后账号可能被删除/移出池，
// 此时 accts 里的 Auth 为 nil；执行主体只能把条目标 skipped，不能解引用崩溃。
func TestQueueExecutionToleratesAccountRemoved(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: upstream.New()})
	q := p.queue()
	q.mu.Lock()
	q.running = true
	q.setItemsLocked([]queueItem{{UID: "gone1", Kind: "growth", Code: "chat_5", Status: "pending"}})
	q.mu.Unlock()

	p.runQueueItems([]queueAccount{{uid: "gone1"}}, []queueItem{{UID: "gone1", Kind: "growth", Code: "chat_5"}}, 1)

	q.mu.Lock()
	got, running := q.items[0], q.running
	q.mu.Unlock()
	if got.Status != "skipped" || !strings.Contains(got.Message, "不在账号池") {
		t.Fatalf("removed account item=%+v want skipped", got)
	}
	if running {
		t.Fatal("queue must release running flag after execution")
	}
}

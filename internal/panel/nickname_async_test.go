package panel

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSyncNicknamesAsyncReturnsBeforeProfileCalls 昵称同步是逐号上游 RPC，不能让
// 手动余额刷新等它跑完。这里让 profile 请求永久阻塞，验证调用立即返回、后台
// 任务仍被停机等待。
func TestSyncNicknamesAsyncReturnsBeforeProfileCalls(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u1","nickname":"新名"}}`))
	}))
	defer srv.Close()

	pl := pool.New("")
	dir := t.TempDir()
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", FilePath: dir + "/workbuddy-u1.json"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl.Add(a)
	up := upstream.New()
	up.WebBaseCN = srv.URL
	up.HTTP = srv.Client()
	p := New(Config{Pool: pl, Upstream: up, Scheduler: scheduler.New(scheduler.Config{Pool: pl, Upstream: up})})

	done := make(chan struct{})
	go func() {
		p.syncNicknamesAsync()
		close(done)
	}()
	select {
	case <-done:
	case <-started:
		t.Fatal("syncNicknamesAsync blocked until profile RPC completed")
	}
	// 后台任务已经启动并处于 profile 请求中；放行后再等停机排空。
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background profile request did not start")
	}
	close(release)
	p.StopWaiting()
	if st, _ := pl.Status("u1"); st.Nickname != "新名" {
		t.Fatalf("nickname=%q want 新名 after background sync", st.Nickname)
	}
}

// TestSyncNicknamesAsyncSkipsEmptyPool 空池/无上游不能启动后台任务，也不能阻塞。
func TestSyncNicknamesAsyncSkipsEmptyPool(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u1","nickname":"x"}}`))
	}))
	defer srv.Close()
	pl := pool.New("")
	up := upstream.New()
	up.WebBaseCN = srv.URL
	up.HTTP = srv.Client()
	p := New(Config{Pool: pl, Upstream: up})
	p.syncNicknamesAsync()
	p.StopWaiting()
	if calls.Load() != 0 {
		t.Fatalf("empty pool issued %d profile calls", calls.Load())
	}
}

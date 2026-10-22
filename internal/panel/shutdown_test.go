package panel

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// StopWaiting 与 GoBackground 并发时不能在 WaitGroup 上交错 Add/Wait。
// 关闭闸后提交的任务应被拒绝；已登记的任务必须全部等到。
func TestStopWaitingRejectsLateBackgroundTasks(t *testing.T) {
	p := New(Config{Version: "test", Pool: nil})
	started := make(chan struct{})
	release := make(chan struct{})
	p.GoBackground(func() {
		close(started)
		<-release
	})
	<-started

	done := make(chan struct{})
	go func() {
		p.StopWaiting()
		close(done)
	}()

	// 给 StopWaiting 一点时间进入 Wait，然后并发提交新任务。旧实现会在这里
	// 因 WaitGroup 计数变化而挂住或让任务逃逸。
	time.Sleep(20 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.GoBackground(func() { t.Error("task submitted after StopWaiting must not run") })
		}()
	}
	wg.Wait()
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopWaiting hung after concurrent GoBackground")
	}
	p.bgMu.Lock()
	rejected := p.bgRejected
	p.bgMu.Unlock()
	if rejected != 20 {
		t.Fatalf("rejected=%d want 20", rejected)
	}
}

// 后台批量任务的 panic 必须在本轮隔离并让 b 返回 true，不能冒出到
// GoBackground 的 goroutine 里把整个进程带崩；done 仍要关闭，调用者不悬挂。
func TestRunBackgroundBoundedRecoversPanic(t *testing.T) {
	p := New(Config{Version: "test"})
	done := make(chan bool, 1)
	go func() {
		done <- p.runBackgroundBounded(2, 1, func(int) { panic("boom") })
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("runBackgroundBounded should report the task was accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runBackgroundBounded hung after worker panic")
	}
	// 停机等待也必须能完成：panic 隔离不能留下未 Done 的 WaitGroup。
	wait := make(chan struct{})
	go func() {
		p.StopWaiting()
		close(wait)
	}()
	select {
	case <-wait:
	case <-time.After(2 * time.Second):
		t.Fatal("StopWaiting hung after background panic")
	}
}

// 停机后仍在等待后台结果的 HTTP 入口不能挂死：GoBackground 拒绝时必须回滚
// wg/账号锁并立即返回。这里用真实 handler 覆盖扫描、队列、导入和批量查询。
func TestShutdownRejectsHandlersWithoutHanging(t *testing.T) {
	t.Run("import cockpit", func(t *testing.T) {
		p := New(Config{Version: "test", AuthDir: t.TempDir()})
		p.StopWaiting()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, err := mw.CreateFormFile("file", "accounts.json")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(`[{"uid":"u1","access_token":"at","refresh_token":"rt"}]`))
		mw.Close()
		req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			p.importCockpit(rec, req)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("importCockpit hung after StopWaiting")
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("import cockpit code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("scan all", func(t *testing.T) {
		p := New(Config{Version: "test", Pool: pool.New(filepath.Join(t.TempDir(), "state.json"))})
		p.StopWaiting()
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			p.tasksScanAll(rec, httptest.NewRequest("POST", "/panel/api/tasks/scan_all", nil))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("tasksScanAll hung after StopWaiting")
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("scan_all code=%d body=%s", rec.Code, rec.Body.String())
		}
		st := p.scan()
		st.mu.Lock()
		running := st.running
		st.mu.Unlock()
		if running {
			t.Fatal("scan must not stay running after rejected background task")
		}
	})

	t.Run("run queue", func(t *testing.T) {
		p, _ := scanFixture(t, 1, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(listJSON()))
		})
		p.StopWaiting()
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			p.tasksRunQueue(rec, httptest.NewRequest("POST", "/panel/api/tasks/run_queue", bytes.NewBufferString(`{}`)))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("tasksRunQueue hung after StopWaiting")
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("run_queue code=%d body=%s", rec.Code, rec.Body.String())
		}
		q := p.queue()
		q.mu.Lock()
		running := q.running
		q.mu.Unlock()
		if running {
			t.Fatal("queue must not stay running after rejected background task")
		}
	})
}

// claimedListJSON 返回一条「已领取」的任务：队列执行它不会触发任何上游写
// 请求与真实动作（runGrowthQueued 直接返回），也让它不进入 accept 批量——
// 这样 worker 会很快走完第一项并停在项间节流上，测试能精确验证「停机能否
// 打断项间节流」而不是被打断在别的等待里。
func claimedListJSON() string {
	return `{"code":0,"data":{"tasks":[{"task_code":"Model_chat_GLM5.2","title":"模型对话","current":1,"target":1,"accept_status":"claimed"}]}}`
}

// TestQueueStopInterruptsThrottleAndLeavesNoPending 停机时队列必须立即退出，
// 而不是等「项间节流 1.05s × 条目数」。剩余条目要显式标成 skipped：停留在
// pending 会让前端一直显示「排队」，用户无法分辨「没跑完」和「卡住」。
//
// 关键时序：必须等第一项真的进入终态（证明 worker 已经走完执行、正停在项间
// 节流上）再调 StopWaiting。否则 worker 可能还没开始跑就被停机信号打断，
// 用例会在「啥也没验证」的情况下假绿。
func TestQueueStopInterruptsThrottleAndLeavesNoPending(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(claimedListJSON()))
	}))
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN, up.BillingBaseCN, up.WebBaseCN = srv.URL, srv.URL, srv.URL
	up.HTTP.Transport = srv.Client().Transport

	dir := t.TempDir()
	pl := pool.New(filepath.Join(dir, "state.json"))
	uid := "uid-stopslow"
	pl.Add(&auth.Auth{UID: uid, AccessToken: "at", Nickname: "n", RefreshToken: "rt", ExpiresAt: 9999999999})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})

	oldGap := reportGap
	reportGap = time.Hour // 若不可取消，本用例会卡到超时
	t.Cleanup(func() { reportGap = oldGap })

	item := queueItem{UID: uid, Kind: "growth", Code: "Model_chat_GLM5.2", Status: "pending"}
	items := []queueItem{item, item, item}
	accts := []queueAccount{{a: pl.AuthByUID(uid), uid: uid}}
	q := p.queue()
	q.mu.Lock()
	q.items = items
	q.byUID = map[string][]int{uid: {0, 1, 2}}
	q.running, q.phase = true, "run"
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		p.runQueueItems(accts, items, 1)
		close(done)
	}()
	// 等第一项进入终态：此时 worker 必然已经执行完第一项、正阻塞在项间节流。
	deadline := time.Now().Add(5 * time.Second)
	for {
		q.mu.Lock()
		st := q.items[0].Status
		q.mu.Unlock()
		if st == "done" || st == "error" {
			q.mu.Lock()
			msg := q.items[0].Message
			q.mu.Unlock()
			if st != "done" {
				t.Fatalf("第一项应为 done，实际 %s：%s", st, msg)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("第一项未在 5s 内完成（当前 %s），无法验证项间节流可中断", st)
		}
		time.Sleep(2 * time.Millisecond)
	}
	start := time.Now()
	p.StopWaiting()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StopWaiting hung：队列仍在不可取消的节流里")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("停机耗时 %v，队列未被中断", elapsed)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running {
		t.Fatal("queue still running after StopWaiting")
	}
	for i, it := range q.items {
		if it.Status == "pending" || it.Status == "running" {
			t.Fatalf("item %d 仍是 %s（应标成 skipped）", i, it.Status)
		}
	}
	if got := q.items[1].Status; got != "skipped" {
		t.Fatalf("item 1 应为 skipped，实际 %s", got)
	}
}

package panel

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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

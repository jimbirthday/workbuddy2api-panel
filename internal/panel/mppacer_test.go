package panel

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// mpPacerFixture 构造一个 mp 任务「还差 2 条事件」的上游桩。
// 上报事件计数用 eventCalls；任务进度固定停在 0/2（不领奖），便于观察分步发送。
func mpPacerFixture(t *testing.T) (*Panel, *auth.Auth, *atomic.Int64) {
	t.Helper()
	pl := pool.New("")
	a := &auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at", RefreshToken: "rt"}
	pl.Add(a)

	var eventCalls atomic.Int64
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"msg":"ok","data":{}}`
		switch {
		case strings.Contains(r.URL.Path, "/activity/growth/tasks") && r.Method == http.MethodGet:
			body = `{"code":0,"msg":"ok","data":{"tasks":[
				{"task_code":"Sequential_Tasks_3","target":5,"current":3,"accept_status":"accepted"}
			]}}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v2/report"):
			// mp 事件上报（chat_request_send 的载体）。
			eventCalls.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})
	return p, a, &eventCalls
}

// TestMPPaceEnqueuesWithoutBlocking 45s/条的事件链不得在请求栈里 sleep：
// 任务动作必须立即返回「已排队」，把等待交给调度器。旧实现单账号全量任务
// 因此必然超过 5 分钟 HTTP 上限（504）且期间一直占着账号锁。
func TestMPPaceEnqueuesWithoutBlocking(t *testing.T) {
	p, a, _ := mpPacerFixture(t)
	oldGap := mpChatEventGap
	mpChatEventGap = 10 * time.Second // 只影响后续发送节奏，不影响入队耗时
	t.Cleanup(func() { mpChatEventGap = oldGap })
	t.Cleanup(func() {
		p.stopMPPace()
		mpChatEventGap = oldGap
	})

	start := time.Now()
	msg, err := runSequentialChat5(p, a)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runSequentialChat5: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("登记耗时 %v，事件链仍在请求栈里等待", elapsed)
	}
	if !strings.Contains(msg, "后台") {
		t.Fatalf("message=%q want 含「后台」提示", msg)
	}
	if !p.mpPacePending(a.UID, "Sequential_Tasks_3") {
		t.Fatal("作业未登记到节奏调度器")
	}
}

// TestMPPaceSendsOneEventPerStep 每一拍只发一条事件（真人节奏由间隔保证，
// 一次发多条会被上游反作弊回滚），且不持锁跨拍等待。
func TestMPPaceSendsOneEventPerStep(t *testing.T) {
	p, a, eventCalls := mpPacerFixture(t)
	oldGap, oldTick := mpChatEventGap, mpPaceTick
	// 间隔取 1 分钟：首条立刻到点，后续不会在观察窗口内再发。
	// 必须先 stopMPPace 再恢复全局（否则清理写与后台读竞争）。
	mpChatEventGap = time.Minute
	mpPaceTick = 5 * time.Millisecond
	t.Cleanup(func() {
		p.stopMPPace()
		mpChatEventGap, mpPaceTick = oldGap, oldTick
	})

	if !p.enqueueMPPace(a, "Sequential_Tasks_3", false) {
		t.Fatal("enqueue failed")
	}
	st := p.mpPace()
	st.mu.Lock()
	for _, job := range st.jobs {
		job.Next = time.Now() // 跳过首条前的随机等待
	}
	st.mu.Unlock()

	// 一拍：应恰好发出 1 条。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eventCalls.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := eventCalls.Load(); got != 1 {
		t.Fatalf("events=%d want 1（每拍一条）", got)
	}
	// 作业仍在排队（进度未达标），且已经排到下一次。
	st.mu.Lock()
	job, ok := st.jobs[mpJobKey(a.UID, "Sequential_Tasks_3")]
	sent := 0
	if ok {
		sent = job.Sent
	}
	st.mu.Unlock()
	if !ok {
		t.Fatal("作业不应在未达标时被移除")
	}
	if sent != 1 {
		t.Fatalf("job.Sent=%d want 1", sent)
	}
}

// TestMPPaceDropsJobWhenAccountRemoved 账号被移除时作业必须清理，
// 不能一直空转到停机。
func TestMPPaceDropsJobWhenAccountRemoved(t *testing.T) {
	p, a, _ := mpPacerFixture(t)
	oldGap, oldTick := mpChatEventGap, mpPaceTick
	mpChatEventGap = time.Minute
	mpPaceTick = 5 * time.Millisecond
	t.Cleanup(func() {
		p.stopMPPace()
		mpChatEventGap, mpPaceTick = oldGap, oldTick
	})

	if !p.enqueueMPPace(a, "Sequential_Tasks_3", false) {
		t.Fatal("enqueue failed")
	}
	p.cfg.Pool.Remove(a.UID)
	st := p.mpPace()
	st.mu.Lock()
	for _, job := range st.jobs {
		job.Next = time.Now()
	}
	st.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st.mu.Lock()
		n := len(st.jobs)
		st.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("账号移除后作业仍留在调度器里")
}

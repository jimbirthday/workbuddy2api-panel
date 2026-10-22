package panel

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// taskSnapshotFixture 构造一个最小 Panel：上游任务列表按口径分别计数，
// 默认口径返回 Model_chat，mp 口径额外返回 mp 专属任务。
func taskSnapshotFixture(t *testing.T) (*Panel, *auth.Auth, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	pl := pool.New("")
	a := &auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at", RefreshToken: "rt"}
	pl.Add(a)

	var defCalls, mpCalls atomic.Int64
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"msg":"ok","data":{"tasks":[
			{"task_code":"Model_chat","target":1,"current":0,"accept_status":"accepted"}
		]}}`
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			mpCalls.Add(1)
			body = `{"code":0,"msg":"ok","data":{"tasks":[
				{"task_code":"Model_chat","target":1,"current":0,"accept_status":"accepted"},
				{"task_code":"Sequential_Tasks_1","target":1,"current":0,"accept_status":"accepted"}
			]}}`
		} else {
			defCalls.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})
	return p, a, &defCalls, &mpCalls
}

// TestTaskListSnapshotCoalescesRepeatedReads 一次「一键做任务」会对同一账号读
// 几十次任务列表；同一次执行内的重复读取必须复用快照（默认+mp 各 1 次），
// 而不是每次都重打上游。这是「做任务慢」的第二大来源。
func TestTaskListSnapshotCoalescesRepeatedReads(t *testing.T) {
	p, a, defCalls, mpCalls := taskSnapshotFixture(t)

	// 模拟 25 个动作各自的 before 读取。
	for i := 0; i < 25; i++ {
		task, err := p.taskByCode(a, "Model_chat")
		if err != nil || task == nil {
			t.Fatalf("taskByCode #%d err=%v task=%v", i, err, task)
		}
	}
	if got := defCalls.Load(); got != 1 {
		t.Fatalf("默认口径列表请求=%d want 1（应复用快照）", got)
	}
	if got := mpCalls.Load(); got != 1 {
		t.Fatalf("mp 口径列表请求=%d want 1（合并快照只拉一次）", got)
	}

	// mp 专属码必须能从同一份合并快照里命中（不再额外打 mp 列表）。
	if task, err := p.taskByCodeMP(a, "Sequential_Tasks_1"); err != nil || task == nil {
		t.Fatalf("taskByCodeMP err=%v task=%v", err, task)
	}
	if got := defCalls.Load(); got != 1 {
		t.Fatalf("mp 回读额外打了默认口径 %d 次", got)
	}
	if got := mpCalls.Load(); got != 1 {
		t.Fatalf("mp 回读额外打了 mp 口径 %d 次", got)
	}
}

// TestTaskListSnapshotInvalidatedAfterWrite 写操作后必须失效：
// 否则后续 before 判定会用旧进度，重复执行同一动作。
func TestTaskListSnapshotInvalidatedAfterWrite(t *testing.T) {
	p, a, defCalls, _ := taskSnapshotFixture(t)
	if _, err := p.taskByCode(a, "Model_chat"); err != nil {
		t.Fatal(err)
	}
	if defCalls.Load() != 1 {
		t.Fatalf("warmup calls=%d", defCalls.Load())
	}
	p.invalidateTaskSnap(a.UID)
	if _, err := p.taskByCode(a, "Model_chat"); err != nil {
		t.Fatal(err)
	}
	if got := defCalls.Load(); got != 2 {
		t.Fatalf("失效后读取 calls=%d want 2", got)
	}
	// 新鲜回读（不走缓存）也必须重新打上游。
	if _, err := p.taskByCodeFresh(a, "Model_chat"); err != nil {
		t.Fatal(err)
	}
	if got := defCalls.Load(); got != 3 {
		t.Fatalf("fresh 读取 calls=%d want 3", got)
	}
}

// TestTaskListSnapshotSingleFlight 同一账号的并发读取只打一次上游
// （扫描与队列同时打开同一账号时不重复拉列表）。
func TestTaskListSnapshotSingleFlight(t *testing.T) {
	p, a, defCalls, _ := taskSnapshotFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.taskByCode(a, "Model_chat"); err != nil {
				t.Errorf("taskByCode: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := defCalls.Load(); got != 1 {
		t.Fatalf("并发读取触发 %d 次列表请求 want 1（single-flight）", got)
	}
}

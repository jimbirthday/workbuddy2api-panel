package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func taskRequest(p *Panel, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/panel/api/"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

func TestTaskAsyncSurvivesDisconnectAndRetainsResults(t *testing.T) {
	pl := pool.New("")
	a := &auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at"}
	pl.Add(a)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{}}`))}, nil
	})}
	store := activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})
	p := New(Config{Pool: pl, APIKey: "k", Upstream: up, Activity: store})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/panel/api/tasks/run", strings.NewReader(`{"scope":"13800138000","action":"checkin","async":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	cancel()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background action did not start")
	}
	snapshot := taskRequest(p, "GET", "benefits/batches", "")
	if !strings.Contains(snapshot.Body.String(), `"running":true`) || strings.Contains(snapshot.Body.String(), a.UID) {
		t.Fatalf("running snapshot=%s", snapshot.Body.String())
	}
	duplicate := taskRequest(p, "POST", "tasks/run", `{"scope":"all","action":"checkin","async":true}`)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate code=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	once.Do(func() { close(release) })
	p.StopWaiting()
	for i := 0; i < 2; i++ {
		snapshot = taskRequest(p, "GET", "benefits/batches", "")
		var data struct {
			Runs []batchRun `json:"runs"`
		}
		if err := json.Unmarshal(snapshot.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Runs) != 1 {
			t.Fatalf("runs=%s", snapshot.Body.String())
		}
		run := data.Runs[0]
		if run.Running || !run.Done || run.OK != 1 || run.Failed != 0 || len(run.Results) != 1 || !run.Results[0].OK || run.Scope != benefitRefFor(a.UID) {
			t.Fatalf("finished snapshot=%s", snapshot.Body.String())
		}
	}
	history, err := store.Query(activity.Filter{Task: activity.TaskCheckin})
	if err != nil || history.Total != 1 || !history.Records[0].OK {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestTaskRequestRejectsMalformedJSONBeforeExecution(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid request called upstream")
		return nil, context.Canceled
	})}
	p := New(Config{Pool: pl, Upstream: up, APIKey: "k"})
	for _, body := range []string{`{"action":"checkin",`, `{"action":"checkin"} {}`, strings.Repeat(" ", 1<<20) + `{}`} {
		rr := taskRequest(p, "POST", "tasks/run", body)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
	}
}

func TestTaskActionFailureMatchesHistory(t *testing.T) {
	for _, action := range []string{"checkin", "travel", "streak"} {
		t.Run(action, func(t *testing.T) {
			pl := pool.New("")
			pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
			up := upstream.New()
			up.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 502, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":500,"msg":"upstream unavailable"}`))}, nil
			})}
			store := activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})
			sched := scheduler.New(scheduler.Config{Pool: pl, Upstream: up, Activity: store})
			p := New(Config{Pool: pl, Upstream: up, APIKey: "k", Activity: store, Scheduler: sched})
			rr := taskRequest(p, "POST", "tasks/run", `{"action":"`+action+`"}`)
			var data struct {
				Results []tasksRunResult `json:"results"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			if rr.Code != 200 || len(data.Results) != 1 || data.Results[0].OK {
				t.Fatalf("response=%s", rr.Body.String())
			}
			got, err := store.Query(activity.Filter{Task: activity.Task(action)})
			if err != nil || got.Total != 1 || got.Records[0].OK {
				t.Fatalf("history=%+v err=%v", got, err)
			}
		})
	}
}

func TestFocusedTasksRetainAllOptionsAndScopedSummary(t *testing.T) {
	pl := pool.New("")
	for _, uid := range []string{"u1", "u2"} {
		pl.Add(&auth.Auth{UID: uid, Nickname: uid, AccessToken: "at"})
	}
	pl.NoteCheckinDone("u2")
	p := New(Config{Pool: pl, APIKey: "k"})
	rr := taskRequest(p, "GET", "tasks?focus="+benefitRefFor("u1"), "")
	var data struct {
		Accounts []tasksAccount `json:"accounts"`
		Options  []tasksAccount `json:"account_options"`
		Summary  tasksSummary   `json:"summary"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Accounts) != 1 || len(data.Options) != 2 || data.Summary.Accounts != 1 || data.Summary.CheckinDone != 0 || data.Summary.CheckinPending != 1 {
		t.Fatalf("snapshot=%s", rr.Body.String())
	}
}

func TestBenefitLogRejectsUnknownReferenceAndInvalidFilter(t *testing.T) {
	p := New(Config{Pool: pool.New(""), APIKey: "k", Activity: activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})})
	for _, path := range []string{"benefits/log?ref=unknown", "benefits/log?task=invalid", "benefits/log?result=invalid"} {
		rr := taskRequest(p, "GET", path, "")
		if rr.Code != 400 {
			t.Fatalf("%s code=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestGrowthCompletedTaskClaimsWithoutRepeatingAction(t *testing.T) {
	for _, claimFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "claim_failure"}[claimFails], func(t *testing.T) {
			var claims int
			up := upstream.New()
			up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"code":0,"data":{"tasks":[{"task_code":"chat_5","accept_status":"completed","progress":{"current":5,"target":5}}]}}`
				if strings.HasSuffix(r.URL.Path, "/claim") {
					claims++
					body = `{"code":0,"data":{"credit":100}}`
					if claimFails {
						body = `{"code":500,"msg":"claim rejected"}`
					}
				} else if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/growth/tasks") {
					t.Errorf("completed task repeated action: %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			store := activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})
			p := New(Config{Pool: pool.New(""), Upstream: up, Activity: store})
			_, err := p.runGrowthQueued(&auth.Auth{UID: "u1", AccessToken: "at"}, "chat_5")
			if claims != 1 || (err != nil) != claimFails {
				t.Fatalf("claims=%d err=%v", claims, err)
			}
			history, queryErr := store.Query(activity.Filter{Task: activity.TaskGrowth})
			if queryErr != nil || history.Total != 1 || history.Records[0].OK == claimFails {
				t.Fatalf("history=%+v err=%v", history, queryErr)
			}
		})
	}
}

func TestBenefitsPendingUsesCurrentTaskSnapshot(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "available", true: "unknown"}[failed], func(t *testing.T) {
			pl := pool.New("")
			pl.Add(&auth.Auth{UID: "13800138000", AccessToken: "at"})
			up := upstream.New()
			up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := `{"code":0,"data":{"tasks":[{"task_code":"chat_5","progress":{"current":0,"target":5}}]}}`
				if failed {
					body = `{"code":500,"msg":"unavailable"}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			p := New(Config{Pool: pl, Upstream: up, APIKey: "k"})
			rr := taskRequest(p, "GET", "benefits", "")
			var data struct {
				Accounts []struct {
					Pending int  `json:"growth_pending"`
					Known   bool `json:"growth_pending_known"`
				} `json:"accounts"`
				Summary struct {
					Pending int `json:"growth_pending"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
				t.Fatal(err)
			}
			want := 1
			if failed {
				want = 0
			}
			if rr.Code != 200 || len(data.Accounts) != 1 || data.Accounts[0].Pending != want || data.Accounts[0].Known == failed || data.Summary.Pending != want {
				t.Fatalf("response=%s", rr.Body.String())
			}
		})
	}
}

func TestBenefitBatchWaitsForAllHistoryAndPreservesNewerRun(t *testing.T) {
	store := activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName()), Async: true})
	t.Cleanup(func() { _ = store.Close() })
	p := New(Config{Pool: pool.New(""), Activity: store})
	started := time.Now()
	p.batchRuns = map[string]*batchRun{"checkin": {Kind: "checkin", StartedAt: started, Running: true}}
	for i := 0; i < 600; i++ {
		if err := store.Append(activity.Record{UID: "u1", Task: activity.TaskCheckin, OK: i%2 == 0}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	close(done)
	p.finishBenefitBatch("checkin", started, done)
	run := p.batchRuns["checkin"]
	if !run.Done || run.Running || run.Total != 600 || run.OK != 300 || run.Failed != 300 {
		t.Fatalf("finished batch=%+v", run)
	}
	newer := &batchRun{Kind: "checkin", StartedAt: time.Now(), Running: true}
	p.batchRuns["checkin"] = newer
	p.finishBenefitBatch("checkin", started, done)
	if p.batchRuns["checkin"] != newer {
		t.Fatal("old history monitor replaced a newer batch")
	}
}

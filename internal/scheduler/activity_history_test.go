package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func historyStore(t *testing.T) *activity.Store {
	t.Helper()
	return activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})
}

// TestCheckinHistoryManualSuccess 手动签到成功必须落一条 manual/checkin/ok。
func TestCheckinHistoryManualSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/billing/meter/daily-checkin" {
			w.Write([]byte(`{"code":0,"msg":"ok"}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":50}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "甲", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	store := historyStore(t)
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, Activity: store})
	s.RunCheckinNow()

	got, err := store.Query(activity.Filter{Task: activity.TaskCheckin})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 {
		t.Fatalf("records=%d want 1", got.Total)
	}
	rec := got.Records[0]
	if rec.UID != "u1" || rec.Nickname != "甲" || rec.Trigger != activity.TriggerManual || !rec.OK {
		t.Errorf("unexpected record: %#v", rec)
	}
}

// TestActivityHistoryScheduledTrigger runBatch 路径必须标记为 schedule。
func TestActivityHistoryScheduledTrigger(t *testing.T) {
	fastActivity(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			w.Write([]byte(`{"code":0}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	store := historyStore(t)
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, Activity: store})
	s.runActivity(context.Background(), activity.TriggerSchedule)

	got, err := store.Query(activity.Filter{Task: activity.TaskActivity})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || got.Records[0].Trigger != activity.TriggerSchedule || !got.Records[0].OK {
		t.Fatalf("unexpected records: %#v", got.Records)
	}
}

// TestKeepaliveHistoryFailure 保活失败也要记录 failed，且不影响后续账号。
func TestKeepaliveHistoryFailure(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Refresh-Token") == "bad-rt" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`boom`))
			return
		}
		w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	badPath := filepath.Join(t.TempDir(), "bad.json")
	okPath := filepath.Join(t.TempDir(), "ok.json")
	for _, path := range []string{badPath, okPath} {
		if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p.Add(&auth.Auth{UID: "bad", AccessToken: "at", RefreshToken: "bad-rt", ExpiresAt: 1, FilePath: badPath})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "ok-rt", ExpiresAt: 1, FilePath: okPath})
	store := historyStore(t)
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, Activity: store})
	s.RunKeepaliveNow()

	got, err := store.Query(activity.Filter{Task: activity.TaskKeepalive})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 2 {
		t.Fatalf("records=%d want 2 (calls=%d)", got.Total, calls)
	}
	byUID := map[string]activity.Record{}
	for _, rec := range got.Records {
		byUID[rec.UID] = rec
	}
	if byUID["bad"].OK {
		t.Fatalf("bad account should be failed: %#v", byUID["bad"])
	}
	// ok 账号 refresh 成功，但临时目录里没有可写回的 auth 文件；此时记录为失败
	// （写回失败），并发后续账号的语义不变。
	if !byUID["ok"].OK {
		t.Fatalf("ok account should be recorded as ok: %#v", byUID["ok"])
	}
}

// TestActivityStoreFailureDoesNotBlockTasks 历史写入失败（路径为目录）不能影响任务。
func TestActivityStoreFailureDoesNotBlockTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":50}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	store := activity.New(activity.Config{Path: t.TempDir()})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, Activity: store})
	s.RunCheckinNow() // 不应 panic，也不应中止余额刷新

	st, ok := p.Status("u1")
	if !ok || st.Credits != 50 {
		t.Fatalf("credits not refreshed after history failure: %#v ok=%v", st, ok)
	}
}

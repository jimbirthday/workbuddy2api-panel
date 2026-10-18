package panel

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func accountOpsFixture(t *testing.T, handler http.HandlerFunc) (*Panel, *pool.Pool, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.WebBaseCN = srv.URL
	up.HTTP.Transport = srv.Client().Transport
	up.ChatHTTP = srv.Client()

	dir := t.TempDir()
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", Nickname: "旧名", FilePath: filepath.Join(dir, "workbuddy-u1.json")}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl := pool.New(filepath.Join(dir, "state.json"))
	pl.Add(a)
	return New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up}), pl, a
}

func accountOpsPost(t *testing.T, p *Panel, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestAccountTestSuccessRequiresNonEmptyReply(t *testing.T) {
	p, pl, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n"))
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/test", `{"model":"cn:deepseek-v4.1-flash"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), `"reply":"pong"`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Cooling {
		t.Fatalf("success must not cool account: %+v", st)
	}
}

func TestAccountTestEmptyReplyIsFailure(t *testing.T) {
	p, pl, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/test", "")
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("empty reply must be failure, body=%s", rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Cooling {
		t.Fatalf("empty reply must not cool account: %+v", st)
	}
}

func TestAccountTest6004LocksModelOnly(t *testing.T) {
	p, pl, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"code\":6004,\"msg\":\"model usage limit exceeded\"}\n\ndata: [DONE]\n\n"))
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/test", `{"model":"glm-5.3"}`)
	if !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("6004 must fail: %s", rec.Body.String())
	}
	st, _ := pl.Status("u1")
	if st.Cooling || !st.Until.IsZero() {
		t.Fatalf("6004 must not cool the whole account: %+v", st)
	}
	var locked bool
	for _, row := range st.RateLimitedModels {
		if row.Model == "glm-5.3" {
			locked = true
		}
	}
	if !locked {
		t.Fatalf("6004 must lock the tested model: %+v", st.RateLimitedModels)
	}
}

func TestAccountTestHTTPErrorDoesNotPenalizeAccount(t *testing.T) {
	var calls atomic.Int64
	p, pl, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11101,"msg":"bad params"}`))
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/test", `{"model":"deepseek-v4.1-flash"}`)
	if !strings.Contains(rec.Body.String(), `"ok":false`) || calls.Load() != 1 {
		t.Fatalf("code=%d body=%s calls=%d", rec.Code, rec.Body.String(), calls.Load())
	}
	st, _ := pl.Status("u1")
	if st.Cooling || st.BreakerFails != 0 || st.ErrTotal != 0 {
		t.Fatalf("non-6004 failure must not penalize account: %+v", st)
	}
}

func TestAccountTestUnknownAccountAndAuth(t *testing.T) {
	p, _, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {})
	if rec := accountOpsPost(t, p, "/panel/api/accounts/missing/test", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing account code=%d want 404", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/test", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code=%d want 401", rec.Code)
	}
}

func TestAccountSyncProfileUpdatesNickname(t *testing.T) {
	p, pl, a := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/console/account") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"u1","nickname":"新名字","phoneNumber":"13800000000"}}`))
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/sync_profile", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"nickname":"新名字"`) || !strings.Contains(rec.Body.String(), `"updated":true`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Nickname != "新名字" {
		t.Fatalf("pool nickname=%q", st.Nickname)
	}
	if a.Nickname != "新名字" {
		t.Fatalf("auth nickname=%q", a.Nickname)
	}
	// 同名再次同步：落盘成功但 updated=false，不能误报为失败。
	rec = accountOpsPost(t, p, "/panel/api/accounts/u1/sync_profile", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"updated":false`) {
		t.Fatalf("unchanged sync code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountSyncProfileFailureIsExplicit(t *testing.T) {
	p, _, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":1002,"msg":"unauthorized"}`))
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/sync_profile", "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountTestModelRealmMismatch(t *testing.T) {
	p, _, _ := accountOpsFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not reach upstream")
	})
	rec := accountOpsPost(t, p, "/panel/api/accounts/u1/test", `{"model":"global:glm-5.3"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

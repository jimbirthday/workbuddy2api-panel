package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestTasksOverviewScopeAndRefs(t *testing.T) {
	pl := pool.New("")
	cn := &auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at"}
	cn2 := &auth.Auth{UID: "13900139000", Nickname: "冬雪", AccessToken: "at"}
	global := &auth.Auth{UID: "global-user", Nickname: "Global", AccessToken: "at", Domain: "www.workbuddy.ai"}
	ent := &auth.Auth{UID: "enterprise-user", Nickname: "企业", AccessToken: "at", EnterpriseID: "ent-1"}
	pl.Add(cn)
	pl.Add(cn2)
	pl.Add(global)
	pl.Add(ent)
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("all code=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "13800138000") {
		t.Fatalf("tasks overview leaked full UID: %s", rr.Body.String())
	}
	var all struct {
		Accounts []tasksAccount `json:"accounts"`
		Focus    string         `json:"focus"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Accounts) != 2 {
		t.Fatalf("accounts=%+v want 2 CN personal", all.Accounts)
	}
	if all.Focus != "" {
		t.Fatalf("all focus=%q want empty (batch scope)", all.Focus)
	}

	ref := benefitRefFor(cn.UID)
	req = httptest.NewRequest(http.MethodGet, "/panel/api/tasks?focus="+ref, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("focus code=%d body=%s", rr.Code, rr.Body.String())
	}
	var one struct {
		Accounts []tasksAccount `json:"accounts"`
		Focus    string         `json:"focus"`
		Tasks    []any          `json:"tasks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if len(one.Accounts) != 1 || one.Accounts[0].Ref != ref {
		t.Fatalf("focus accounts=%+v want only ref %s", one.Accounts, ref)
	}
	if one.Tasks == nil {
		t.Fatal("tasks must be an empty array, not null")
	}
}

func TestTasksRunSingleAndBatchResults(t *testing.T) {
	pl := pool.New("")
	cn := &auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at", RefreshToken: "rt"}
	cn2 := &auth.Auth{UID: "13900139000", Nickname: "冬雪", AccessToken: "at", RefreshToken: "rt"}
	pl.Add(cn)
	pl.Add(cn2)
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":0,"msg":"ok","data":{}}`)),
		}, nil
	})}
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})
	ref := benefitRefFor(cn.UID)

	for _, tc := range []struct {
		name string
		body string
		code int
		want int
	}{
		{"single success", `{"ref":"` + ref + `","action":"checkin"}`, http.StatusOK, 1},
		{"batch success", `{"scope":"all","action":"checkin"}`, http.StatusOK, 2},
		{"unknown ref rejected", `{"ref":"not-a-ref","action":"checkin"}`, http.StatusBadRequest, 0},
		{"unknown action rejected", `{"scope":"all","action":"nope"}`, http.StatusBadRequest, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/panel/api/tasks/run", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer k")
			rr := httptest.NewRecorder()
			p.ServeHTTP(rr, req)
			if rr.Code != tc.code {
				t.Fatalf("code=%d want %d body=%s", rr.Code, tc.code, rr.Body.String())
			}
			if tc.code != http.StatusOK {
				return
			}
			var out struct {
				AccountsCount int              `json:"accounts_count"`
				Results       []tasksRunResult `json:"results"`
				Logs          []string         `json:"logs"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.AccountsCount != tc.want || len(out.Results) != tc.want || len(out.Logs) != tc.want {
				t.Fatalf("count=%d results=%d logs=%d body=%s", out.AccountsCount, len(out.Results), len(out.Logs), rr.Body.String())
			}
			for _, item := range out.Results {
				if !item.OK {
					t.Fatalf("checkin result=%+v want OK", item)
				}
			}
		})
	}
}

// TestTasksOverviewFocusDoesNotSweepWholePool 聚焦单账号时不得对整池做成长扫描。
// 旧实现把整池 statuses 传进 benefitsGrowthSnapshot，切一个账号就给 470 号各打
// 2 个端点（并与并行加载的 /benefits 完全重复）。这里用「每个账号的任务列表
// 端点只允许被访问一次」的探针钉住：聚焦 A 时不碰 B。
func TestTasksOverviewFocusDoesNotSweepWholePool(t *testing.T) {
	pl := pool.New("")
	target := &auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at1"}
	other := &auth.Auth{UID: "13900139000", Nickname: "冬雪", AccessToken: "at2"}
	pl.Add(target)
	pl.Add(other)

	var targetCalls, otherCalls atomic.Int64
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// 用 access token 区分「为哪个账号发的请求」。
		if r.Header.Get("Authorization") == "Bearer at2" {
			otherCalls.Add(1)
		} else {
			targetCalls.Add(1)
		}
		body := `{"code":0,"msg":"ok","data":{"tasks":[]}}`
		switch {
		case strings.Contains(r.URL.Path, "/activity/growth/streak"):
			body = `{"code":0,"msg":"ok","data":{"streak":{"days":1}}}`
		case strings.Contains(r.URL.Path, "/activity/growth/lottery/summary"):
			body = `{"code":0,"msg":"ok","data":{"chances":0}}`
		case strings.Contains(r.URL.Path, "/activity/growth/energy"):
			body = `{"code":0,"msg":"ok","data":{"balance":0}}`
		case strings.Contains(r.URL.Path, "/buddy/travel/status"):
			body = `{"code":0,"msg":"ok","data":{"state":"home"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/tasks?focus="+benefitRefFor(target.UID), nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if targetCalls.Load() == 0 {
		t.Fatal("聚焦账号的成长明细未查询（应至少打一次上游）")
	}
	if n := otherCalls.Load(); n != 0 {
		t.Fatalf("聚焦 A 却对账号 B 打了 %d 次上游（单焦点触发全池扫描）", n)
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

func TestBackgroundModelAlias(t *testing.T) {
	cases := []struct {
		name, model, names, target string
		want, hit                  string
	}{
		{"exact case-insensitive", "GPT-5.6-TERRA", "gpt-5.6-terra", "deepseek-v4.1-flash", "deepseek-v4.1-flash", "1"},
		{"glob", "gpt-5.6-luna", "gpt-5.6-*", "deepseek-v4.1-flash", "deepseek-v4.1-flash", "1"},
		{"realm prefix stripped for match", "global:gpt-5.6-terra", "gpt-5.6-terra", "cn:deepseek-v4.1-flash", "cn:deepseek-v4.1-flash", "1"},
		{"not matched", "glm-5.3", "gpt-5.6-*", "deepseek-v4.1-flash", "", "0"},
		{"empty names disabled", "gpt-5.6-terra", "", "deepseek-v4.1-flash", "", "0"},
		{"empty target disabled", "gpt-5.6-terra", "gpt-5.6-terra", "", "", "0"},
		{"empty model", "", "gpt-5.6-terra", "deepseek-v4.1-flash", "", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := backgroundModelAlias(tc.model, tc.names, tc.target)
			if tc.hit == "1" {
				if got.Empty || got.Target != tc.want {
					t.Fatalf("alias(%q)=%+v want target %q", tc.model, got, tc.want)
				}
			} else if !got.Empty {
				t.Fatalf("alias(%q)=%+v want no hit", tc.model, got)
			}
		})
	}
}

func TestRewriteBackgroundModelPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-terra","stream":true,"messages":[{"role":"user","content":"hi"}],"custom":{"keep":1}}`)
	out, ok := rewriteBackgroundModel(body, "gpt-5.6-terra", "deepseek-v4.1-flash")
	if !ok {
		t.Fatal("rewrite did not report a hit")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "deepseek-v4.1-flash" || obj["stream"] != true {
		t.Fatalf("unexpected rewrite: %s", out)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 || obj["custom"] == nil {
		t.Fatalf("other fields changed: %s", out)
	}
}

// 离线校验：结构化 body 的 model 已被改名时，上游收到的必须是目标模型；
// 同时请求日志保留原模型名，便于核对后台请求确实被重定向。
func TestBackgroundModelAliasEndToEnd(t *testing.T) {
	var gotBody []byte
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	// newFakeUpstream 的 roundTripper 不暴露请求体，这里用独立 client 抓取。
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{
		Keys:                  []httpauth.Key{{ID: "client", Key: "sk-test", Enabled: true}},
		BackgroundModelNames:  "gpt-5.6-*",
		BackgroundModelTarget: "deepseek-v4.1-flash",
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})
	body := []byte(`{"model":"gpt-5.6-terra","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body invalid: %v (%s)", err, gotBody)
	}
	if sent["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("upstream model=%v want deepseek-v4.1-flash (body=%s)", sent["model"], gotBody)
	}
	if st, _ := p.Status("u1"); st.TokenUsage.LastModel != "deepseek-v4.1-flash" {
		t.Fatalf("account last model=%q, want actual target", st.TokenUsage.LastModel)
	}
}

// 红线：Key 的模型白名单必须在改名之前判定。受限 Key 只能使用白名单里的
// gpt-5.6-terra；即使该名字命中了后台别名，也不能借改名访问白名单外的
// deepseek-v4-pro。请求应直接 400，且不触达上游。
func TestBackgroundModelAliasCannotBypassKeyModelAllowlist(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{
		Keys: []httpauth.Key{{ID: "limited", Key: "sk-limited", Enabled: true,
			Models: []string{"gpt-5.6-terra"}}},
		BackgroundModelNames:  "gpt-5.6-*",
		BackgroundModelTarget: "deepseek-v4-pro",
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})
	body := []byte(`{"model":"gpt-5.6-terra","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-limited")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400; body=%s", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Fatalf("upstream must not be reached, got %d calls", calls)
	}
}

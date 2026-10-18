package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// 协议入口必须与 chat 共用 trace：X-Request-Id、path、key_id、usage 都落账，
// 且 200 流里只有 role-only + [DONE] 时不能记成成功。
func TestProtocolResponsesTraceAndEmptyStreamNotSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n", true
	})
	live := livecfg.New(livecfg.Snapshot{Keys: []httpauth.Key{{ID: "client-a", Key: "sk-test", Enabled: true}}})
	rec := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:   up,
		Live:       live,
		RequestLog: rec,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Header().Get("X-Request-Id") == "" {
		t.Fatalf("protocol route bypassed trace: headers=%v", rr.Header())
	}
	snap := rec.Snapshot()
	if len(snap.Recent) != 1 {
		t.Fatalf("trace not recorded: %+v", snap.Recent)
	}
	e := snap.Recent[0]
	if e.Path != "/v1/responses" || e.KeyID != "client-a" {
		t.Fatalf("trace path/key mismatch: %+v", e)
	}
	if e.OK || e.Outcome != reqlog.OutcomeStreamError {
		t.Fatalf("role-only empty stream recorded as success: %+v", e)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("empty stream must return a native JSON error, code=%d body=%s", rr.Code, rr.Body)
	}
	if rr.Code != http.StatusBadGateway || body["error"] == nil {
		t.Fatalf("empty stream must be a real 502, code=%d body=%s", rr.Code, rr.Body)
	}
	if strings.Contains(rr.Body.String(), "response.completed") || strings.Contains(rr.Body.String(), "event: ") {
		t.Fatalf("empty stream must not fake a 200 SSE success: %s", rr.Body)
	}
}

// 上游 HTTP>=400 必须按协议 native 错误信封返回，不能先开 200 SSE。
func TestProtocolUpstreamHTTPErrorUsesNativeEnvelope(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return http.StatusTooManyRequests, `{"code":6004,"msg":"rate limited"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "sk-test",
	})
	cases := []struct {
		path, body, want string
	}{
		{"/v1/responses", `{"model":"glm-5.2","input":"hi","stream":true}`, "error"},
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, `"type":"error"`},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer sk-test")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code < 400 || !strings.Contains(rr.Body.String(), tc.want) {
			t.Errorf("%s native error missing: code=%d body=%s", tc.path, rr.Code, rr.Body)
		}
		if strings.Contains(rr.Body.String(), "event: ") || strings.Contains(rr.Body.String(), "message_stop") {
			t.Errorf("%s emitted SSE on upstream HTTP error: %s", tc.path, rr.Body)
		}
	}
}

// 跨协议：首号 6004 只锁该模型，Responses 与 Messages 都要继续换号完成；不能因单模型限流冻结整个账单调度。
func TestProtocolModelRateLimitRotatesAcrossBothProtocols(t *testing.T) {
	cases := []struct {
		path, body string
	}{
		{"/v1/responses", `{"model":"glm-5.2","input":"hi","stream":true}`},
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		calls := 0
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			calls++
			if strings.Contains(authz, "at-bad") {
				return 200, "data: {\"error\":{\"message\":\"rate limited\",\"code\":6004}}\n\ndata: [DONE]\n\n", true
			}
			return 200, sseOK, true
		})
		p := testPoolWith(
			&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
			&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
		)
		p.SetCredits("bad", 2000, 0)
		p.SetCredits("good", 1000, 0)
		h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-test"})
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer sk-test")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if calls < 2 {
			t.Fatalf("%s stopped rotation on model limit: calls=%d", tc.path, calls)
		}
		out := rr.Body.String()
		if tc.path == "/v1/responses" && (!strings.Contains(out, "response.completed") || strings.Contains(out, "event: error")) {
			t.Fatalf("%s did not complete after rotation: %s", tc.path, out)
		}
		if tc.path == "/v1/messages" && (!strings.Contains(out, "message_stop") || strings.Contains(out, "event: error")) {
			t.Fatalf("%s did not complete after rotation: %s", tc.path, out)
		}
		if p.UIDAvailableForModel("bad", "glm-5.2", "") {
			t.Fatalf("%s did not apply per-model cooldown to bad account", tc.path)
		}
		if !p.UIDAvailableForModel("bad", "kimi-k3", "") {
			t.Fatalf("%s froze the whole account instead of one model", tc.path)
		}
	}
}

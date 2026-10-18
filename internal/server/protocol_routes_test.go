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
	"github.com/linguo2625469/workbuddy2api-panel/internal/protocol"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

func TestResponsesNonStreamTranslatesUsageAndTools(t *testing.T) {
	var seenBody []byte
	up := newFakeUpstream(t, func(auth string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seenBody, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, APIKey: "sk-test",
	})
	reqBody := `{"model":"glm-5.2","input":"say hi","instructions":"be brief","max_output_tokens":64,"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"Bash","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp["object"] != "response" || resp["output_text"] != "你好" {
		t.Errorf("unexpected response: %v", resp)
	}
	if resp["usage"] == nil {
		t.Errorf("usage missing: %v", resp)
	}
	var chat map[string]any
	if err := json.Unmarshal(seenBody, &chat); err != nil {
		t.Fatalf("upstream body invalid: %v", err)
	}
	if chat["model"] != "glm-5.2" || chat["max_tokens"] == nil || chat["reasoning_effort"] != "high" {
		t.Errorf("chat body lost fields: %v", chat)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) < 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("instructions not mapped to system: %v", msgs)
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "function" {
		t.Errorf("tools not translated: %v", tools)
	}
}

func TestResponsesStreamTranslatesAndDoesNotFakeSuccess(t *testing.T) {
	stream := "data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, stream, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "response.completed") || !strings.Contains(body, "hi") {
		t.Fatalf("bad stream code=%d body=%s", rec.Code, body)
	}
	if !strings.Contains(body, "response.output_text.delta") {
		t.Errorf("text delta missing: %s", body)
	}
}

func TestResponsesStreamErrorIsExplicit(t *testing.T) {
	stream := "data: {\"error\":{\"message\":\"rate limited\",\"code\":\"6004\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, stream, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(body, `"error"`) {
		t.Fatalf("pre-content error frame must be a native 429, code=%d body=%s", rec.Code, body)
	}
	if strings.Contains(body, "event: ") || strings.Contains(body, "response.completed") {
		t.Fatalf("error frame must not be a fake success: %s", body)
	}
}

func TestMessagesNonStreamAndCountTokens(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	body := `{"model":"glm-5.2","max_tokens":32,"system":"be brief","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("json: %v", err)
	}
	if msg["type"] != "message" || msg["stop_reason"] != "end_turn" || msg["usage"] == nil {
		t.Errorf("bad messages response: %v", msg)
	}
	req = httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-test")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("count code=%d body=%s", rec.Code, rec.Body)
	}
	var count map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &count); err != nil {
		t.Fatalf("count json: %v", err)
	}
	if count["input_tokens"].(float64) < 1 || count["estimate"] != true {
		t.Errorf("bad count response: %v", count)
	}
}

func TestMessagesStreamToolUse(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, stream, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	body := `{"model":"glm-5.2","max_tokens":32,"stream":true,"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"ls"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	if !strings.Contains(out, "content_block_start") || !strings.Contains(out, "tool_use") || !strings.Contains(out, "call_1") {
		t.Fatalf("tool stream missing native tool_use: %s", out)
	}
}

func TestProtocolRoutesAuthEnvelope(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }), APIKeys: []httpauth.Key{{ID: "k1", Key: "sk-test", Enabled: true}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"x","max_tokens":1,"messages":[]}`)))
	var msg map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if rec.Code != 401 || msg["type"] != "error" {
		t.Errorf("messages auth envelope: code=%d body=%s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"x","input":"hi"}`)))
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 401 || resp["error"] == nil {
		t.Errorf("responses auth envelope: code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestProtocolRoutesRejectInvalidJSON(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }), APIKey: "sk-test"})
	for _, path := range []string{"/v1/responses", "/v1/messages", "/v1/messages/count_tokens"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{not json`))
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("%s code=%d body=%s", path, rec.Code, rec.Body)
		}
	}
}

func TestProtocolCountTokensDoesNotCallUpstream(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up, APIKey: "sk-test",
	})
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"你好 world"}]}`))
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || calls != 0 {
		t.Fatalf("count_tokens must be local: code=%d calls=%d body=%s", rec.Code, calls, rec.Body)
	}
	if rec.Header().Get("X-Token-Count-Estimate") != "true" || !strings.Contains(rec.Body.String(), `"estimate":true`) {
		t.Fatalf("estimate marker missing: headers=%v body=%s", rec.Header(), rec.Body)
	}
}

func TestProtocolCountTokensNotRequestLogged(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	rec := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up, APIKey: "sk-test", RequestLog: rec,
	})
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-test")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	if got := len(rec.Snapshot().Recent); got != 0 {
		t.Fatalf("count_tokens must not enter chat request archive: %d", got)
	}
}

func TestProtocolStreamWriterHandlesSplitFrames(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, stream, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	pw := newProtocolStreamWriter(&fragmentWriter{ResponseWriter: rec, size: 7}, protoResponses, protocol.ResponsesMeta{Model: "m"})
	h.chatCompletions(pw, req)
	pw.Finish()
	out := rec.Body.String()
	if !strings.Contains(out, "response.output_text.delta") || !strings.Contains(out, "response.completed") || !strings.Contains(out, "hello") {
		t.Fatalf("split frames lost: %s", out)
	}
}

type fragmentWriter struct {
	http.ResponseWriter
	size int
}

func (f *fragmentWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := f.size
		if n > len(p) {
			n = len(p)
		}
		if _, err := f.ResponseWriter.Write(p[:n]); err != nil {
			return total - len(p), err
		}
		p = p[n:]
	}
	return total, nil
}

func TestProtocolRoutesRealmAndModelGate(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	live := livecfg.New(livecfg.Snapshot{Keys: []httpauth.Key{{
		ID: "restricted", Key: "sk-test", Enabled: true, Realm: "cn", Models: []string{"glm-*"},
	}}})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up, Live: live,
	})
	cases := []struct{ path, body, want string }{
		{"/v1/responses", `{"model":"global:kimi-k3","input":"hi"}`, "realm_not_allowed"},
		{"/v1/responses", `{"model":"kimi-k3","input":"hi"}`, "model_not_allowed"},
		{"/v1/messages", `{"model":"kimi-k3","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, "not allowed to use model"},
		{"/v1/messages/count_tokens", `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`, "not allowed to use model"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
		req.Header.Set("x-api-key", "sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s code=%d body=%s want %s", c.path, rec.Code, rec.Body, c.want)
		}
	}
}

func TestProtocolNonStreamErrorPreservesRequestID(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `data: {"error":{"message":"bad params","code":11101,"requestId":"req-42","extError":{"code":"400001","message":"detail"}}}` + "\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	cases := []struct {
		path, body string
	}{
		{"/v1/responses", `{"model":"glm-5.2","input":"hi"}`},
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		out := rec.Body.String()
		// The upstream HTTP body is preserved verbatim as the native error
		// message; native envelopes add their own type while keeping the raw
		// JSON string intact so requestId and extError stay reachable.
		if rec.Code < 400 || !strings.Contains(out, "req-42") || !strings.Contains(out, "400001") {
			t.Errorf("%s lost requestId/extError: code=%d body=%s", tc.path, rec.Code, out)
		}
	}
}

func TestProtocolNonStreamErrorPreservesStatusAndEnvelope(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"error\":{\"message\":\"bad params\",\"code\":11101,\"requestId\":\"r1\"}}\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		body := `{"model":"glm-5.2","input":"hi"}`
		if path == "/v1/messages" {
			body = `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		}
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("%s must not fake success: code=%d body=%s", path, rec.Code, rec.Body)
		}
		if path == "/v1/messages" && !strings.Contains(rec.Body.String(), `"type":"error"`) {
			t.Errorf("messages envelope: %s", rec.Body)
		}
		if path == "/v1/responses" && !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("responses envelope: %s", rec.Body)
		}
	}
}

func TestProtocolNonStreamRejectsEmptyChoices(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, "data: {\"id\":\"x\",\"model\":\"m\",\"choices\":[]}\n\ndata: [DONE]\n\n", true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		body := `{"model":"glm-5.2","input":"hi"}`
		if path == "/v1/messages" {
			body = `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		}
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 400 || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s empty choices must fail, code=%d body=%s", path, rec.Code, rec.Body)
		}
	}
}

func TestProtocolStreamWriterRejectsUnboundedPendingFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	pw := newProtocolStreamWriter(rec, protoResponses, protocol.ResponsesMeta{Model: "m"})
	chunk := bytes.Repeat([]byte("x"), 64*1024)
	var err error
	for i := 0; i < 32 && err == nil; i++ {
		_, err = pw.Write(chunk)
	}
	if err == nil {
		t.Fatalf("unterminated SSE line must be bounded, pending=%d", len(pw.pending))
	}
}

func TestMessagesStreamDoesNotStopRotationOnSingleModelLimit(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		if calls == 1 {
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
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if calls < 2 {
		t.Fatalf("single-model limit stopped rotation: calls=%d", calls)
	}
	if !strings.Contains(rec.Body.String(), "message_stop") || strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("second account did not complete: %s", rec.Body)
	}
}

func TestResponsesNonStreamRotatesOnSingleModelLimit(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(auth string) (int, string, bool) {
		calls++
		if auth == "Bearer at-bad" {
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
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if calls < 2 {
		t.Fatalf("single-model limit stopped rotation: calls=%d", calls)
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Fatalf("second account did not complete: code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestProtocolStreamErrorPreservesRequestID(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"glm-5.2","input":"hi","stream":true}`},
		{"/v1/messages", `{"model":"glm-5.2","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		up := newFakeUpstream(t, func(string) (int, string, bool) {
			return 200, "data: {\"error\":{\"message\":\"rate limited\",\"code\":6004,\"requestId\":\"req-stream-9\"}}\n\ndata: [DONE]\n\n", true
		})
		h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up, APIKey: "sk-test"})
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer sk-test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		out := rec.Body.String()
		if rec.Code < 400 || !strings.Contains(out, "req-stream-9") {
			t.Errorf("%s stream error lost requestId: code=%d body=%s", tc.path, rec.Code, out)
		}
	}
}

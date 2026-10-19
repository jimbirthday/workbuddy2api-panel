package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/protocol"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func regressionRequestBody(path string) string {
	switch path {
	case "/v1/responses":
		return `{"model":"glm-5.2","input":"hi","stream":true}`
	default:
		return `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	}
}

func regressionProtocolEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func assertRegressionFailedStream(t *testing.T, rr *httptest.ResponseRecorder, rec *reqlog.Recorder) {
	t.Helper()
	body := rr.Body.String()
	if !strings.Contains(body, "partial answer") || !strings.Contains(body, "event: error") {
		t.Fatalf("partial answer and stream error must reach client: %s", body)
	}
	if strings.Contains(body, "event: response.completed") || strings.Contains(body, "event: message_stop") {
		t.Fatalf("failed stream must not emit normal completion: %s", body)
	}
	e := rec.Snapshot().Recent[0]
	if e.OK || e.Outcome != reqlog.OutcomeStreamError {
		t.Fatalf("failed stream recorded as success: %+v", e)
	}
}

func TestStreamRegressionReadErrorMustNotComplete(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&errReader{data: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}]}\n\n")})}, nil
			})}, ChatBaseCN: "https://fake.example"}
			rec := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			assertRegressionFailedStream(t, rr, rec)
			st, _ := p.Status("u1")
			if st.SuccessCount != 0 || !st.LastSuccessTime.IsZero() {
				t.Fatalf("upstream read failure marked account successful: %+v", st)
			}
		})
	}
}

func TestStreamRegressionWriteErrorMustNotCountSuccess(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
			rec := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			h.ServeHTTP(newFailingResponseWriter(), httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			st, _ := p.Status("u1")
			e := rec.Snapshot().Recent[0]
			if st.SuccessCount != 0 || !st.LastSuccessTime.IsZero() || st.ConsecutiveFails != 0 || e.OK || e.Outcome != reqlog.OutcomeInterrupted {
				t.Fatalf("all downstream writes failed but success_count=%d outcome=%s OK=%v", st.SuccessCount, e.Outcome, e.OK)
			}
		})
	}
}

func TestStreamRegressionCustomToolDeltasMatchDone(t *testing.T) {
	rr := httptest.NewRecorder()
	s := protocol.NewResponsesStream(rr, protocol.ResponsesMeta{Model: "m", CustomTools: map[string]bool{"apply_patch": true}})
	for i, arg := range []string{`{"input":"`, `line one\n    line two"}`} {
		fn := map[string]any{"arguments": arg}
		if i == 0 {
			fn["name"] = "apply_patch"
		}
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "function": fn}}}}}})
		s.Feed(raw)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	var delta, done string
	var deltas, dones int
	for _, e := range regressionProtocolEvents(t, rr.Body.String()) {
		if e["type"] == "response.custom_tool_call_input.delta" {
			delta += e["delta"].(string)
			deltas++
		}
		if e["type"] == "response.custom_tool_call_input.done" {
			done = e["input"].(string)
			dones++
		}
	}
	if deltas == 0 || dones != 1 || delta != done || done != "line one\n    line two" {
		t.Fatalf("custom tool delta=%q (%d events), done=%q (%d events)", delta, deltas, done, dones)
	}
}

func TestStreamRegressionLegacyFunctionCallContinuation(t *testing.T) {
	for _, kind := range []string{"responses", "messages"} {
		t.Run(kind, func(t *testing.T) {
			rr := httptest.NewRecorder()
			var s interface {
				Feed([]byte)
				Finish() error
			}
			if kind == "responses" {
				s = protocol.NewResponsesStream(rr, protocol.ResponsesMeta{Model: "m"})
			} else {
				s = protocol.NewMessagesStream(rr, "m")
			}
			for _, fn := range []map[string]any{{"name": "echo", "arguments": `{"text":`}, {"arguments": `"hello"}`}} {
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"function_call": fn}}}})
				s.Feed(raw)
			}
			if err := s.Finish(); err != nil {
				t.Fatal(err)
			}
			var got string
			for _, e := range regressionProtocolEvents(t, rr.Body.String()) {
				if e["type"] == "response.function_call_arguments.delta" {
					got += e["delta"].(string)
				}
				if e["type"] == "content_block_delta" {
					d := e["delta"].(map[string]any)
					if d["type"] == "input_json_delta" {
						got += d["partial_json"].(string)
					}
				}
			}
			if got != `{"text":"hello"}` {
				t.Fatalf("legacy argument fragments lost: got=%q", got)
			}
		})
	}
}

func TestStreamRegressionLengthMustEmitIncomplete(t *testing.T) {
	rr := httptest.NewRecorder()
	s := protocol.NewResponsesStream(rr, protocol.ResponsesMeta{Model: "m"})
	s.Feed([]byte(`{"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`))
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rr.Body.String(), "event: response.incomplete") {
		t.Fatal("finish_reason=length emitted response.completed, with nested status=incomplete")
	}
	for _, e := range regressionProtocolEvents(t, rr.Body.String()) {
		if e["type"] == "response.completed" {
			t.Fatal("length-truncated response must not emit response.completed")
		}
		if e["type"] == "response.incomplete" && e["response"].(map[string]any)["status"] != "incomplete" {
			t.Fatalf("incomplete event disagrees with response status: %+v", e)
		}
	}
}

func TestStreamRegressionFullMessageFramePreservesAnswer(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, "data: {\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"full answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", true
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			if rr.Code != 200 || !strings.Contains(rr.Body.String(), "full answer") {
				t.Fatalf("message content lost: HTTP=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestStreamRegressionSSEUsageSpacing(t *testing.T) {
	for _, prefix := range []string{"data: ", "data:", "data:\t"} {
		t.Run(prefix, func(t *testing.T) {
			r := newChatStatsReaderSince(strings.NewReader(prefix+`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":0.5}}`+"\n\n"), time.Now())
			if _, err := io.Copy(io.Discard, r); err != nil {
				t.Fatal(err)
			}
			tokens, known := r.Tokens()
			credit, hasCredit := r.Credit()
			if !known || tokens != 2 || !hasCredit || credit != 0.5 || r.TTFB() <= 0 {
				t.Fatalf("valid SSE usage lost: tokens=%d known=%v credit=%v credit_known=%v", tokens, known, credit, hasCredit)
			}
		})
	}
}

func TestStreamRegressionPrematureEOFMustNotComplete(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}]}\n\n", true
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			rec := reqlog.New(reqlog.Config{})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			assertRegressionFailedStream(t, rr, rec)
			st, _ := p.Status("u1")
			if st.SuccessCount != 0 || !st.LastSuccessTime.IsZero() {
				t.Fatalf("premature EOF marked account successful: %+v", st)
			}
		})
	}
}

// A disconnect can happen only when sending buffered custom input or the final
// completion event, after every upstream frame has been read successfully.
type regressionFinalWriteFailure struct {
	*httptest.ResponseRecorder
	marker string
	fails  int
}

func (w *regressionFinalWriteFailure) Write(b []byte) (int, error) {
	if strings.Contains(string(b), w.marker) {
		w.fails++
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(b)
}

func TestStreamRegressionFinalWriteFailureMustNotCountSuccess(t *testing.T) {
	const customFrame = `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch body\"}"}}]},"finish_reason":"tool_calls"}]}`
	for _, tc := range []struct {
		name, path, marker, body, upstreamBody string
	}{
		{"responses-completed", "/v1/responses", "event: response.completed", regressionRequestBody("/v1/responses"), sseOK},
		{"messages-stop", "/v1/messages", "event: message_stop", regressionRequestBody("/v1/messages"), sseOK},
		{"custom-input", "/v1/responses", "event: response.custom_tool_call_input.delta",
			`{"model":"glm-5.2","input":"hi","stream":true,"tools":[{"type":"custom","name":"apply_patch"}]}`,
			"data: " + customFrame + "\n\ndata: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, tc.upstreamBody, true })
			rec := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			w := &regressionFinalWriteFailure{ResponseRecorder: httptest.NewRecorder(), marker: tc.marker}
			h.ServeHTTP(w, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
			if w.fails != 1 || w.Body.Len() == 0 {
				t.Fatalf("expected one final write failure after successful content: failures=%d body=%s", w.fails, w.Body)
			}
			st, _ := p.Status("u1")
			e := rec.Snapshot().Recent[0]
			if st.SuccessCount != 0 || !st.LastSuccessTime.IsZero() || st.ConsecutiveFails != 0 || e.OK || e.Outcome != reqlog.OutcomeInterrupted {
				t.Fatalf("final write failure must be interrupted without account success/penalty: status=%+v event=%+v", st, e)
			}
		})
	}
}

func TestStreamRegressionErrorFrameAfterContent(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, "data: " + `{"choices":[{"delta":{"content":"partial answer"}}]}` + "\n\ndata: " + `{"error":{"message":"upstream failed"}}` + "\n\ndata: [DONE]\n\n", true
			})
			rec := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			assertRegressionFailedStream(t, rr, rec)
		})
	}
}

func TestStreamRegressionReadFailureBeforeContent(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&errReader{})}, nil
			})}, ChatBaseCN: "https://fake.example"}
			rec := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			var body map[string]any
			if rr.Code != http.StatusBadGateway || json.Unmarshal(rr.Body.Bytes(), &body) != nil || body["error"] == nil {
				t.Fatalf("pre-content read failure must return native 502 JSON: code=%d body=%s", rr.Code, rr.Body)
			}
			e := rec.Snapshot().Recent[0]
			if e.OK || e.Outcome != reqlog.OutcomeStreamError {
				t.Fatalf("pre-content read failure recorded as success: %+v", e)
			}
		})
	}
}

func TestStreamRegressionExplicitFinishWithoutDone(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, "data: " + `{"choices":[{"delta":{"content":"full answer"},"finish_reason":"stop"}]}` + "\n\n", true
			})
			rec := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: rec})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest("POST", path, strings.NewReader(regressionRequestBody(path))))
			e := rec.Snapshot().Recent[0]
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "full answer") || !e.OK || e.Outcome != reqlog.OutcomeSuccess {
				t.Fatalf("explicit finish without DONE must succeed: code=%d body=%s event=%+v", rr.Code, rr.Body, e)
			}
		})
	}
}

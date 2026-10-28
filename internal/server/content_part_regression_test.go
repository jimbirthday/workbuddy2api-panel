package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// reasoningContentBody 是「原生 chat 请求带 reasoning content part」的请求体模板：
// 一条 assistant 消息，content 为数组，包含 reasoning part 与 text part。
const reasoningContentBody = `{"model":"%s","stream":true,"messages":[{"role":"assistant","content":[{"type":"reasoning","summary":[{"type":"summary_text","text":"hidden thought"}]},{"type":"text","text":"visible"}]}]}`

// newCapturingUpstream 构造一个 fake upstream：捕获出站请求体并返回指定的流式响应。
// 测试串行且单个请求，body 用闭包局部变量即可（多请求场景请改用带锁的 map）。
func newCapturingUpstream(t *testing.T, status int, body string) (*upstream.Client, *[]byte) {
	t.Helper()
	var captured []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			captured = b
			ct := "application/json"
			if strings.HasPrefix(body, "data:") || body == sseOK {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return up, &captured
}

func runChatRequest(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestChatReasoningContentPartNormalized 端到端验证：出站 normalize 会把
// deepseek 系 assistant 消息里的 reasoning content part 清理掉，并把提取到的
// 思考文本并入该消息的 reasoning_content。
func TestChatReasoningContentPartNormalized(t *testing.T) {
	up, outbound := newCapturingUpstream(t, http.StatusOK, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := runChatRequest(t, h, strings.Replace(reasoningContentBody, "%s", "deepseek-v4.1-flash", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*outbound) == 0 {
		t.Fatal("no outbound body captured")
	}

	var out map[string]any
	if err := json.Unmarshal(*outbound, &out); err != nil {
		t.Fatalf("unmarshal outbound body: %v\nbody=%s", err, *outbound)
	}
	msg := assistantMessage(t, out)
	parts := contentParts(t, msg)

	if len(parts) != 1 {
		t.Fatalf("content parts=%d want 1 (reasoning removed, text kept): %v", len(parts), parts)
	}
	part, _ := parts[0].(map[string]any)
	if typ, _ := part["type"].(string); typ != "text" {
		t.Errorf("remaining part type=%q want text", typ)
	}
	if txt, _ := part["text"].(string); txt != "visible" {
		t.Errorf("text part=%q want visible", txt)
	}
	if rc, _ := msg["reasoning_content"].(string); rc != "hidden thought" {
		t.Errorf("reasoning_content=%q want hidden thought", rc)
	}
}

// TestChatReasoningContentPartDroppedForNonDeepSeek 端到端验证：非 deepseek 模型
// 的 reasoning content part 同样被移除，但不并入 reasoning_content（不新增字段）。
func TestChatReasoningContentPartDroppedForNonDeepSeek(t *testing.T) {
	up, outbound := newCapturingUpstream(t, http.StatusOK, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := runChatRequest(t, h, strings.Replace(reasoningContentBody, "%s", "glm-5.2", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(*outbound) == 0 {
		t.Fatal("no outbound body captured")
	}

	var out map[string]any
	if err := json.Unmarshal(*outbound, &out); err != nil {
		t.Fatalf("unmarshal outbound body: %v\nbody=%s", err, *outbound)
	}
	msg := assistantMessage(t, out)
	parts := contentParts(t, msg)

	if len(parts) != 1 {
		t.Fatalf("content parts=%d want 1 (reasoning removed, text kept): %v", len(parts), parts)
	}
	part, _ := parts[0].(map[string]any)
	if typ, _ := part["type"].(string); typ != "text" {
		t.Errorf("remaining part type=%q want text", typ)
	}
	if txt, _ := part["text"].(string); txt != "visible" {
		t.Errorf("text part=%q want visible", txt)
	}
	if rc, has := msg["reasoning_content"]; has {
		t.Errorf("non-deepseek message must not gain reasoning_content, got %v", rc)
	}
}

// TestChatUnsupportedContentTypeFailsFast 端到端验证：上游 400 + unsupported
// content type 被归为 ErrBadParams，handler fail-fast——400 + bad_params + 透传
// 上游原文，不罚号、不轮转。
func TestChatUnsupportedContentTypeFailsFast(t *testing.T) {
	calls := map[string]int{}
	var mu sync.Mutex
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			calls[r.Header.Get("Authorization")]++
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`Parse message failed: unsupported content type at index 0: reasoning`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 确定性随机源 r=0 → pickWeighted 必选积分最高者；让 u1 先被选中。
	p.SetCredits("u1", 2000, 0)
	p.SetCredits("u2", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := runChatRequest(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400 fail-fast)", rec.Code, rec.Body)
	}
	respBody := rec.Body.String()
	if !strings.Contains(respBody, "bad_params") {
		t.Errorf("response must contain bad_params: %s", respBody)
	}
	if !strings.Contains(respBody, "unsupported content type") {
		t.Errorf("response must carry upstream original message: %s", respBody)
	}

	if calls["Bearer at1"] != 1 || calls["Bearer at2"] != 0 {
		t.Errorf("calls=%v want u1 1 次、u2 0 次（fail-fast 零轮转）", calls)
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 status missing")
	}
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account u1: %+v", st)
	}
}

// assistantMessage 从出站 body 中取第一条消息（测试 body 恒只有一条 assistant）。
func assistantMessage(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("outbound messages missing: %v", out)
	}
	msg, _ := msgs[0].(map[string]any)
	if msg == nil {
		t.Fatalf("outbound messages[0] is not an object: %v", msgs[0])
	}
	return msg
}

// contentParts 从出站消息中取 content 数组。
func contentParts(t *testing.T, msg map[string]any) []any {
	t.Helper()
	parts, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("outbound message content is not an array: %v", msg["content"])
	}
	return parts
}

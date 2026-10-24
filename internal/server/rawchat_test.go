package server

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rawTransport 捕获出站请求体/头并返回固定响应，供透传端点断言用。
type rawTransport struct {
	capturedBody []byte
	capturedAuth string
	status       int
	header       http.Header
	body         string
	err          error
}

func (rt *rawTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.capturedAuth = r.Header.Get("Authorization")
	if r.Body != nil {
		rt.capturedBody, _ = io.ReadAll(r.Body)
	}
	if rt.err != nil {
		return nil, rt.err
	}
	// 每次调用返回全新 body，因为上一轮透传会把它 Close/读光。
	return &http.Response{
		StatusCode: rt.status,
		Header:     rt.header,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
	}, nil
}

// TestRawChatPassthroughByteForByte POST /v2/chat 的上游响应必须逐字节透传：
// 状态码、自定义响应头、body 都不能被网关转换。body 用非 OpenAI 兼容的
// 原始 JSON（含 reasoning_content 等上游原字段），验证网关不做任何重建。
func TestRawChatPassthroughByteForByte(t *testing.T) {
	raw := `{"id":"chatcmpl-bf369aad2c18","object":"chat.completion","model":"kimi-k3.1-pro","choices":[{"index":0,"message":{"role":"assistant","content":"你好","reasoning_content":"思考链原文"}}],"usage":{"prompt_tokens":115,"completion_tokens":701,"total_tokens":816,"credit":1.19}}`
	rt := &rawTransport{
		status: 200,
		header: http.Header{
			"Content-Type":      []string{"application/json; charset=utf-8"},
			"X-Upstream-Raw":    []string{"keep-me"},
			"X-Request-Id":      []string{"req-123"},
			"Transfer-Encoding": []string{"chunked"},
		},
		body: raw,
	}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	in := `{"model":"kimi-k3.1-pro","stream":false,"messages":[{"role":"user","content":"你是谁"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v2/chat", strings.NewReader(in))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != raw {
		t.Fatalf("body must be byte-for-byte passthrough:\n got=%s\nwant=%s", got, raw)
	}
	if got := rec.Header().Get("X-Upstream-Raw"); got != "keep-me" {
		t.Errorf("custom upstream header lost: %q", got)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "req-123" {
		t.Errorf("X-Request-Id not passed through: %q", got)
	}
	if got := rec.Header().Get("Transfer-Encoding"); got != "" {
		t.Errorf("hop-by-hop Transfer-Encoding must be stripped, got %q", got)
	}
	// 请求侧也必须原样出站：不注入 prompt_cache_key、不改写 model。
	if rt.capturedBody == nil || !bytes.Equal(rt.capturedBody, []byte(in)) {
		t.Errorf("outbound body not byte-for-byte:\n got=%s\nwant=%s", rt.capturedBody, in)
	}
	if rt.capturedAuth != "Bearer at1" {
		t.Errorf("Authorization not injected, got %q", rt.capturedAuth)
	}
}

// TestRawChatUpstreamBusinessErrorPassthrough 上游 4xx/5xx 的响应体同样原样透传
// （不读 body 分类、不重写成 OpenAI error 形态）。
func TestRawChatUpstreamBusinessErrorPassthrough(t *testing.T) {
	raw := `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`
	rt := &rawTransport{status: 400, header: http.Header{"Content-Type": []string{"application/json"}}, body: raw}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != raw {
		t.Fatalf("upstream error body must be passthrough:\n got=%s\nwant=%s", got, raw)
	}
}

// TestRawChatTransportErrorCountsFailure transport 层失败走 502，且按主链路口径
// 记一次连败失败（NoteFailures），好号快速换走。
func TestRawChatTransportErrorCountsFailure(t *testing.T) {
	rt := &rawTransport{err: errors.New("dial tcp: connection refused")}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s (transport error must be 502)", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 1 {
		t.Errorf("ConsecutiveFails=%d want 1 (transport failure must feed NoteFailures)", st.ConsecutiveFails)
	}
}

// TestRawChatStripsConnectionNominatedHopHeaders 上游 Connection 头点名的额外
// 逐跳头必须一并剔除（RFC 7230 §6.1），其余自定义头保留。
func TestRawChatStripsConnectionNominatedHopHeaders(t *testing.T) {
	raw := `{"ok":true}`
	rt := &rawTransport{
		status: 200,
		header: http.Header{
			"Content-Type":  []string{"application/json"},
			"Connection":    []string{"keep-alive, X-Custom-Hop"},
			"X-Custom-Hop":  []string{"must-not-leak"},
			"X-Keep-Header": []string{"keep"},
		},
		body: raw,
	}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v2/chat", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Connection"); got != "" {
		t.Errorf("Connection must be stripped, got %q", got)
	}
	if got := rec.Header().Get("X-Custom-Hop"); got != "" {
		t.Errorf("Connection-nominated hop header must be stripped, got %q", got)
	}
	if got := rec.Header().Get("X-Keep-Header"); got != "keep" {
		t.Errorf("non-hop header must be kept, got %q", got)
	}
}

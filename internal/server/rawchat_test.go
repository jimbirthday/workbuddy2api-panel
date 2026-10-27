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
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rawTransport 捕获出站请求体/头并返回固定响应，供透传链路断言用。
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

// ptKey 返回一个 Passthrough=true 的客户端 key。
func ptKey() httpauth.Key {
	return httpauth.Key{ID: "pt", Key: "pt-key-123", Enabled: true, Passthrough: true}
}

// TestRawChatPassthroughByteForByte 透传 key 走 /v1/chat/completions 时，
// 上游响应必须逐字节透传：状态码、自定义响应头、body 都不能被网关转换。
// body 用非 OpenAI 兼容的原始 JSON（含 reasoning_content 等上游原字段）。
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
	h := NewHandler(Config{Pool: p, Upstream: up, APIKeys: []httpauth.Key{ptKey()}})

	in := `{"model":"kimi-k3.1-pro","stream":false,"messages":[{"role":"user","content":"你是谁"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(in))
	req.Header.Set("Authorization", "Bearer pt-key-123")
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
	// 请求侧：不注入 prompt_cache_key、不改写 model；唯一改写是强制 stream=true。
	if !bytes.Contains(rt.capturedBody, []byte(`"stream":true`)) {
		t.Errorf("outbound body must force stream=true:\n got=%s", rt.capturedBody)
	}
	if bytes.Contains(rt.capturedBody, []byte("prompt_cache_key")) {
		t.Errorf("passthrough must not inject prompt_cache_key:\n got=%s", rt.capturedBody)
	}
	if rt.capturedAuth != "Bearer at1" {
		t.Errorf("Authorization not injected, got %q", rt.capturedAuth)
	}
}

// TestRawChatUpstreamBusinessErrorPassthrough 透传 key 下上游 4xx/5xx 的响应体
// 同样原样透传（不读 body 分类、不重写成 OpenAI error 形态）。
func TestRawChatUpstreamBusinessErrorPassthrough(t *testing.T) {
	raw := `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`
	rt := &rawTransport{status: 400, header: http.Header{"Content-Type": []string{"application/json"}}, body: raw}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKeys: []httpauth.Key{ptKey()}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer pt-key-123")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != raw {
		t.Fatalf("upstream error body must be passthrough:\n got=%s\nwant=%s", got, raw)
	}
}

// TestRawChatTransportErrorCountsFailure 透传链路 transport 层失败走 502，且按
// 主链路口径记一次账号级连败失败（NoteFailures，model=""），好号快速换走。
func TestRawChatTransportErrorCountsFailure(t *testing.T) {
	rt := &rawTransport{err: errors.New("dial tcp: connection refused")}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKeys: []httpauth.Key{ptKey()}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer pt-key-123")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s (transport error must be 502)", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 1 {
		t.Errorf("ConsecutiveFails=%d want 1 (transport failure must feed account-scoped NoteFailures)", st.ConsecutiveFails)
	}
	if len(st.DegradedModels) != 0 {
		t.Errorf("DegradedModels=%+v want empty (transport failure must not feed model-scoped degrade)", st.DegradedModels)
	}
}

// TestRawChatStripsConnectionNominatedHopHeaders 透传链路下，上游 Connection 头
// 点名的额外逐跳头必须一并剔除（RFC 7230 §6.1），其余自定义头保留。
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
	h := NewHandler(Config{Pool: p, Upstream: up, APIKeys: []httpauth.Key{ptKey()}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer pt-key-123")
	h.ServeHTTP(rec, req)
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

// TestRawChatNonPassthroughKeyUsesMainPipeline 非透传 key 必须照常走主链路
// （不是透传）：出站 body 会被注入 prompt_cache_key。
func TestRawChatNonPassthroughKeyUsesMainPipeline(t *testing.T) {
	rt := &rawTransport{
		status: 200,
		header: http.Header{"Content-Type": []string{"application/json"}},
		body:   sseOK,
	}
	up := &upstream.Client{HTTP: &http.Client{Transport: rt}, ChatBaseCN: "https://fake.example"}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKeys: []httpauth.Key{
		{ID: "normal", Key: "normal-key-123", Enabled: true},
	}})

	in := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(in))
	req.Header.Set("Authorization", "Bearer normal-key-123")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 主链路会注入 prompt_cache_key；透传则不会。
	if !bytes.Contains(rt.capturedBody, []byte("prompt_cache_key")) {
		t.Errorf("non-passthrough key must go through main pipeline (expect prompt_cache_key injected)")
	}
}

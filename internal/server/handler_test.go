package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
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
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// failingResponseWriter 模拟客户端已断开：响应头可写，但任何正文写入都返回
// io.ErrClosedPipe（net/http 在连接关闭后写 socket 的典型错误）。
type failingResponseWriter struct{ header http.Header }

func newFailingResponseWriter() *failingResponseWriter {
	return &failingResponseWriter{header: make(http.Header)}
}

func (w *failingResponseWriter) Header() http.Header { return w.header }
func (w *failingResponseWriter) WriteHeader(int)     {}
func (w *failingResponseWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// TestChatLargeBodyNoGatewayLimit 请求体无网关侧上限（max_body_mb 已移除）：
// 数 MB 的合法 body 完整读入并照常打上游，网关不再 413（超限类问题交由上游
// 自然响应，对齐上游 e34cfa4 规约）。
func TestChatLargeBodyNoGatewayLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	pad := strings.Repeat("a", 4<<20) // 4MB 合法 JSON 字符串值
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatImageFollowUpKeepsDerivedSession 端到端钉住 11135 的多轮修复：
//   - 第 1 轮带图 -> 出站 X-Conversation-ID / prompt_cache_key 进入按账号隔离的图片会话；
//   - 第 2 轮同一 conversation_id 只发文字 -> 两轮必须沿用同一个派生会话；
//   - 换图 -> 派生会话必须变化。
//
// 断言的是实际写到上游 HTTP 请求上的值，而不是 session 包内部返回值。
func TestChatImageFollowUpKeepsDerivedSession(t *testing.T) {
	type seen struct {
		conv string
		key  string
	}
	var got []seen
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(body, &obj)
			key, _ := obj["prompt_cache_key"].(string)
			got = append(got, seen{conv: r.Header.Get("X-Conversation-ID"), key: key})
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	send := func(body string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}
	image := `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`
	send(`{"model":"glm-5.2","stream":true,"conversation_id":"conv-1","messages":[{"role":"user","content":[` + image + `]}]}`)
	send(`{"model":"glm-5.2","stream":true,"conversation_id":"conv-1","messages":[{"role":"user","content":"这次图里是什么？"}]}`)
	send(`{"model":"glm-5.2","stream":true,"conversation_id":"conv-1","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]}]}`)

	if len(got) != 3 {
		t.Fatalf("upstream calls=%d want 3", len(got))
	}
	if got[0].conv == "" || got[0].conv == "conv-1" || !strings.Contains(got[0].conv, ":img") {
		t.Fatalf("first image turn did not use derived session: %+v", got[0])
	}
	if got[1].conv != got[0].conv || got[1].key == "" || got[1].key != got[0].key {
		t.Fatalf("text follow-up must reuse image session: first=%+v follow-up=%+v", got[0], got[1])
	}
	if got[2].conv == got[0].conv {
		t.Fatalf("replacing image must switch session: first=%+v replaced=%+v", got[0], got[2])
	}
}

// TestChatIgnoresInboundConversationHeader X-Conversation-ID 的来源是请求体
// conversation_id / sessKey，不是入站同名头。若直接复制客户端头，调用方可以
// 随意覆盖图片隔离派生的会话，11135 换图隔离会被绕过；没有会话键时也不应
// 把客户端头当会话来源。
func TestChatIgnoresInboundConversationHeader(t *testing.T) {
	var gotConv string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotConv = r.Header.Get("X-Conversation-ID")
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	body := `{"model":"glm-5.2","stream":true,"conversation_id":"body-conv","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Conversation-ID", "header-must-not-win")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotConv != "body-conv" {
		t.Fatalf("outbound X-Conversation-ID=%q want body conversation_id", gotConv)
	}
}

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 请求级错误：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），**且不轮转**——
// 同一 body 换号必然同样失败。端到端断言只打一次上游、直接回 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatBadParams400CarriesUpstreamBody 11101 的 400 响应必须包含上游原始
// 11101 信息（含 requestId，客户端据此排查），且不再出现空洞的 no_healthy_account。
func TestChatBadParams400CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
}

// TestChatToolSequenceFailsFastWithoutPenalty 上游 400 + 11148（工具调用与结果
// 记录不匹配）→ 请求历史损坏：不罚账号、不轮转，直接 400 透传上游原文。
// 同一份坏历史换任何账号都必然同样 400，轮转只会把坏历史重放给健康号。
func TestChatToolSequenceFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry","requestId":"req-11148"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: corrupted tool history must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11148") || !strings.Contains(body, "tool calls and tool results do not match") {
		t.Errorf("400 message should carry upstream 11148 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
	// 账号完好：不罚号。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrToolSequence must not penalize account: %+v", st)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

// TestChatNonStreamEmptyChoicesNotFakeSuccess 上游 200 + 只有空 choices 控制帧：
// 必须返回 502 upstream_parse，不能合成 200 空内容，也不能把这一跳记成成功
// （否则失败号会被 NoteSuccess 洗白、粘性绑定钉在坏号上）。
func TestChatNonStreamEmptyChoicesNotFakeSuccess(t *testing.T) {
	const emptyChoices = "data: {\"id\":\"chatcmpl-empty\",\"object\":\"chat.completion.chunk\",\"choices\":[]}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, emptyChoices, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s want 502", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_parse") {
		t.Fatalf("body=%s want upstream_parse", rec.Body.String())
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.SuccessCount != 0 {
		t.Fatalf("empty choices must not be recorded as success: %+v", st.TokenUsage)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

// TestChatStreamRoleOnlyNotFakeSuccess 上游只发 role 首帧（没有正文/思考/tool_calls）
// 时不能按成功收尾：流式必须返回空流错误，且账号成功计数保持 0。
func TestChatStreamRoleOnlyNotFakeSuccess(t *testing.T) {
	const roleOnly = "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, roleOnly, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	st, _ := p.Status("u1")
	if st.SuccessCount != 0 {
		t.Fatalf("role-only stream must not be recorded as success: %+v", st)
	}
	// 流式已经提交 200 头；空流以 error 帧告知客户端，不能合成正常回答。
	if !strings.Contains(rec.Body.String(), "upstream_parse") && !strings.Contains(rec.Body.String(), "empty upstream stream") {
		t.Fatalf("role-only stream must surface empty-stream error: %s", rec.Body.String())
	}
}

// TestChatStreamEmptyRecordsUsageFailure 空流必须按失败计入 usage：复用 role-only
// 空回答形态，断言 usage 的请求数增加而成功数不增加。此前流式成功标志在空流分支
// 仍是 true，导致面板「成功请求」把上游缺陷算成健康。
func TestChatStreamEmptyRecordsUsageFailure(t *testing.T) {
	const roleOnly = "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, roleOnly, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	usageRec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: usageRec})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	totals := usageRec.Snapshot(0, nil).Totals
	if totals.Requests != 1 || totals.Errors != 1 {
		t.Fatalf("empty stream usage totals=%+v want requests=1 errors=1", totals)
	}
}

// TestChatStreamClientGoneUsageNotFailed 客户端主动断连（写侧失败）不罚账号：
// 这一跳可能已收到完整上游回答，usage 事件应是成功或至少不是账号失败（Errors=0），
// 避免「用户点停止」把健康号的成功率打下去。
func TestChatStreamClientGoneUsageNotFailed(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	usageRec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: usageRec})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	totals := usageRec.Snapshot(0, nil).Totals
	if totals.Requests != 1 || totals.Errors != 0 {
		t.Fatalf("client-gone usage totals=%+v want requests=1 errors=0", totals)
	}
}

// TestChatStreamClientWriteErrorDoesNotWashAccount 客户端写失败（连接断开）时，
// 上游可能只发了一半就停止；没有完整读完就不能证明账号成功。此前该分支会
// NoteSuccess + 清 11102 + 绑粘性，于是用户点停止即可把限流号洗成健康号。
func TestChatStreamClientWriteErrorDoesNotWashAccount(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	store := newBindStore()
	sess := session.New(session.Config{
		TTL: time.Minute,
		// 故意把过期绑定 old 报告为可用：Resolve 会先选中它，PickByUIDForModel(old)
		// 找不到账号后解绑并回落轮换到 u1。若写失败分支错误地 Bind，会把 conv-1
		// 重新绑到 u1；修复后应保持解绑（存储中无 conv-1）。
		Store:     store,
		Available: func() []string { return []string{"old", "u1"} },
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	usageRec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, Usage: usageRec})
	sess.Bind("conv-1", "old")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`))
	h.ServeHTTP(newFailingResponseWriter(), req)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.SuccessCount != 0 {
		t.Fatalf("client write error must not be recorded as success: %+v", st.TokenUsage)
	}
	if !st.LastSuccessTime.IsZero() {
		t.Fatalf("client write error must not set last_success: %v", st.LastSuccessTime)
	}
	if uid, bound := store.lastUID("conv-1"); bound {
		t.Fatalf("client write error must not bind sticky session, got uid=%s", uid)
	}
	totals := usageRec.Snapshot(0, nil).Totals
	if totals.Requests != 1 || totals.Errors != 0 {
		t.Fatalf("client-gone usage totals=%+v want requests=1 errors=0", totals)
	}
}

// TestChatStreamErrorFrameAfterContentUnbindsSticky 正文之后收到 6004 error 帧：
// 必须释放租约并解绑当前粘性号（账号对其他模型仍可用），同时只锁该模型、
// 不记成功。否则下一轮会话仍会被钉在这个限流号上。
func TestChatStreamErrorFrameAfterContentUnbindsSticky(t *testing.T) {
	const halfErr = "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"code\":6004,\"message\":\"model rate limit\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, halfErr, true })
	store := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     store,
		Available: func() []string { return []string{"u1"} },
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	sess.Bind("conv-1", "u1")

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if uid, bound := store.lastUID("conv-1"); bound {
		t.Fatalf("error frame after content must unbind sticky session, got uid=%s", uid)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.SuccessCount != 0 {
		t.Fatalf("error frame must not count as success: success_count=%d", st.SuccessCount)
	}
	if st.Cooling {
		t.Fatalf("6004 must only lock the model, not the whole account: %+v", st)
	}
	found := false
	for _, m := range st.RateLimitedModels {
		if m.Model == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("glm-5.2 should remain model-rate-limited: %+v", st.RateLimitedModels)
	}
}

// TestChatStreamHalfErrorFrameRecordsUsageFailure 正文之后才出现 error 帧：
// StreamHint 返回 nil（字节已透传），但 usage 必须把这次尝试记成失败；否则
// 「正文+错误收尾」的坏流会在面板上伪装成成功。
func TestChatStreamHalfErrorFrameRecordsUsageFailure(t *testing.T) {
	const halfErr = "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"code\":6004,\"message\":\"model rate limit\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, halfErr, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	usageRec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: usageRec})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	totals := usageRec.Snapshot(0, nil).Totals
	if totals.Requests != 1 || totals.Errors != 1 {
		t.Fatalf("half error stream usage totals=%+v want requests=1 errors=1", totals)
	}
}

func TestChatRecordsCreditForStreamAndSync(t *testing.T) {
	const sseCredit = "data: {\"id\":\"chatcmpl-credit\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseCredit, true
			})
			rec := usage.New("")
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				Usage:    rec,
			})
			body := `{"model":"glm-5.2","messages":[]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[]}`
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
			}
			s := rec.Snapshot(24, nil)
			if s.Totals.Credits != 1.25 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 10 || s.Totals.CreditsPer1MTokens != 125000 {
				t.Fatalf("usage totals = %+v, want credit=1.25 tokens=10 ratio=125000", s.Totals)
			}
			if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "u1" ||
				len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" {
				t.Fatalf("credit dimensions = %+v / %+v", s.CreditByAccount, s.CreditByModel)
			}
		})
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 0 {
		t.Errorf("bad token usage=%+v", st.TokenUsage)
	}
	good, _ := p.Status("good")
	if good.TokenUsage.RequestCount != 1 || good.TokenUsage.TotalTokens != 2 {
		t.Errorf("good token usage=%+v", good.TokenUsage)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	// 吸收上游语义：带解析重置时间的 body 对齐墙钟；无时间文案走有界退避，且
	// **冷却中的重复触发不堆加**（旧「每次都翻倍」正是全池被推到 2h 封顶的元凶）。
	// 跨冷却期的堆加语义由 pool 包 CooldownSoftRate 测试覆盖；此处锚定 handler 侧
	// 的调用形态：连续 3 次软限流错误 → 首次 600s，后续不延长。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("call %d: 冷却中兜底探测不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		if st.CoolRemaining < 600-3 || st.CoolRemaining > 600 {
			t.Errorf("call %d: cool_remaining_sec=%d want ~600（不堆加）", i, st.CoolRemaining)
		}
	}
}

func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	// 404 分流：偶发上游 404 的冷却基数固定 60s（notFoundCooldown），不取 soft_rate
	// 的 600s 基数，也不受其配置值影响。Cooldown 重构后 404 是固定时长冷却
	//（不堆加、不喂熔断），成功后自然到期恢复。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i, st)
		}
		if st.SoftStreak != 0 {
			t.Errorf("call %d: 404 固定冷却不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m），且重复触发不延长。
		if st.CoolRemaining < notFoundSec-3 || st.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i, st.CoolRemaining, notFoundSec, notFoundSec)
		}
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：PickByUIDForModel 将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4 (next-day 04:00)", st.Until.Hour())
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 2000)
	p.SetCredits("good", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" || st.Until.Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", st.Reason, st.Until)
	}
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChatRateLimitRotatesPastMaxRotate 账号很多时，一次请求不该在默认 3 次换号后
// 把 429 交回客户端。前 4 个号都限流，第 5 个号成功，客户端只看到 200。
func TestChatRateLimitRotatesPastMaxRotate(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		calls[authz]++
		mu.Unlock()
		if authz == "Bearer at-good" {
			return 200, sseOK, true
		}
		return 429, `{"code":1,"msg":"rate limit"}`, false
	})
	auths := []*auth.Auth{
		{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
		{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
		{UID: "d", AccessToken: "at-d", ExpiresAt: 9999999999},
		{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	}
	p := testPoolWith(auths...)
	// 积分从高到低，确定性随机源每次选剩余里积分最高的，顺序 a→b→c→d→good。
	for i, a := range auths {
		p.SetCredits(a.UID, int64(5000-i*100), 0)
	}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls["Bearer at-good"] != 1 {
		t.Fatalf("good account was not reached: calls=%v", calls)
	}
	for _, a := range []string{"at-a", "at-b", "at-c", "at-d"} {
		if calls["Bearer "+a] != 1 {
			t.Errorf("limited account %s calls=%d want 1", a, calls["Bearer "+a])
		}
	}
}

// TestChatStreamRateLimitBeforeContentRotates 流式 200 + 首帧即限流：客户端还没看到
// 正文，应换号继续，而不是把 error 帧写进已经开始的 SSE。
func TestChatStreamRateLimitBeforeContentRotates(t *testing.T) {
	const rateFrame = "data: {\"error\":{\"code\":6004,\"message\":\"模型限流，将在 2099-01-01 00:00:00 重置\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 200, rateFrame, true
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "6004") || strings.Contains(rec.Body.String(), "限流") {
		t.Fatalf("rate-limit frame leaked to client: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("success content missing: %s", rec.Body.String())
	}
	st, _ := p.Status("bad")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("bad should carry a model rate-limit row: %+v", st.RateLimitedModels)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级独立冷却：账号级不 cooling，台账单行 until ≈ reset。
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row glm-5.3: %+v", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "hy3-x")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6021QueueFullDoesNotFreezeAccount 429 + code 6021 只锁触发模型。
// 同模型换到其他号；该号改用别的模型仍然可选，账号级冷却不置位。
func TestChat6021QueueFullDoesNotFreezeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":6021,"msg":"queue.full.title","requestId":"600815c599a88d976ad23d47a953b22c"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6021 must not set account-level cooling: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Fatalf("6021 must not advance soft_streak: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "deepseek-v4.1-flash" {
		t.Fatalf("want model row deepseek-v4.1-flash: %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Until.IsZero() {
		t.Fatal("model cooldown until is zero")
	}
	if rem := time.Until(st.RateLimitedModels[0].Until); rem <= 0 || rem > time.Minute+time.Second {
		t.Errorf("model cooldown remaining=%v want ~1m", rem)
	}
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "deepseek-v4.1-flash")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad, got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "glm-5.3")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should still select bad, got %+v", diff)
	}
}

// TestChat6020QueueWaitingDoesNotFreezeAccount 429 + code 6020 只锁触发模型。
// 冷却时长取 body 的 retry_after（40s），不用 estimated_wait，也不写账号级 until。
func TestChat6020QueueWaitingDoesNotFreezeAccount(t *testing.T) {
	const body6020 = `{"code":6020,"msg":"queue.waiting.title","requestId":"e90f5c990715c3811c037a1a5cd42fb2","data":{"queue_position":4714,"queue_size":"999+","retry_after":40,"estimated_wait":23570,"client_user_type":"free"}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, body6020, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 10 * time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if st.Cooling || st.SoftStreak != 0 {
		t.Fatalf("6020 must not freeze the account: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want model row glm-5.3: %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Reason != "6020 queue waiting" {
		t.Fatalf("reason=%q", st.RateLimitedModels[0].Reason)
	}
	rem := time.Until(st.RateLimitedModels[0].Until)
	if rem < 30*time.Second || rem > 40*time.Second+time.Second {
		t.Errorf("model cooldown remaining=%v want ~40s (retry_after, not estimated_wait or soft_rate)", rem)
	}
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad, got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "deepseek-v4.1-flash")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should still select bad, got %+v", diff)
	}
}

// TestChat6021OnlyAccountReturnsModelUnavailable 池里只剩这个号时，6021 回模型不可用，
// 而不是把号冻进账号级软冷却。
func TestChat6021OnlyAccountReturnsModelUnavailable(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6021,"msg":"queue.full.title"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at-only", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != "model_unavailable" {
		t.Fatalf("error.code=%q want model_unavailable", env.Error.Code)
	}
	st, _ := p.Status("only")
	if st.Cooling {
		t.Fatalf("single-account 6021 froze the account: %+v", st)
	}
	other := p.PickExcludingForModel(nil, "glm-5.3")
	if other == nil || other.UID != "only" {
		t.Fatalf("other model should still pick the account, got %+v", other)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → **模型级**有界退避：
// 只锁该模型（有界），账号级 until 不冻结、其他模型仍可选——单模型限流绝不冻整号。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	// 关键：6004 无重置时间也必须只锁模型，不得冻结账号级 until（否则其他模型也一起不可选）。
	if st.Cooling {
		t.Fatalf("6004 (no reset) must NOT freeze the whole account: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate-limited models=%+v, want one model-level row for glm-5.3", st.RateLimitedModels)
	}
	// 模型级退避时长 = 注入 soft 基数(60s)（非解析时间），落在模型条目的 until 上。
	if rem := time.Until(st.RateLimitedModels[0].Until); rem <= 0 || rem > time.Minute+time.Second {
		t.Errorf("model until remaining=%v want ~60s (soft base, not parsed)", rem)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("cn:glm-5.2 missing")
	}
}

// /v1/models 对受限 key 只返回其 realm/models 允许的条目；上游目录本身不变。
func TestModelsFilteredByAPIKeyRestrictions(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.3"}, {ID: "kimi-k3"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.Unlock()
	}()

	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, `{"code":0,"data":{"models":[]}}`, false })
	live := livecfg.New(livecfg.Snapshot{Keys: []httpauth.Key{{
		ID: "restricted", Key: "key-restricted", Enabled: true, Realm: "cn", Models: []string{"glm-*"},
	}}})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, Live: live,
	})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer key-restricted")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0]["id"] != "cn:glm-5.3" {
		t.Fatalf("filtered models=%+v", resp.Data)
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens + reasoning 档位）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
			// reasoning 档位透出：supported_efforts + default_effort
			efforts, _ := mm["supported_efforts"].([]any)
			if len(efforts) != 3 || efforts[0] != "low" {
				t.Errorf("dyn-model-a supported_efforts=%v", mm["supported_efforts"])
			}
			if mm["default_effort"] != "medium" {
				t.Errorf("dyn-model-a default_effort=%v want medium", mm["default_effort"])
			}
		case "dyn-model-b":
			// 上游未返回 reasoning → 两个档位字段都省略（客户端按自身默认）
			if _, has := mm["supported_efforts"]; has {
				t.Errorf("dyn-model-b supported_efforts should be omitted, got %v", mm["supported_efforts"])
			}
			if _, has := mm["default_effort"]; has {
				t.Errorf("dyn-model-b default_effort should be omitted")
			}
		case "glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	// 纯动态化（产品决策）：上游失败 → 空列表（200），不再回落静态表——
	// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("pure-dynamic failure should yield empty list, got %d", len(data))
	}
}

func TestModelsFetchFailurePenalizesAccount(t *testing.T) {
	// 吸收上游 9832283：models 拉取失败只进负缓存，不 NoteError——
	// NoteError 喂的是 chat 熔断器，models 端点 5xx 跨界惩罚 chat 通道健康号。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若仍罚号，一次 fetch 失败即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("models fetch failure must not trip chat breaker: %+v", st)
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls atomic.Int32 // FetchModels 企业/v3 两路并发探测回调，计数须原子
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 纯动态下空列表仍返回 200。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	// 一轮探测 = 2 次上游调用（企业端点 + /v3/config 并发，两路全失败才进负缓存）。
	if got := calls.Load(); got != 2 {
		t.Errorf("want 2 probes (console + v3), got %d", got)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("want 4 probes after cooldown (2 rounds x 2), got %d", got)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 0)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// 多 key：命中条目、禁用条目与 realm/models 限制必须在请求前生效；跨域明确 400，
// 不静默改走另一个域。
func TestMultiAPIKeyIdentityRealmAndModels(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "cn1", AccessToken: "at-cn", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{Keys: []httpauth.Key{
		{ID: "cn", Key: "key-cn", Enabled: true, Realm: "cn"},
		{ID: "global", Key: "key-global", Enabled: true, Realm: "global"},
		{ID: "models", Key: "key-models", Enabled: true, Models: []string{"glm-*"}},
		{ID: "off", Key: "key-off", Enabled: false},
	}})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})
	cases := []struct {
		name, token, body string
		want              int
		errCode           string
	}{
		{"realm match", "key-cn", `{"model":"cn:glm-5.3","messages":[]}`, 200, ""},
		{"realm mismatch", "key-cn", `{"model":"global:kimi-k3","messages":[]}`, 400, "realm_not_allowed"},
		{"global key cannot cn", "key-global", `{"model":"glm-5.3","messages":[]}`, 400, "realm_not_allowed"},
		{"model allowed", "key-models", `{"model":"glm-5.3","messages":[]}`, 200, ""},
		{"model denied", "key-models", `{"model":"kimi-k3","messages":[]}`, 400, "model_not_allowed"},
		{"disabled key", "key-off", `{"model":"glm-5.3","messages":[]}`, 401, "invalid_api_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+c.token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("code=%d body=%s want %d", rec.Code, rec.Body, c.want)
			}
			if c.errCode != "" && !strings.Contains(rec.Body.String(), c.errCode) {
				t.Fatalf("body=%s want %s", rec.Body, c.errCode)
			}
		})
	}
}

// 命中 key 的身份必须进入请求归档（key_id），且归档/响应绝不能出现密钥原文。
func TestRequestLogRecordsKeyIDWithoutSecret(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	rec := reqlog.New(reqlog.Config{})
	live := livecfg.New(livecfg.Snapshot{Keys: []httpauth.Key{{ID: "team-a", Key: "super-secret-token", Enabled: true}}})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, RequestLog: rec, Live: live,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer super-secret-token")
	h.ServeHTTP(httptest.NewRecorder(), req)
	snap := rec.Snapshot()
	if len(snap.Recent) != 1 || snap.Recent[0].KeyID != "team-a" {
		t.Fatalf("recent=%+v", snap.Recent)
	}
	raw, _ := json.Marshal(snap.Recent[0])
	if strings.Contains(string(raw), "super-secret-token") {
		t.Fatalf("secret leaked into archive event: %s", raw)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// 降级粘性：新请求 Active()=true，body 已被 Rewrite(Degraded)，上游首字节即 200。
	// 但 fake 上游只对 firstCall 返回 400，之后都 200，无法区分"直达"与"重试"。
	// 用 degrade.Active() 直接断言粘性生效。
	if !h.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报）；仍拦则直接回
// 400 content_blocked 防火墙文案（不轮转、不暴露账号/上游错误码）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下仍拦 → 400 content_blocked（内容终态，换号无意义），不降级重试。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked (custom does not degrade)", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", env.Error.Code)
	}
	// 错误透传（error-passthrough，吸收上游 5755fe3）：message 装上游 body 原文
	// （code/msg 原样），客户端必须看到真实错误才能排查；gateway_hint 并列补充。
	if !strings.Contains(rec.Body.String(), "11128") {
		t.Errorf("message should carry upstream original body (passthrough): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}
	if h.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.SanitizeFingerprints.Store(true) // 开启清洗层（与生产一致）
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留。
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// 旧 system 原文（含句点）不应以 system 角色出现；但 sanitize 把它改写为
		// "...official CLI tool for Claude."，所以原文 fingerprint 串应消失。
	}
	// 原始指纹串（逐字精确匹配）在出站 body 中应被改写：
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// 改写后的痕迹应在（证明 sanitize 层生效，不是"全删了"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// --- 模型原生工具调用标记修复（端到端，见 upstream/dsml.go）-----------------

// markupFixture 拼一段「正文 + 原生工具调用标记」，模拟上游在没有 tools 的请求里
// 把工具调用吐成文本的形态。分隔符按码点构造，避免测试源码里出现裸全角字面量。
func markupFixture(prose, cmd string) string {
	return markupFixtureNamed(prose, "exec_command", cmd)
}

// markupFixtureNamed 同 markupFixture，但可指定工具名（用于弱判定的形态把关测试）。
func markupFixtureNamed(prose, name, cmd string) string {
	const pipe = "\uFF5C\uFF5C"
	tag := func(rest string) string { return "<" + pipe + "DSML" + pipe + " " + rest + ">" }
	ctag := func(rest string) string { return "</" + pipe + "DSML" + pipe + " " + rest + ">" }
	return prose + tag("calls") + "\n" +
		tag(`invoke name="`+name+`"`) + "\n" +
		tag(`parameter name="cmd" string="true"`) + cmd + ctag("parameter") + "\n" +
		ctag("invoke") + "\n" + ctag("calls")
}

// markupUpstreamSSE 用一段正文拼上游 SSE（role 首帧 + 正文帧 + stop 收尾帧 + [DONE]）。
func markupUpstreamSSE(content string) string {
	chunk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{
			"id": "resp-1", "object": "chat.completion.chunk", "created": 1, "model": "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return "data: " + string(b) + "\n\n"
	}
	return chunk(map[string]any{"role": "assistant"}, nil) +
		chunk(map[string]any{"content": content}, nil) +
		chunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
}

// declaredToolsBody 是带 tools 声明的请求体（标记修复的启用前提）。
const declaredToolsBody = `"tools":[{"type":"function","function":{"name":"exec_command",` +
	`"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`

// TestChatNonStreamRepairsMarkupToolCalls 非流式：上游把工具调用吐成正文标记时，
// 网关还原成 message.tool_calls，正文不再残留标记，finish_reason 收敛为 tool_calls。
func TestChatNonStreamRepairsMarkupToolCalls(t *testing.T) {
	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "先读一遍控制器。\n\n" {
		t.Errorf("content=%q", msg["content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v", fn["arguments"])
	}
	if args["cmd"] != "ls -la" {
		t.Errorf("cmd=%v", args["cmd"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatStreamRepairsMarkupToolCalls 流式：同样还原成 delta.tool_calls，且收尾帧
// finish_reason 为 tool_calls、排在调用帧之后。
func TestChatStreamRepairsMarkupToolCalls(t *testing.T) {
	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "\uFF5C") {
		t.Fatalf("流式响应里仍残留标记: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺 [DONE] 收尾: %s", body)
	}
	var text strings.Builder
	var calls int
	frameNo, lastCallFrame, finishFrame := 0, -1, -1
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		frameNo++
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("帧不是合法 JSON: %q (%v)", payload, err)
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		ch, _ := chs[0].(map[string]any)
		d, _ := ch["delta"].(map[string]any)
		if v, ok := d["content"].(string); ok {
			text.WriteString(v)
		}
		if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
			calls += len(tcs)
			lastCallFrame = frameNo
			fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
			if fn == nil || fn["name"] != "exec_command" {
				t.Fatalf("调用帧 function=%#v", tcs[0])
			}
		}
		if v, ok := ch["finish_reason"].(string); ok && v != "" {
			finishFrame = frameNo
			if v != "tool_calls" {
				t.Errorf("finish_reason=%q want tool_calls", v)
			}
		}
	}
	if text.String() != "先读一遍控制器。\n\n" {
		t.Errorf("正文错误: %q", text.String())
	}
	if calls != 1 {
		t.Errorf("tool_call 帧数=%d want 1", calls)
	}
	if lastCallFrame < 0 || finishFrame < 0 {
		t.Fatalf("缺调用帧或收尾帧: lastCall=%d finish=%d", lastCallFrame, finishFrame)
	}
	if finishFrame < lastCallFrame {
		t.Errorf("收尾帧（第 %d 帧）必须排在调用帧（第 %d 帧）之后", finishFrame, lastCallFrame)
	}
}

// --- 弱判定（名单为空）端到端：这是实测最常见的泄漏形态 ---------------------

// TestChatRepairsMarkupWithoutDeclaredTools 请求体里**没有** tools 声明时（tools 在
// 中转环节丢失是实测常态），弱判定必须照样把正文标记还原成 tool_calls——否则修复层
// 恰好对主场景不设防。夹具用生产环境真实泄漏的正文。
func TestChatRepairsMarkupWithoutDeclaredTools(t *testing.T) {
	content := markupFixture("", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatMarkupRepairRejectsNonIdentifierToolName 弱判定的把关：名字不像工具名
// （模型在正文里「讨论」这段语法时的占位符）→ 原文透传，绝不改写成工具调用。
func TestChatMarkupRepairRejectsNonIdentifierToolName(t *testing.T) {
	content := markupFixtureNamed("", "<tool_name>", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("占位符工具名不得转换: %#v", msg["tool_calls"])
	}
	if got, _ := msg["content"].(string); !strings.Contains(got, "\uFF5C") {
		t.Fatalf("应原文透传，实际 content=%q", got)
	}
}

// TestChatMarkupRepairStrictModeRejectsUndeclaredTool 名单非空时走严格判定：
// 块内工具名不在名单里 → 不转换（即使名字形态合法）。
func TestChatMarkupRepairStrictModeRejectsUndeclaredTool(t *testing.T) {
	content := markupFixtureNamed("", "rm_rf_everything", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("未声明的工具名不得转换: %#v", msg["tool_calls"])
	}
}

// TestChatNonStreamModelRateLimitLocksModelOnly 非流式路径撞上游 6004 模型级限流帧：
// 必须返回 429/503 类错误（绝不能是 200 空内容假成功），且账号只被**模型级**冷却
// （modelCooldowns 命中该模型），账号级 until 不得被冻结——其他模型仍可选。
func TestChatNonStreamModelRateLimitLocksModelOnly(t *testing.T) {
	const errFrame = "data: {\"error\":{\"code\":6004,\"message\":\"model usage limit exceeded, will reset at 2026-09-11 18:33:27 UTC+8\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, errFrame, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("model rate-limit frame must not be a 200 fake success: body=%s", rec.Body)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.Cooling {
		t.Errorf("model-level 6004 must not freeze the whole account (until=%v)", st.Until)
	}
	// 该模型在账号 UI 上应可见为限流模型（模型级冷却已记入）。
	found := false
	for _, m := range st.RateLimitedModels {
		if m.Model == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Errorf("glm-5.2 should be recorded as model-rate-limited: %+v", st.RateLimitedModels)
	}
	// 终态必须明确是 429（可重试），不能落回 503/200：唯一账号撞 6004 时
	// 没有可轮转对象，lastErr 在末端统一映射；同时不得增加成功计数。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("single-account model limit code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	if st.SuccessCount != 0 {
		t.Fatalf("model limit must not count as success: success_count=%d", st.SuccessCount)
	}
	if a := p.PickExcludingForRealm(nil, "other-model", ""); a == nil || a.UID != "u1" {
		t.Fatalf("other model must remain routable after model-level limit: %+v", a)
	}
}

// TestChatNonStreamModelRateLimitRotatesToHealthyAccount 钉死红线：单模型限流
// 只能淘汰当前账号的该模型，不能中断整个账单/账号池调度。首个账号返回 6004，
// 第二个账号必须被继续选中并完成请求；限流号的其他模型仍可路由。
func TestChatNonStreamModelRateLimitRotatesToHealthyAccount(t *testing.T) {
	reset := time.Now().In(upstream.SoftRateResetLoc()).Add(10 * time.Minute).Format("2006-01-02 15:04:05")
	errBody := "data: {\"code\":6004,\"msg\":\"model usage limit exceeded, will reset at " + reset + " UTC+8\"}\n\ndata: [DONE]\n\n"
	const okBody = "data: {\"id\":\"ok\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	var calls atomic.Int32
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		if authz == "Bearer at1" {
			return 200, errBody, true
		}
		return 200, okBody, true
	})
	// 固定随机源优先选积分最高号；u1 先被选，命中 6004 后只能轮到 u2。
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("model limit must rotate to healthy account, code=%d body=%s", rec.Code, rec.Body)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected rotate to second account, calls=%d", calls.Load())
	}
	st1, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 status missing")
	}
	if st1.Cooling {
		t.Fatalf("u1 must not be account-level cooled: until=%v", st1.Until)
	}
	found := false
	for _, m := range st1.RateLimitedModels {
		if m.Model == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("u1 glm-5.2 should be model-rate-limited: %+v", st1.RateLimitedModels)
	}
	if st2, ok := p.Status("u2"); !ok || st2.Cooling || st2.SuccessCount == 0 {
		t.Fatalf("u2 should serve the rotated request: %+v ok=%v", st2, ok)
	}
	if a := p.PickExcludingForRealm(nil, "other-model", ""); a == nil || a.UID != "u1" {
		t.Fatalf("u1 must remain routable for other models: %+v", a)
	}
}

// TestChat6004WithoutModelNameNoBogusAudit 无模型名的 6004（model==""）：
// 没有模型名无法做模型级路由，但 6004 语义仍是模型级限流，绝不能退回账号级
// 冷却把整号冻住（会违反「单模型限流不得停整个账号调度」）。此时既不能记
// 模型台账（无模型名），也不能写账号级 until。
func TestChat6004WithoutModelNameNoBogusAudit(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	// 请求不带 model 字段 → bareModel=="" → applyErrorPolicy 的 model 参数为空串。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.Cooling || !st.Until.IsZero() {
		t.Fatalf("无模型名的 6004 不得冻结整号: %+v", st)
	}
	if len(st.RateLimitedModels) != 0 {
		t.Fatalf("无模型名不得产生模型限额台账行: %+v", st.RateLimitedModels)
	}
}

// TestChatClientGoneDoesNotPenalize 客户端断连（ctx 取消）落在传输层抖动分支时，
// 不得喂连败计数——否则健康号 5 次后被临时踢出选号池。
func TestChatClientGoneDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("transport: client closed")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端已断连
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("client-gone must not accumulate failures: consecutive_fails=%d", st.ConsecutiveFails)
	}
}

// TestChatStreamUpstreamReadErrorNotSuccess 流式读取上游失败（连接中断）时，
// 账号不得被记成功（不清 11102、不绑粘性），避免坏号长期驻留被粘性钉死。
func TestChatStreamUpstreamReadErrorNotSuccess(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// 首帧之后连接中断（body 读时返回错误）。
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(&errReader{
					data: []byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"),
				}),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)
	st, _ := p.Status("u1")
	if st.SuccessCount != 0 {
		t.Fatalf("upstream read error must not be recorded as success: success_count=%d", st.SuccessCount)
	}
}

// TestChatStreamUpstreamReadErrorAfterClientGoneUsageNotFailed 上游读失败与客户端
// 断连同时发生时，账号不罚、usage 也不记错误：人已走，这次截断不能算上游协议失败。
// 缺少这条时「用户点停止」会以 StreamReadError 收场并把 Errors 抬高。
func TestChatStreamUpstreamReadErrorAfterClientGoneUsageNotFailed(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(&errReader{
					data: []byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"),
				}),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	usageRec := usage.New("")
	h := NewHandler(Config{Pool: p, Upstream: up, Usage: usageRec})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	totals := usageRec.Snapshot(0, nil).Totals
	if totals.Requests != 1 || totals.Errors != 0 {
		t.Fatalf("client-gone read error usage totals=%+v want requests=1 errors=0", totals)
	}
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("client-gone read error must not accumulate failures: consecutive_fails=%d", st.ConsecutiveFails)
	}
}

// errReader 先返回一段数据，随后返回读错误（模拟上游半途断流）。
type errReader struct {
	data []byte
	done bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if !e.done {
		e.done = true
		n := copy(p, e.data)
		return n, nil
	}
	return 0, errors.New("unexpected EOF")
}

// TestChatStreamContentBlockedFrameDegrades 流式首帧为内容拦截 error 帧时，
// 与非流式一致地触发降级重试（passthrough 模式）。
func TestChatStreamContentBlockedFrameDegrades(t *testing.T) {
	var bodies [][]byte
	const blocked = "data: {\"error\":{\"message\":\"blocked by security policy\"}}\n\n"
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			body := blocked
			if len(bodies) > 1 {
				body = sseOK
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (blocked frame + degraded retry), got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("client should receive retried content: %s", rec.Body)
	}
}

// TestChatNonStreamContentBlockedDegradedRetry 非流式 passthrough 首遇内容拦截 error 帧：
// 与流式 / status>=400 路径同一套降级重试（换 Degraded 提示词同请求内重试），而非直接
// 把 error 帧当失败返回。此前非流式 error 帧分支只特判 ErrSoftRate，内容拦截会落到
// no_healthy_account(503) 并透传原始错误帧。
func TestChatNonStreamContentBlockedDegradedRetry(t *testing.T) {
	const blockedFrame = "data: {\"error\":{\"message\":\"blocked by security policy\",\"type\":\"content_blocked\"}}\n\ndata: [DONE]\n\n"
	var bodies [][]byte
	up := &upstream.Client{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, raw)
		body := blockedFrame
		if len(bodies) > 1 {
			body = sseOK
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (blocked frame + degraded retry), got %d", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("degraded retry should succeed, code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestUsageCreditTotalKeepsCreditWithoutTotal 上游只回 credit、不回 total_tokens
// 时，积分观测必须保留：旧实现把「有 credit 但 total=0」整条丢弃，导致用量页
// 少记一次扣减、积分/1M 比例分母缺样本。
func TestUsageCreditTotalKeepsCreditWithoutTotal(t *testing.T) {
	cases := []struct {
		name   string
		resp   map[string]any
		credit float64
		total  int
		ok     bool
	}{
		{
			name:   "credit only",
			resp:   map[string]any{"usage": map[string]any{"credit": 2.5}},
			credit: 2.5, total: 0, ok: true,
		},
		{
			name:   "credit with total",
			resp:   map[string]any{"usage": map[string]any{"credit": 2.5, "total_tokens": 1000.0}},
			credit: 2.5, total: 1000, ok: true,
		},
		{
			name:   "no credit key",
			resp:   map[string]any{"usage": map[string]any{"total_tokens": 1000.0}},
			credit: 0, total: 0, ok: false,
		},
		{
			name:   "credit null",
			resp:   map[string]any{"usage": map[string]any{"credit": nil, "total_tokens": 10.0}},
			credit: 0, total: 0, ok: false,
		},
		{
			name:   "credit as string",
			resp:   map[string]any{"usage": map[string]any{"credit": "2.5"}},
			credit: 0, total: 0, ok: false,
		},
		{
			name:   "no usage",
			resp:   map[string]any{},
			credit: 0, total: 0, ok: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			credit, total, ok := usageCreditTotal(c.resp)
			if ok != c.ok || credit != c.credit || total != c.total {
				t.Fatalf("got (%v,%d,%v) want (%v,%d,%v)", credit, total, ok, c.credit, c.total, c.ok)
			}
		})
	}
}

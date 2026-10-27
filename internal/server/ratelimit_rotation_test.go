package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestRateLimitFamilyRotatesPastMaxRotate 是 429 限流族「持续换号」的覆盖：
// 前 4 个号分别返回不同的 429 形态（普通限流 / 14018 积分耗尽 / 14017 trial
// 未激活 / 12153 会话失效），第 5 个号健康。此前只有 ErrSoftRate 会免扣
// MaxRotate，其余三类会在 3 次换号后中断并把 429/503 交回客户端。
func TestRateLimitFamilyRotatesPastMaxRotate(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"429 rate limit", `{"code":1,"msg":"rate limit"}`},
		{"429+14018 credit exhausted", `{"code":14018,"msg":"Credits exhausted"}`},
		{"429+14017 trial not activated", `{"code":14017,"msg":"trial not activated"}`},
		{"429+12153 session dead", `{"code":12153,"msg":"Offline user session not found"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				if authz == "Bearer at-good" {
					return 200, sseOK, true
				}
				return 429, c.body, false
			})
			auths := []*auth.Auth{
				{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
				{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
				{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
				{UID: "d", AccessToken: "at-d", ExpiresAt: 9999999999},
				{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
			}
			p := testPoolWith(auths...)
			// 积分从高到低：确定性随机源让选中顺序固定为 a→b→c→d→good。
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
			if !strings.Contains(rec.Body.String(), "你好") {
				t.Fatalf("success content missing: %s", rec.Body.String())
			}
		})
	}
}

// TestRateLimitFamilyExhaustedReturns429 全池都是 429（混合形态）时，末端必须回
// 429 而不是默认 503 no_healthy_account：这是「可重试」与「服务端过载」的语义分界。
func TestRateLimitFamilyExhaustedReturns429(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"429+14018 credit exhausted", `{"code":14018,"msg":"Credits exhausted"}`},
		{"429+14017 trial not activated", `{"code":14017,"msg":"trial not activated"}`},
		{"429+12153 session dead", `{"code":12153,"msg":"Offline user session not found"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 429, c.body, false
			})
			p := testPoolWith(
				&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
				&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
			)
			p.SetCredits("a", 2000, 0)
			p.SetCredits("b", 1000, 0)
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
				t.Fatalf("missing rate_limit_exceeded code: %s", rec.Body.String())
			}
		})
	}
}

// TestStreamRateLimitFamilyRotatesBeforeContent 流式 200 + 首帧即限流族 error：
// 正文未写出前应换号继续；换不动时回 429 JSON，而不是 502。
func TestStreamRateLimitFamilyRotatesBeforeContent(t *testing.T) {
	frames := map[string]string{
		"6004 model rate limit":     "{\"error\":{\"code\":6004,\"message\":\"模型限流，将在 2099-01-01 00:00:00 重置\"}}",
		"14018 credit exhausted":    "{\"code\":14018,\"msg\":\"Credits exhausted\"}",
		"14017 trial not activated": "{\"code\":14017,\"msg\":\"trial not activated\"}",
		"12153 session dead":        "{\"code\":12153,\"msg\":\"Offline user session not found\"}",
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			rateFrame := "data: " + frame + "\n\ndata: [DONE]\n\n"
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
			if strings.Contains(rec.Body.String(), "限流") || strings.Contains(rec.Body.String(), "Credits exhausted") ||
				strings.Contains(rec.Body.String(), "trial not activated") || strings.Contains(rec.Body.String(), "Offline user session") {
				t.Fatalf("rate-limit frame leaked to client: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "你好") {
				t.Fatalf("success content missing: %s", rec.Body.String())
			}
		})
	}
}

// TestStreamRateLimitFamilyExhaustedReturns429 流式全池限流族：正文前扣住的帧应
// 转成 429 JSON 而不是 502 upstream_error。
func TestStreamRateLimitFamilyExhaustedReturns429(t *testing.T) {
	frames := map[string]string{
		"14018 credit exhausted":    "{\"code\":14018,\"msg\":\"Credits exhausted\"}",
		"14017 trial not activated": "{\"code\":14017,\"msg\":\"trial not activated\"}",
	}
	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			rateFrame := "data: " + frame + "\n\ndata: [DONE]\n\n"
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, rateFrame, true
			})
			p := testPoolWith(
				&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
				&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
			)
			p.SetCredits("a", 2000, 0)
			p.SetCredits("b", 1000, 0)
			h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
				t.Fatalf("missing rate_limit_exceeded code: %s", rec.Body.String())
			}
		})
	}
}

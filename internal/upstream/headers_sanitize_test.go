package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersStripForeignHeaders 出站 chat 请求不得把入站其他平台的头
// （OpenAI/Anthropic/浏览器指纹）透传到上游：ChatHeaders 必须先从空头开始，
// 只写入 WorkBuddy 标准头族。
func TestChatHeadersStripForeignHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://upstream/v2/chat/completions", nil)
	for k, v := range map[string]string{
		"Authorization":     "Bearer sk-client-key",
		"x-api-key":         "sk-client-key",
		"anthropic-version": "2023-06-01",
		"x-stainless-os":    "macOS",
		"Origin":            "https://evil.example",
		"Referer":           "https://evil.example/chat",
		"User-Agent":        "OpenAI/Python 1.0",
		"Content-Type":      "text/plain",
	} {
		req.Header.Set(k, v)
	}
	c := &Client{}
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{})

	for _, k := range []string{"x-api-key", "anthropic-version", "x-stainless-os"} {
		if got := req.Header.Get(k); got != "" {
			t.Fatalf("%s leaked to upstream: %q", k, got)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at" {
		t.Fatalf("Authorization = %q, want account token", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Header.Get("User-Agent"); got != defaultUAString {
		t.Fatalf("User-Agent = %q, want WorkBuddy standard", got)
	}
	// WorkBuddy VLM 真实出站不带 Origin/Referer/Accept-Language：这些头在 VlmUpstream
	// 中被显式丢弃，出站只保留标准头族。
	for _, k := range []string{"Origin", "Referer", "Accept-Language"} {
		if got := req.Header.Get(k); got != "" {
			t.Fatalf("%s = %q, want absent (VLM 标准不出此头)", k, got)
		}
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", got)
	}
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Fatalf("X-Product = %q, want SaaS", got)
	}
	if got := req.Header.Get("X-Agent-Intent"); got != "craft" {
		t.Fatalf("X-Agent-Intent = %q, want craft", got)
	}
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Fatalf("X-Private-Data = %q, want true", got)
	}
	if got := req.Header.Get("acp-connection-id"); got == "" {
		t.Fatalf("acp-connection-id must be generated (VLM 标准必发)")
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got == "" {
		t.Fatalf("X-Conversation-Request-ID must be generated")
	}
}

// TestBillingHeadersStripForeignHeaders billing 出站同样不得透传入站平台头。
func TestBillingHeadersStripForeignHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://upstream/v2/report", nil)
	req.Header.Set("Authorization", "Bearer sk-client-key")
	req.Header.Set("x-api-key", "sk-client-key")
	c := &Client{}
	c.BillingHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"})

	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("x-api-key leaked to upstream: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at" {
		t.Fatalf("Authorization = %q, want account token", got)
	}
}

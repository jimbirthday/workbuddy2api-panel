package server

import (
	"encoding/json"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func decodeEnvelope(body string) (chatEnvelope, bool) {
	var e chatEnvelope
	if json.Unmarshal([]byte(body), &e) == nil {
		e.prepare()
		return e, true
	}
	return chatEnvelope{Stream: jsonBoolField([]byte(body), "stream"), Model: jsonStringField([]byte(body), "model")}, false
}

// TestChatEnvelopeMatchesLegacyExtractors 钉住入口统一解码不会改变路由/提示
// 判定：model/stream、工具名单、会话键、轮级键、conversationId、图片形态与
// 原辅助函数逐项一致。宽松回退另覆盖了部分字段类型畸形的情况。
func TestChatEnvelopeMatchesLegacyExtractors(t *testing.T) {
	bodies := []string{
		`{"model":"cn:glm-5.3","stream":true,"messages":[{"role":"user","content":"你好"}]}`,
		`{"model":"global:kimi-k3","metadata":{"conversationId":"m1","user_id":"u1"},"messages":[]}`,
		`{"conversation_id":"c1","prompt_cache_key":"p1","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`,
		`{"tools":[{"type":"function","function":{"name":"exec_command"}},{"name":"responses_raw"}],"functions":[{"name":"legacy"}],"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"history_tool"}}],"function_call":{"name":"history_legacy"}}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[]}`,
		`{}`,
		`{"model":"m","stream":true,"messages":[{"role":3,"content":"x"},{"role":"user","content":"y"}]}`,
	}
	for _, body := range bodies {
		e, ok := decodeEnvelope(body)
		peek := e.peek()
		var legacyPeek struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal([]byte(body), &legacyPeek)
		if peek.Stream != legacyPeek.Stream || peek.Model != legacyPeek.Model {
			t.Errorf("%s: peek=%+v legacy=%+v", body, peek, legacyPeek)
		}

		gotTools := upstream.ToolNamesFromFields(e.toolDecls(), e.functionDecls(), e.messageToolNames())
		if !ok {
			gotTools = upstream.ToolNameAllowlist([]byte(body))
		}
		wantTools := upstream.ToolNameAllowlist([]byte(body))
		if !equalStringSet(gotTools, wantTools) {
			t.Errorf("%s: tools=%v legacy=%v", body, gotTools, wantTools)
		}
		gotKey, wantKey := session.ExtractKeyFromFields(e.keyFields()), session.ExtractKey([]byte(body))
		if !ok {
			gotKey = wantKey
		}
		if got, want := gotKey, wantKey; got != want {
			t.Errorf("%s: session key=%q legacy=%q", body, got, want)
		}
		gotTurn, wantTurn := session.TurnKeyFromFields(e.turnMessages()), session.TurnKey([]byte(body))
		if !ok {
			gotTurn = wantTurn
		}
		if got, want := gotTurn, wantTurn; got != want {
			t.Errorf("%s: turn key=%q legacy=%q", body, got, want)
		}
		gotConv, wantConv := session.ResolveConversationIDFromFields(e.convFields()), session.ResolveConversationID([]byte(body))
		if !ok {
			gotConv = wantConv
		}
		if got, want := gotConv, wantConv; got != want {
			t.Errorf("%s: conv id=%q legacy=%q", body, got, want)
		}
		var legacyImage struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"messages"`
		}
		wantImage := false
		if json.Unmarshal([]byte(body), &legacyImage) == nil {
			for _, m := range legacyImage.Messages {
				for _, p := range m.Content {
					if p.Type == "image_url" {
						wantImage = true
					}
				}
			}
		}
		gotImage := e.hasImage()
		if !ok {
			gotImage = wantImage
		}
		if got, want := gotImage, wantImage; got != want {
			t.Errorf("%s: has image=%v legacy=%v", body, got, want)
		}
	}
}

func equalStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestChatEnvelopeBrokenJSON 保证两份解析都失败时各提取器仍返回安全空值，
// 不改变上游对原始坏 body 的处理路径。
func TestChatEnvelopeBrokenJSON(t *testing.T) {
	e, ok := decodeEnvelope(`{broken`)
	if ok {
		t.Fatal("broken JSON should not parse into envelope")
	}
	if e.peek().Model != "" || e.hasImage() {
		t.Fatalf("broken envelope = %+v", e)
	}
	if session.ExtractKeyFromFields(e.keyFields()) != "" || session.TurnKeyFromFields(e.turnMessages()) != "" {
		t.Fatal("broken JSON should not synthesize session keys")
	}
}

func BenchmarkChatEnvelopeFields(b *testing.B) {
	body := []byte(`{"model":"deepseek-v4.1-flash","stream":true,"conversation_id":"c1","tools":[{"type":"function","function":{"name":"exec_command"}}],"messages":[{"role":"system","content":"s"},{"role":"assistant","tool_calls":[{"function":{"name":"exec_command"}}],"function_call":{"name":"legacy"}},{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var e chatEnvelope
		if err := json.Unmarshal(body, &e); err != nil {
			b.Fatal(err)
		}
		e.prepare()
		_ = e.peek()
		_ = e.keyFields()
		_ = e.convFields()
		_ = e.turnMessages()
		_ = e.toolDecls()
		_ = e.functionDecls()
		_ = e.messageToolNames()
		_ = e.hasImage()
		toolMessages := e.messageToolNames()
		_ = upstream.ToolNamesFromFields(e.toolDecls(), e.functionDecls(), toolMessages)
	}
}

// messageToolNames 不得再为每条 tool_call 构造匿名结构体和指针；只按消息数与
// 调用数做必要切片，供入口工具名单判定复用。这里钉住固定分配，防止回退。
func BenchmarkChatEnvelopeMessageToolNames(b *testing.B) {
	e := chatEnvelope{Messages: make([]chatEnvelopeMessage, 64)}
	for i := range e.Messages {
		e.Messages[i] = chatEnvelopeMessage{
			Role: "assistant",
			ToolCalls: []upstream.ToolCallDecl{
				{Function: &upstream.FunctionCallDecl{Name: "a"}},
				{Function: &upstream.FunctionCallDecl{Name: "b"}},
			},
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := e.messageToolNames(); len(got) != len(e.Messages) || len(got[0].ToolCalls) != 2 {
			b.Fatalf("projection=%d/%d", len(got), len(got[0].ToolCalls))
		}
	}
}

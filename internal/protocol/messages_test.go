package protocol

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesToChatMapsAnthropicShapes(t *testing.T) {
	body := `{
		"model":"m","max_tokens":64,"temperature":0.2,"stream":true,
		"system":[{"type":"text","text":"be brief"}],
		"metadata":{"user_id":"u42"},
		"thinking":{"type":"enabled","budget_tokens":16000},
		"tool_choice":{"type":"any","disable_parallel_tool_use":true},
		"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},{"type":"web_search_20250305","name":"web_search"}],
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"ls"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"},{"type":"text","text":"continue"}]},
			{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
			{"role":"system","content":"mid system"}
		]
	}`
	chatBody, err := MessagesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if chat["model"] != "m" || chat["max_tokens"].(float64) != 64 || chat["stream"] != true || chat["user"] != "u42" {
		t.Errorf("core: %v", chat)
	}
	if chat["reasoning_effort"] != "high" || chat["tool_choice"] != "required" || chat["parallel_tool_calls"] != false {
		t.Errorf("controls: %v", chat)
	}
	msgs := chat["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("system first: %v", msgs[0])
	}
	var sawToolCall, sawToolResult, sawImage, sawMidSystem bool
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["tool_calls"] != nil {
			sawToolCall = true
		}
		if m["role"] == "tool" {
			sawToolResult = true
		}
		if parts, ok := m["content"].([]any); ok {
			for _, p := range parts {
				if p.(map[string]any)["type"] == "image_url" {
					sawImage = true
				}
			}
		}
		if m["role"] == "system" && strings.Contains(contentText(m["content"]), "mid system") {
			sawMidSystem = true
		}
	}
	if !sawToolCall || !sawToolResult || !sawImage || !sawMidSystem {
		t.Errorf("tool/system/image mapping incomplete: %v", msgs)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "Bash" {
		t.Errorf("tools: %v", tools)
	}
	joined, _ := json.Marshal(msgs[0])
	if !strings.Contains(string(joined), "web_search") {
		t.Errorf("skipped server tool should be noted in system: %s", joined)
	}
}

func TestMessagesToChatValidation(t *testing.T) {
	cases := []string{
		`{bad`,
		`{"messages":[]}`,
		`{"model":"m"}`,
		`{"model":"m","messages":[]}`,
		`{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
	}
	for _, body := range cases {
		if _, err := MessagesToChat([]byte(body)); err == nil {
			t.Errorf("must reject: %s", body)
		}
	}
}

func TestChatToMessages(t *testing.T) {
	chat := `{"id":"c1","model":"m","choices":[{"message":{"content":"answer","reasoning_content":"because","tool_calls":[{"id":"call_1","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"prompt_cache_hit_tokens":4}}`
	out, err := ChatToMessages([]byte(chat))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["stop_reason"] != "tool_use" || msg["type"] != "message" {
		t.Errorf("message: %v", msg)
	}
	content := msg["content"].([]any)
	if len(content) != 3 || content[0].(map[string]any)["type"] != "thinking" || content[2].(map[string]any)["type"] != "tool_use" {
		t.Errorf("content: %v", content)
	}
	u := msg["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 9 || u["cache_read_input_tokens"].(float64) != 4 {
		t.Errorf("usage: %v", u)
	}
}

func TestMessagesStreamLifecycle(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	rec := httptest.NewRecorder()
	TranslateMessagesStream(bytes.NewReader([]byte(stream)), rec, "m")
	out := rec.Body.String()
	for _, want := range []string{"message_start", "content_block_start", "thinking_delta", "text_delta", "input_json_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) || !strings.Contains(out, `"input_tokens":8`) {
		t.Errorf("final metadata: %s", out)
	}
}

func TestMessagesStreamErrorAndEmpty(t *testing.T) {
	for _, stream := range []string{
		"data: {\"error\":{\"message\":\"limited\",\"code\":6004}}\n\ndata: [DONE]\n\n",
		"data: {\"code\":6004,\"msg\":\"limited\"}\n\ndata: [DONE]\n\n",
		"data: [DONE]\n\n",
	} {
		rec := httptest.NewRecorder()
		TranslateMessagesStream(bytes.NewReader([]byte(stream)), rec, "m")
		out := rec.Body.String()
		if !strings.Contains(out, "event: error") || strings.Contains(out, "message_stop") {
			t.Errorf("must be explicit error, got:\n%s", out)
		}
	}
}

func TestCountMessagesTokensIsLocalEstimate(t *testing.T) {
	body := `{"model":"m","system":"be brief","messages":[{"role":"user","content":"hello world"}]}`
	n, err := CountMessagesTokens([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("n=%d", n)
	}
	if _, err := CountMessagesTokens([]byte(`{"messages":[]}`)); err == nil {
		t.Errorf("model is required")
	}
}

func TestMessagesStreamMissingIndexParallelTools(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"id\":\"call_a\",\"function\":{\"name\":\"A\",\"arguments\":\"{}\"}}," +
		"{\"id\":\"call_b\",\"function\":{\"name\":\"B\",\"arguments\":\"{}\"}}" +
		"]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	TranslateMessagesStream(bytes.NewReader([]byte(stream)), rec, "m")
	out := rec.Body.String()
	if strings.Count(out, `"type":"tool_use"`) != 2 || !strings.Contains(out, `"name":"A"`) || !strings.Contains(out, `"name":"B"`) {
		t.Fatalf("parallel missing-index calls merged:\n%s", out)
	}
}

func TestProtocolErrorTypes(t *testing.T) {
	var openAI map[string]any
	if err := json.Unmarshal(OpenAIError(401, "invalid_api_key", "bad key"), &openAI); err != nil {
		t.Fatal(err)
	}
	if openAI["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("openai auth type: %v", openAI)
	}
	var anthropic map[string]any
	if err := json.Unmarshal(AnthropicError(429, "slow down"), &anthropic); err != nil {
		t.Fatal(err)
	}
	if anthropic["error"].(map[string]any)["type"] != "rate_limit_error" {
		t.Fatalf("anthropic rate type: %v", anthropic)
	}
}

func TestChatToMessagesLegacyFunctionCall(t *testing.T) {
	chat := `{"id":"c1","model":"m","choices":[{"message":{"content":"","function_call":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},"finish_reason":"function_call"}]}`
	out, err := ChatToMessages([]byte(chat))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	content := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("legacy function_call lost: %v", content)
	}
	if msg["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason: %v", msg)
	}
}

func TestChatToMessagesRejectsEmptySuccess(t *testing.T) {
	cases := []string{
		`{"choices":[]}`,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
		`{"choices":[{}]}`,
		`{"choices":[{"message":{}}]}`,
	}
	for _, body := range cases {
		if _, err := ChatToMessages([]byte(body)); err == nil {
			t.Errorf("must reject empty success: %s", body)
		}
	}
}

func TestAnthropicUsagePromptCacheMissFallback(t *testing.T) {
	chat := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_cache_miss_tokens":7}}`
	out, err := ChatToMessages([]byte(chat))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	u := msg["usage"].(map[string]any)
	if u["cache_creation_input_tokens"].(float64) != 7 {
		t.Fatalf("cache miss fallback missing: %v", u)
	}
}

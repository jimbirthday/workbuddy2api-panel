package protocol

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesToChatMapsCoreFields(t *testing.T) {
	body := `{
		"model":"global:glm-5.2","instructions":"be terse",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"dev rules"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"https://example.com/a.png"}]},
			{"type":"function_call","call_id":"call_1","name":"Bash","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"additional_tools","tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}]}
		],
		"max_output_tokens":128,"reasoning":{"effort":"high"},
		"text":{"format":{"type":"json_schema","name":"out","schema":{"type":"object"}}},
		"tools":[{"type":"custom","name":"apply_patch","description":"edit"}],
		"tool_choice":"required","parallel_tool_calls":false,"stream":true
	}`
	chatBody, meta, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if chat["model"] != "global:glm-5.2" || chat["max_tokens"].(float64) != 128 || chat["reasoning_effort"] != "high" || chat["stream"] != true {
		t.Errorf("core fields: %v", chat)
	}
	if chat["parallel_tool_calls"] != false || chat["tool_choice"] != "required" {
		t.Errorf("tool controls: %v", chat)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) < 4 {
		t.Fatalf("messages: %v", msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["role"] != "system" {
		t.Errorf("instructions/developer must map to system: %v", msgs)
	}
	user := msgs[2].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("image input not preserved: %v", user)
	}
	if msgs[4].(map[string]any)["role"] != "tool" || msgs[4].(map[string]any)["tool_call_id"] != "call_1" {
		t.Errorf("function output not tool message: %v", msgs)
	}
	if _, ok := chat["response_format"].(map[string]any); !ok {
		t.Errorf("json_schema format not translated: %v", chat["response_format"])
	}
	if !meta.CustomTools["apply_patch"] {
		t.Errorf("custom tool not tracked: %v", meta.CustomTools)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools: %v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "apply_patch" || !strings.Contains(fn["description"].(string), "freeform tool") {
		t.Errorf("custom tool not downgraded: %v", fn)
	}
	if tools[1].(map[string]any)["function"].(map[string]any)["name"] != "Read" {
		t.Errorf("additional_tools not merged: %v", tools)
	}
}

func TestResponsesToChatIgnoresUnknownMetadataItems(t *testing.T) {
	chat, _, err := ResponsesToChat([]byte(`{"model":"m","input":[{"type":"future_metadata","x":1},{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("unknown metadata item must not fail the request: %v", err)
	}
	if !strings.Contains(string(chat), `"content":"hi"`) {
		t.Errorf("user input lost: %s", chat)
	}
}

func TestResponsesToChatRejectsUnsupportedFields(t *testing.T) {
	cases := []string{
		`{"model":"m","input":"x","background":true}`,
		`{"model":"m","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"m","input":"x","prompt":{"id":"p"}}`,
		`{"model":"m","input":[{"type":"reasoning","summary":[]},{"role":"tool","content":"x"}]}`,
	}
	for _, body := range cases {
		if _, _, err := ResponsesToChat([]byte(body)); err == nil {
			t.Errorf("must reject: %s", body)
		}
	}
	if _, _, err := ResponsesToChat([]byte(`{bad`)); err == nil {
		t.Errorf("invalid JSON must be rejected")
	}
}

func TestChatToResponseReinflatesCustomTool(t *testing.T) {
	chat := `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"","reasoning_content":"think","tool_calls":[{"id":"call_1","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_cache_hit_tokens":3}}`
	meta := ResponsesMeta{Model: "m", CustomTools: map[string]bool{"apply_patch": true}}
	out, err := ChatToResponse([]byte(chat), meta)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	output := resp["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output: %v", output)
	}
	call := output[1].(map[string]any)
	if call["type"] != "custom_tool_call" || call["input"] != "*** Begin Patch" {
		t.Errorf("custom call: %v", call)
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 5 || u["input_tokens_details"].(map[string]any)["cached_tokens"].(float64) != 3 {
		t.Errorf("usage: %v", u)
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"plan"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"p\":"}}]}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}`,
		`data: {"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	rec := httptest.NewRecorder()
	TranslateResponsesStream(bytes.NewReader([]byte(stream)), rec, ResponsesMeta{Model: "m", Tools: []any{}, ToolChoice: "auto"})
	out := rec.Body.String()
	for _, want := range []string{"response.created", "response.reasoning_summary_text.delta", "response.output_text.delta", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.output_item.done", "response.completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `\"p\":\"a\"`) {
		t.Errorf("tool args not reassembled: %s", out)
	}
}

func TestResponsesStreamErrorAndEmpty(t *testing.T) {
	for _, stream := range []string{
		"data: {\"error\":{\"message\":\"limited\",\"code\":6004}}\n\ndata: [DONE]\n\n",
		"data: {\"code\":6004,\"msg\":\"limited\"}\n\ndata: [DONE]\n\n",
		"data: [DONE]\n\n",
	} {
		rec := httptest.NewRecorder()
		TranslateResponsesStream(bytes.NewReader([]byte(stream)), rec, ResponsesMeta{Model: "m"})
		out := rec.Body.String()
		if !strings.Contains(out, "event: error") || strings.Contains(out, "response.completed") {
			t.Errorf("must be explicit error, got:\n%s", out)
		}
	}
}

func TestResponsesImageAndRoleOnly(t *testing.T) {
	chatBody, _, err := ResponsesToChat([]byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chatBody), `"image_url":{"url":"data:image/png;base64,AAA"}`) {
		t.Errorf("image form: %s", chatBody)
	}
	rec := httptest.NewRecorder()
	TranslateResponsesStream(bytes.NewReader([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), rec, ResponsesMeta{Model: "m"})
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("role-only stream must not be a fake success: %s", rec.Body)
	}
}

func TestResponsesStreamMissingIndexParallelTools(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"id\":\"call_a\",\"function\":{\"name\":\"A\",\"arguments\":\"{}\"}}," +
		"{\"id\":\"call_b\",\"function\":{\"name\":\"B\",\"arguments\":\"{}\"}}" +
		"]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	TranslateResponsesStream(bytes.NewReader([]byte(stream)), rec, ResponsesMeta{Model: "m"})
	out := rec.Body.String()
	if strings.Count(out, "response.output_item.done") < 2 || !strings.Contains(out, `"name":"A"`) || !strings.Contains(out, `"name":"B"`) {
		t.Fatalf("parallel missing-index calls merged:\n%s", out)
	}
}

func TestResponsesStreamCustomToolUsesCustomDelta(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"patch\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	TranslateResponsesStream(bytes.NewReader([]byte(stream)), rec, ResponsesMeta{Model: "m", CustomTools: map[string]bool{"apply_patch": true}})
	out := rec.Body.String()
	if strings.Contains(out, "response.function_call_arguments.delta") || !strings.Contains(out, "response.custom_tool_call_input.delta") || !strings.Contains(out, `"input":"patch"`) {
		t.Fatalf("custom tool stream wrong:\n%s", out)
	}
}

func TestResponsesNamespaceToolsExpandAndRestore(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"codex_apps__github","children":[
			{"name":"list_threads","description":"list","parameters":{"type":"object"}},
			{"name":"apply_patch","type":"custom"}
		]},
		{"type":"namespace","name":"node_repl","tools":[{"name":"js","input_schema":{"type":"object"}}]}
	]}`
	chatBody, meta, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Namespaces["list_threads"] != "codex_apps__github" || meta.Namespaces["js"] != "node_repl" {
		t.Fatalf("namespace map: %v", meta.Namespaces)
	}
	if !meta.CustomTools["apply_patch"] {
		t.Fatalf("nested custom tool lost: %v", meta.CustomTools)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("namespace tools not flattened: %v", tools)
	}
	for _, raw := range tools {
		if raw.(map[string]any)["type"] != "function" {
			t.Fatalf("flat function expected: %v", raw)
		}
	}

	chatResp := `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","function":{"name":"codex_apps__github__list_threads","arguments":"{}"}},{"id":"call_2","function":{"name":"node_repl::js","arguments":"{\"input\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`
	out, err := ChatToResponse([]byte(chatResp), meta)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	output := resp["output"].([]any)
	if output[0].(map[string]any)["name"] != "list_threads" || output[0].(map[string]any)["namespace"] != "codex_apps__github" {
		t.Fatalf("first namespace: %v", output[0])
	}
	if output[1].(map[string]any)["name"] != "js" || output[1].(map[string]any)["namespace"] != "node_repl" {
		t.Fatalf("second namespace: %v", output[1])
	}
}

func TestResponsesStreamNamespaceRestored(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"node_repl__js\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	TranslateResponsesStream(bytes.NewReader([]byte(stream)), rec, ResponsesMeta{
		Model: "m", Namespaces: map[string]string{"js": "node_repl"},
	})
	out := rec.Body.String()
	if !strings.Contains(out, `"name":"js"`) || !strings.Contains(out, `"namespace":"node_repl"`) {
		t.Fatalf("stream namespace not restored: %s", out)
	}
}

func TestChatToResponseLegacyFunctionCall(t *testing.T) {
	chat := `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":null,"function_call":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},"finish_reason":"function_call"}]}`
	out, err := ChatToResponse([]byte(chat), ResponsesMeta{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	output := resp["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["type"] != "function_call" {
		t.Fatalf("legacy function_call lost: %v", output)
	}
	if output[0].(map[string]any)["name"] != "Bash" || output[0].(map[string]any)["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("legacy function_call fields: %v", output[0])
	}
}

func TestChatToResponseRejectsEmptySuccess(t *testing.T) {
	cases := []string{
		`{"choices":[]}`,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
		`{"choices":[{}]}`,
		`{"choices":[{"message":{}}]}`,
	}
	for _, body := range cases {
		if _, err := ChatToResponse([]byte(body), ResponsesMeta{Model: "m"}); err == nil {
			t.Errorf("must reject empty success: %s", body)
		}
	}
}

func TestResponsesNestedNamespaceCustomTool(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"outer","tools":[
			{"type":"namespace","name":"inner","tools":[
				{"name":"apply_patch","type":"custom"}
			]}
		]}
	]}`
	chatBody, meta, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.CustomTools["apply_patch"] || meta.Namespaces["apply_patch"] != "outer__inner" {
		t.Fatalf("nested custom metadata: custom=%v namespaces=%v", meta.CustomTools, meta.Namespaces)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	tool := chat["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tool["name"] != "outer__inner__apply_patch" {
		t.Fatalf("nested tool must be uniquely prefixed: %v", tool)
	}

	chatResp := `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_1","function":{"name":"outer__inner__apply_patch","arguments":"{\"input\":\"PATCH\"}"}}]},"finish_reason":"tool_calls"}]}`
	out, err := ChatToResponse([]byte(chatResp), meta)
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	item := resp["output"].([]any)[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "apply_patch" || item["namespace"] != "outer__inner" || item["input"] != "PATCH" {
		t.Fatalf("nested custom call not restored: %v", item)
	}
}

func TestResponsesNamespaceDepthLimitRejected(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[
		{"type":"namespace","name":"n1","tools":[
			{"type":"namespace","name":"n2","tools":[
				{"type":"namespace","name":"n3","tools":[
					{"type":"namespace","name":"n4","tools":[
						{"type":"namespace","name":"n5","tools":[
							{"name":"deep","parameters":{"type":"object"}}
						]}
					]}
				]}
			]}
		]}
	]}`
	if _, _, err := ResponsesToChat([]byte(body)); err == nil {
		t.Fatalf("over-deep namespace must be rejected, not silently dropped")
	}
}

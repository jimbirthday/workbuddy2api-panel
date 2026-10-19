package protocol

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtocolStreamsPreserveWhitespace(t *testing.T) {
	text := []string{"Hi!", " ", "What", " would", " you", " like", " to", " work", " on?", "\n", "    code", "\t", "　"}
	args := []string{"{\n", "  ", `"text":"`, "Hi!", " ", "What", " would", " you", " like", " to", " work", " on?", `"`, "\n", "}"}
	for _, proto := range []string{"responses", "messages"} {
		t.Run(proto, func(t *testing.T) {
			rr := httptest.NewRecorder()
			var s interface {
				Feed([]byte)
				Finish() error
			}
			if proto == "responses" {
				s = NewResponsesStream(rr, ResponsesMeta{Model: "m"})
			} else {
				s = NewMessagesStream(rr, "m")
			}
			feed := func(delta map[string]any) {
				raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
				if err != nil {
					t.Fatal(err)
				}
				s.Feed(raw)
			}
			for _, field := range []string{"reasoning_content", "content"} {
				for _, fragment := range text {
					feed(map[string]any{field: fragment})
				}
			}
			feed(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "echo"}}}})
			for _, fragment := range args {
				feed(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": fragment}}}})
			}
			if err := s.Finish(); err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, line := range strings.Split(rr.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				switch event["type"] {
				case "response.output_text.delta":
					got["text"] += event["delta"].(string)
				case "response.reasoning_summary_text.delta":
					got["thinking"] += event["delta"].(string)
				case "response.function_call_arguments.delta":
					got["args"] += event["delta"].(string)
				case "content_block_delta":
					delta := event["delta"].(map[string]any)
					for field, key := range map[string]string{"text": "text", "thinking": "thinking", "partial_json": "args"} {
						if value, ok := delta[field].(string); ok {
							got[key] += value
						}
					}
				}
			}
			for key, want := range map[string]string{"text": strings.Join(text, ""), "thinking": strings.Join(text, ""), "args": strings.Join(args, "")} {
				if got[key] != want {
					t.Errorf("%s whitespace lost: got %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

func TestProtocolContentPreservesWhitespace(t *testing.T) {
	want := " \n    Hi! What would you like to work on?\t　\n"
	if got := contentText([]any{map[string]any{"text": want}}); got != want {
		t.Errorf("contentText=%q, want %q", got, want)
	}
	if got := reasoningText(map[string]any{"summary": []any{map[string]any{"text": want}}}); got != want {
		t.Errorf("reasoningText=%q, want %q", got, want)
	}
	for _, proto := range []string{"responses", "messages"} {
		t.Run(proto, func(t *testing.T) {
			var body []byte
			var err error
			if proto == "responses" {
				raw, _ := json.Marshal(map[string]any{"model": "m", "instructions": want, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": want}}}}})
				body, _, err = ResponsesToChat(raw)
			} else {
				raw, _ := json.Marshal(map[string]any{"model": "m", "system": []any{map[string]any{"type": "text", "text": want}}, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": want}}}}})
				body, err = MessagesToChat(raw)
			}
			if err != nil {
				t.Fatal(err)
			}
			var chat struct {
				Messages []struct{ Content string }
			}
			if err := json.Unmarshal(body, &chat); err != nil {
				t.Fatal(err)
			}
			if len(chat.Messages) != 2 {
				t.Fatalf("messages=%s", body)
			}
			for _, message := range chat.Messages {
				if message.Content != want {
					t.Errorf("request whitespace lost: got %q, want %q", message.Content, want)
				}
			}
			raw, _ := json.Marshal(map[string]any{"model": "m", "choices": []any{map[string]any{"message": map[string]any{"content": []any{map[string]any{"text": want}}, "reasoning_content": want}}}})
			if proto == "responses" {
				body, err = ChatToResponse(raw, ResponsesMeta{Model: "m"})
			} else {
				body, err = ChatToMessages(raw)
			}
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			if proto == "responses" {
				if result["output_text"] != want {
					t.Errorf("response text=%q, want %q", result["output_text"], want)
				}
				items := result["output"].([]any)
				result = items[0].(map[string]any)["summary"].([]any)[0].(map[string]any)
				if result["text"] != want {
					t.Errorf("reasoning=%q, want %q", result["text"], want)
				}
			} else {
				for _, raw := range result["content"].([]any) {
					block := raw.(map[string]any)
					field := "text"
					if block["type"] == "thinking" {
						field = "thinking"
					}
					if block[field] != want {
						t.Errorf("%s=%q, want %q", field, block[field], want)
					}
				}
			}
		})
	}
}

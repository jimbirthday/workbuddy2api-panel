package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// MessagesToChat translates an Anthropic Messages request into Chat Completions.
func MessagesToChat(body []byte) ([]byte, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	model := stringValue(obj["model"])
	if model == "" {
		return nil, badRequest("invalid_request", "model is required")
	}
	rawMessages, ok := obj["messages"].([]any)
	if !ok {
		return nil, badRequest("invalid_request", "messages must be a list")
	}
	if len(rawMessages) == 0 {
		return nil, badRequest("invalid_request", "messages must not be empty")
	}
	chat := map[string]any{"model": model, "messages": []any{}}
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := obj[key]; ok && v != nil {
			chat[key] = v
		}
	}
	if v, ok := obj["max_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	if v, ok := obj["stream"]; ok && v != nil {
		chat["stream"] = v
	}
	if stops, ok := obj["stop_sequences"].([]any); ok && len(stops) > 0 {
		chat["stop"] = stops
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if user := stringValue(md["user_id"]); user != "" {
			chat["user"] = user
		}
	}

	messages := make([]any, 0, len(rawMessages)+1)
	system := anthropicSystemText(obj["system"])
	if system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	skipped := make([]string, 0)
	if tools, ok := obj["tools"].([]any); ok {
		converted, unsupported := anthropicTools(tools)
		if len(converted) > 0 {
			chat["tools"] = converted
		}
		skipped = unsupported
	}
	if len(skipped) > 0 {
		note := "These Anthropic server tools are not available here: " + strings.Join(skipped, ", ") + ". Do not call them."
		if system != "" {
			messages[0] = map[string]any{"role": "system", "content": system + "\n" + note}
		} else {
			messages = append([]any{map[string]any{"role": "system", "content": note}}, messages...)
		}
	}
	if choice, ok := anthropicToolChoice(obj["tool_choice"]); ok {
		chat["tool_choice"] = choice
	}
	if tc, ok := obj["tool_choice"].(map[string]any); ok {
		if disable, _ := boolValue(tc["disable_parallel_tool_use"]); disable {
			chat["parallel_tool_calls"] = false
		}
	}
	if effort := anthropicEffort(obj); effort != "" {
		chat["reasoning_effort"] = effort
	}
	for i, raw := range rawMessages {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, badRequest("invalid_request", "messages[%d] must be an object", i)
		}
		role := strings.ToLower(stringValue(item["role"]))
		if role == "" {
			role = "user"
		}
		content := item["content"]
		if role == "system" || role == "developer" {
			text := anthropicSystemText(content)
			if text == "" {
				text = anthropicContentText(content)
			}
			if text != "" {
				messages = append(messages, map[string]any{"role": "system", "content": text})
			}
			continue
		}
		if role != "user" && role != "assistant" {
			return nil, badRequest("invalid_request", "messages[%d].role %q must be user or assistant", i, role)
		}
		switch c := content.(type) {
		case string:
			messages = append(messages, map[string]any{"role": role, "content": c})
		case []any:
			converted, err := anthropicBlocks(role, c)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted...)
		case nil:
			messages = append(messages, map[string]any{"role": role, "content": ""})
		default:
			messages = append(messages, map[string]any{"role": role, "content": fmt.Sprint(c)})
		}
	}
	chat["messages"] = messages
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, &ConvertError{Status: http.StatusInternalServerError, Code: "encode_request", Message: err.Error()}
	}
	return out, nil
}

func anthropicSystemText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	parts, ok := v.([]any)
	if !ok {
		return ""
	}
	var out []string
	for _, raw := range parts {
		if m, ok := raw.(map[string]any); ok {
			if s := rawStringValue(m["text"]); s != "" {
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, "\n")
}

func anthropicContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var out []string
		for _, raw := range c {
			if m, ok := raw.(map[string]any); ok {
				if s := rawStringValue(m["text"]); s != "" {
					out = append(out, s)
				} else if inner := anthropicContentText(m["content"]); inner != "" {
					out = append(out, inner)
				}
			}
		}
		return strings.Join(out, "")
	case nil:
		return ""
	default:
		return fmt.Sprint(c)
	}
}

func anthropicTools(tools []any) ([]any, []string) {
	out := make([]any, 0, len(tools))
	var skipped []string
	for _, raw := range tools {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := item["function"].(map[string]any); ok {
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		name := stringValue(item["name"])
		if name == "" {
			name = stringValue(item["type"])
		}
		schema := item["input_schema"]
		if schema == nil {
			schema = item["parameters"]
		}
		if schema == nil {
			if name != "" {
				skipped = append(skipped, name)
			}
			continue
		}
		fn := map[string]any{"name": name, "parameters": schema}
		if desc := rawStringValue(item["description"]); desc != "" {
			fn["description"] = desc
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, skipped
}

func anthropicToolChoice(v any) (any, bool) {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "auto":
			return "auto", true
		case "any", "required":
			return "required", true
		case "none":
			return "none", true
		}
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	switch strings.ToLower(stringValue(m["type"])) {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		name := stringValue(m["name"])
		if name == "" {
			return nil, false
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
	}
	return nil, false
}

func anthropicEffort(obj map[string]any) string {
	if cfg, ok := obj["output_config"].(map[string]any); ok {
		if effort := stringValue(cfg["effort"]); effort != "" {
			return effort
		}
	}
	thinking, ok := obj["thinking"].(map[string]any)
	if !ok {
		return ""
	}
	typ := strings.ToLower(stringValue(thinking["type"]))
	if typ == "disabled" {
		return "none"
	}
	if typ != "enabled" {
		return ""
	}
	budget, _ := intValue(thinking["budget_tokens"])
	switch {
	case budget >= 32000:
		return "xhigh"
	case budget >= 16000:
		return "high"
	case budget >= 8000:
		return "medium"
	case budget > 0:
		return "low"
	}
	return ""
}

func anthropicBlocks(role string, blocks []any) ([]any, error) {
	var toolCalls, toolMessages []any
	var contentParts []any
	var texts []string
	hasNonText := false
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.ToLower(stringValue(block["type"]))
		switch typ {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use", "server_tool_use":
			input := block["input"]
			args, _ := json.Marshal(input)
			if input == nil {
				args = []byte("{}")
			}
			id := stringValue(block["id"])
			if id == "" {
				id = newID("toolu_")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": stringValue(block["name"]), "arguments": string(args)},
			})
		case "tool_result":
			text := anthropicContentText(block["content"])
			if isErr, _ := boolValue(block["is_error"]); isErr {
				text = "error: " + text
			}
			toolMessages = append(toolMessages, map[string]any{
				"role": "tool", "tool_call_id": stringValue(block["tool_use_id"]), "content": text,
			})
		case "image":
			if url := anthropicImageURL(block); url != "" {
				hasNonText = true
				contentParts = append(contentParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "document":
			hasNonText = true
			contentParts = append(contentParts, map[string]any{"type": "text", "text": anthropicDocumentText(block)})
		default:
			text := rawStringValue(block["text"])
			if text == "" {
				text = anthropicContentText(block["content"])
			}
			texts = append(texts, text)
			contentParts = append(contentParts, map[string]any{"type": "text", "text": text})
		}
	}
	out := append([]any{}, toolMessages...)
	if len(toolCalls) == 0 && len(texts) == 0 && !hasNonText {
		return out, nil
	}
	msg := map[string]any{"role": role}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if hasNonText {
		msg["content"] = contentParts
	} else {
		msg["content"] = strings.Join(texts, "")
	}
	out = append(out, msg)
	return out, nil
}

func anthropicImageURL(block map[string]any) string {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return ""
	}
	switch strings.ToLower(stringValue(source["type"])) {
	case "base64":
		mime := stringValue(source["media_type"])
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + stringValue(source["data"])
	case "url":
		return stringValue(source["url"])
	case "file":
		if s := stringValue(source["file_id"]); s != "" {
			return s
		}
		return stringValue(source["url"])
	}
	return ""
}

func anthropicDocumentText(block map[string]any) string {
	title := stringValue(block["title"])
	source, _ := block["source"].(map[string]any)
	body := ""
	if source != nil {
		switch strings.ToLower(stringValue(source["type"])) {
		case "text":
			body = stringValue(source["data"])
		case "content":
			body = anthropicContentText(source["content"])
		case "base64":
			mime := stringValue(source["media_type"])
			if mime == "" {
				mime = "application/octet-stream"
			}
			body = "[document " + mime + " omitted]"
		case "url":
			body = stringValue(source["url"])
		case "file":
			body = stringValue(source["file_id"])
			if body == "" {
				body = stringValue(source["url"])
			}
		}
	}
	if title != "" {
		return title + "\n" + body
	}
	return body
}

// ChatToMessages folds one Chat Completions object into an Anthropic message.
func ChatToMessages(body []byte) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, &ConvertError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned invalid JSON: " + err.Error()}
	}
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return nil, &ConvertError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned no choices"}
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return nil, &ConvertError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned an invalid choice"}
	}
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return nil, &ConvertError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned no message"}
	}
	finish := stringValue(choice["finish_reason"])
	content := make([]any, 0, 4)
	if reasoning := rawStringValue(msg["reasoning_content"]); reasoning != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	if text := contentText(msg["content"]); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range chatMessageToolCalls(msg) {
		name := stringValue(call["name"])
		if name == "" {
			continue
		}
		id := stringValue(call["id"])
		if id == "" {
			id = newID("toolu_")
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": id, "name": name,
			"input": parseToolInput(rawStringValue(call["arguments"])),
		})
	}
	if len(content) == 0 {
		return nil, &ConvertError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned an empty assistant message"}
	}
	return json.Marshal(map[string]any{
		"id": anthropicMessageID(stringValue(chat["id"])), "type": "message", "role": "assistant",
		"model": stringValue(chat["model"]), "content": content,
		"stop_reason": anthropicStopReason(finish), "stop_sequence": nil,
		"usage": anthropicUsage(chat["usage"]),
	})
}

// chatMessageToolCalls normalizes both modern tool_calls entries and the
// legacy single function_call object into {id, name, arguments} maps.
func chatMessageToolCalls(msg map[string]any) []map[string]any {
	if msg == nil {
		return nil
	}
	calls := make([]map[string]any, 0, 2)
	if rawCalls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range rawCalls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				continue
			}
			calls = append(calls, map[string]any{
				"id": stringValue(call["id"]), "name": stringValue(fn["name"]), "arguments": rawStringValue(fn["arguments"]),
			})
		}
	}
	if fn, ok := msg["function_call"].(map[string]any); ok {
		calls = append(calls, map[string]any{
			"id": stringValue(msg["tool_call_id"]), "name": stringValue(fn["name"]), "arguments": rawStringValue(fn["arguments"]),
		})
	}
	return calls
}

func parseToolInput(args string) any {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}
	}
	var parsed any
	if json.Unmarshal([]byte(args), &parsed) != nil {
		return map[string]any{"raw": args}
	}
	if m, ok := parsed.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": parsed}
}

func anthropicMessageID(id string) string {
	if id == "" {
		return newID("msg_")
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func anthropicStopReason(finish string) string {
	switch strings.ToLower(finish) {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter", "refusal":
		return "refusal"
	}
	return "end_turn"
}

func anthropicUsage(raw any) map[string]any {
	out := map[string]any{
		"input_tokens": 0, "cache_creation_input_tokens": 0,
		"cache_read_input_tokens": 0, "output_tokens": 0,
	}
	u, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	if n, ok := intValue(u["prompt_tokens"]); ok {
		out["input_tokens"] = n
	}
	if n, ok := intValue(u["completion_tokens"]); ok {
		out["output_tokens"] = n
	}
	if n, ok := intValue(firstNonNil(u["prompt_cache_hit_tokens"], nestedInt(u, "prompt_tokens_details", "cached_tokens"))); ok {
		out["cache_read_input_tokens"] = n
	}
	if n, ok := intValue(firstNonNil(
		u["prompt_cache_write_tokens"],
		u["prompt_cache_miss_tokens"],
		nestedInt(u, "prompt_tokens_details", "cache_creation_input_tokens"),
		nestedInt(u, "prompt_tokens_details", "cache_miss_tokens"),
	)); ok {
		out["cache_creation_input_tokens"] = n
	}
	return out
}

func nestedInt(m map[string]any, parent, key string) any {
	p, _ := m[parent].(map[string]any)
	if p == nil {
		return nil
	}
	return p[key]
}

// CountMessagesTokens estimates the input token count locally. The Anthropic
// API returns an estimate too when no model tokenizer is available; this
// gateway never sends a billable upstream request for it.
func CountMessagesTokens(body []byte) (int64, error) {
	obj, err := decodeObject(body)
	if err != nil {
		return 0, err
	}
	if stringValue(obj["model"]) == "" {
		return 0, badRequest("invalid_request", "model is required")
	}
	var sb strings.Builder
	sb.WriteString(anthropicSystemText(obj["system"]))
	if tools, ok := obj["tools"].([]any); ok {
		for _, raw := range tools {
			b, _ := json.Marshal(raw)
			sb.Write(b)
			sb.WriteByte('\n')
		}
	}
	if msgs, ok := obj["messages"].([]any); ok {
		for _, raw := range msgs {
			b, _ := json.Marshal(raw)
			sb.Write(b)
			sb.WriteByte('\n')
		}
	}
	n := EstimateTokens(sb.String())
	if n < 1 {
		n = 1
	}
	return int64(n), nil
}

// EstimateTokens is a conservative local estimate: one token per CJK rune and
// one token per four bytes of other content, with a floor of one.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	var cjk, other int
	for _, r := range s {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// AnthropicError renders the native Messages error envelope.
func AnthropicError(status int, message string) []byte {
	return AnthropicErrorDetail(status, ErrorDetail{Message: message})
}

// AnthropicErrorDetail renders the native Messages error envelope while
// preserving the upstream request id and the original provider error object.
func AnthropicErrorDetail(status int, detail ErrorDetail) []byte {
	message := strings.TrimSpace(detail.Message)
	if message == "" {
		message = "upstream request failed"
	}
	errObj := map[string]any{"type": anthropicErrorType(status), "message": message}
	if detail.RequestID != "" {
		errObj["request_id"] = detail.RequestID
	}
	if len(detail.Original) > 0 {
		errObj["upstream"] = detail.Original
	}
	out := map[string]any{"type": "error", "error": errObj}
	if detail.RequestID != "" {
		out["request_id"] = detail.RequestID
	}
	body, _ := json.Marshal(out)
	return body
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	return "api_error"
}

// OpenAIError renders the OpenAI-shaped error envelope used by /v1/responses.
func OpenAIError(status int, code, message string) []byte {
	return OpenAIErrorDetail(status, code, ErrorDetail{Message: message, Code: code})
}

// OpenAIErrorDetail renders the OpenAI-shaped error envelope while preserving
// the upstream request id and the original provider error object.
func OpenAIErrorDetail(status int, code string, detail ErrorDetail) []byte {
	message := strings.TrimSpace(detail.Message)
	if message == "" {
		message = "upstream request failed"
	}
	if code == "" {
		code = detail.Code
	}
	errObj := map[string]any{"message": message, "type": openAIErrorType(status, code)}
	if code != "" {
		errObj["code"] = code
	}
	if detail.RequestID != "" {
		errObj["request_id"] = detail.RequestID
	}
	if len(detail.Original) > 0 {
		errObj["upstream"] = detail.Original
	}
	out := map[string]any{"error": errObj}
	if detail.RequestID != "" {
		out["request_id"] = detail.RequestID
	}
	body, _ := json.Marshal(out)
	return body
}

func openAIErrorType(status int, code string) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if strings.EqualFold(strings.TrimSpace(code), "invalid_api_key") {
			return "authentication_error"
		}
		return "invalid_request_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	return "api_error"
}

package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ResponsesMeta is the subset of the original Responses request that must be
// echoed back on the response, plus custom-tool names for re-inflating freeform
// calls.
type ResponsesMeta struct {
	Model             string
	Tools             []any
	ToolChoice        any
	ParallelToolCalls any
	CustomTools       map[string]bool
	Namespaces        map[string]string
	InputText         string
}

const namespaceMaxDepth = 4

const customToolHint = "This is a freeform tool. Put the COMPLETE raw payload into the single 'input' string parameter, verbatim. Do not wrap it in JSON, do not wrap it in markdown code fences, do not add commentary."

// ResponsesToChat translates an OpenAI Responses request into Chat Completions.
func ResponsesToChat(body []byte) ([]byte, ResponsesMeta, error) {
	var meta ResponsesMeta
	obj, err := decodeObject(body)
	if err != nil {
		return nil, meta, err
	}
	if truthy(obj["background"]) {
		return nil, meta, badRequest("unsupported_field", "background responses are not supported")
	}
	if s := stringValue(obj["previous_response_id"]); s != "" {
		return nil, meta, badRequest("unsupported_field", "previous_response_id is not supported; send the full conversation in input")
	}
	if p, ok := obj["prompt"]; ok && p != nil {
		return nil, meta, badRequest("unsupported_field", "prompt templates are not supported; send messages in input")
	}

	meta.Model = stringValue(obj["model"])
	if meta.Model == "" {
		return nil, meta, badRequest("invalid_request", "model is required")
	}
	messages, err := responsesMessages(obj)
	if err != nil {
		return nil, meta, err
	}
	chat := map[string]any{"model": meta.Model, "messages": messages}
	for _, key := range []string{"temperature", "top_p", "seed"} {
		if v, ok := obj[key]; ok && v != nil {
			chat[key] = v
		}
	}
	if v, ok := obj["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	} else if v, ok := obj["max_tokens"]; ok && v != nil {
		// Some Responses-compatible clients still send the Chat field. Accept
		// it rather than silently dropping the caller's output budget.
		chat["max_tokens"] = v
	}
	// Chat Completions accepts stop as string or string[]; pass it through and
	// let the outbound normalizer produce the upstream []string shape.
	if v, ok := obj["stop"]; ok && v != nil {
		chat["stop"] = v
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			chat["reasoning_effort"] = effort
		}
	}
	if effort := stringValue(obj["reasoning_effort"]); effort != "" {
		chat["reasoning_effort"] = effort
	}
	if v, ok := obj["metadata"]; ok && v != nil {
		chat["metadata"] = v
	}
	if v := stringValue(obj["user"]); v != "" {
		chat["user"] = v
	}
	if v, ok := obj["parallel_tool_calls"]; ok && v != nil {
		chat["parallel_tool_calls"] = v
		meta.ParallelToolCalls = v
	}
	if v, ok := obj["tool_choice"]; ok && v != nil {
		chat["tool_choice"] = v
		meta.ToolChoice = v
	}
	if stream, ok := obj["stream"].(bool); ok {
		chat["stream"] = stream
	}
	if text, ok := obj["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			typ := strings.ToLower(stringValue(format["type"]))
			if typ == "json_schema" {
				schema := map[string]any{"name": stringValue(format["name"]), "schema": format["schema"]}
				if v, ok := format["strict"]; ok {
					schema["strict"] = v
				}
				chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
			} else if typ == "json_object" {
				chat["response_format"] = map[string]any{"type": "json_object"}
			}
		}
	}

	rawTools := make([]any, 0, 4)
	if tools, ok := obj["tools"].([]any); ok {
		rawTools = append(rawTools, tools...)
	}
	rawTools = append(rawTools, additionalTools(obj)...)
	meta.Tools = rawTools
	if len(rawTools) > 0 {
		converted, custom, namespaces, err := responsesTools(rawTools)
		if err != nil {
			return nil, meta, err
		}
		meta.CustomTools = custom
		meta.Namespaces = namespaces
		chat["tools"] = converted
	}
	meta.InputText = requestInputText(obj)
	out, err := json.Marshal(chat)
	if err != nil {
		return nil, meta, &ConvertError{Status: http.StatusInternalServerError, Code: "encode_request", Message: err.Error()}
	}
	return out, meta, nil
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

func responsesMessages(obj map[string]any) ([]any, error) {
	messages := make([]any, 0, 4)
	if instructions := stringValue(obj["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	input := obj["input"]
	if s, ok := input.(string); ok {
		if s != "" {
			messages = append(messages, map[string]any{"role": "user", "content": s})
		}
		return messages, nil
	}
	items, ok := input.([]any)
	if !ok {
		if input == nil {
			return nil, badRequest("invalid_input", "input is required")
		}
		return nil, badRequest("invalid_input", "input must be a string or a list of items")
	}
	pendingReasoning := ""
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			if s, ok := raw.(string); ok {
				messages = append(messages, map[string]any{"role": "user", "content": s})
				continue
			}
			return nil, badRequest("invalid_input", "input[%d] must be an object or string", i)
		}
		typ := strings.ToLower(stringValue(item["type"]))
		switch typ {
		case "", "message", "user", "assistant", "system", "developer":
			content := flattenContent(item["content"])
			if content == nil {
				continue
			}
			role := strings.ToLower(stringValue(item["role"]))
			if typ != "" && typ != "message" {
				role = typ
			}
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			if role == "tool" {
				return nil, badRequest("invalid_input", "input[%d] role \"tool\" requires function_call_output with call_id", i)
			}
			msg := map[string]any{"role": role, "content": content}
			if role == "assistant" && pendingReasoning != "" {
				msg["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			}
			messages = appendResponseMessage(messages, msg)
		case "reasoning":
			text := reasoningText(item)
			if text != "" {
				if n := len(messages); n > 0 {
					if prev, ok := messages[n-1].(map[string]any); ok && prev["role"] == "assistant" {
						prev["reasoning_content"] = text
					} else {
						pendingReasoning = text
					}
				} else {
					pendingReasoning = text
				}
			}
		case "function_call", "custom_tool_call":
			callID := stringValue(item["call_id"])
			if callID == "" {
				callID = stringValue(item["id"])
			}
			var argText string
			if typ == "custom_tool_call" {
				argJSON, _ := json.Marshal(map[string]any{"input": stringValue(item["input"])})
				argText = string(argJSON)
			} else if s, ok := item["arguments"].(string); ok {
				argText = s
			} else {
				argJSON, _ := json.Marshal(item["arguments"])
				argText = string(argJSON)
			}
			if argText == "" || argText == "null" {
				argText = "{}"
			}
			msg := map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": stringValue(item["name"]), "arguments": argText},
				}},
			}
			if pendingReasoning != "" {
				msg["reasoning_content"] = pendingReasoning
				pendingReasoning = ""
			}
			messages = appendResponseMessage(messages, msg)
		case "function_call_output", "custom_tool_call_output":
			callID := stringValue(item["call_id"])
			out := flattenContent(item["output"])
			text := contentText(out)
			if callID == "" {
				if text != "" {
					messages = append(messages, map[string]any{"role": "user", "content": "[Tool output from another task]\n\n" + text})
				}
				continue
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": text})
		case "additional_tools", "item_reference", "web_search_call":
			// additional_tools is merged into the top-level tool list; the other
			// item types are metadata-only for this gateway.
		case "agent_message":
			// Sub-agent side channel: give the model the text as user context
			// instead of silently dropping a real instruction.
			if text := contentText(flattenContent(item["content"])); text != "" {
				messages = append(messages, map[string]any{"role": "user",
					"content": "[Message from another task - treat this as a user instruction]\n\n" + text})
			}
		default:
			// Forward-compatible: Responses grows new metadata item types
			// (compaction, local tool audit, etc.). They are not model input,
			// so ignore them rather than failing an otherwise valid request.
			_ = i
		}
	}
	return messages, nil
}

// appendResponseMessage merges adjacent assistant messages so tool calls and
// text from a Responses replay stay in one Chat Completions message.
func appendResponseMessage(messages []any, msg map[string]any) []any {
	if len(messages) == 0 || msg["role"] != "assistant" {
		return append(messages, msg)
	}
	prev, ok := messages[len(messages)-1].(map[string]any)
	if !ok || prev["role"] != "assistant" {
		return append(messages, msg)
	}
	if txt, ok := msg["content"].(string); ok && txt != "" {
		if old, _ := prev["content"].(string); old != "" {
			prev["content"] = old + "\n" + txt
		} else {
			prev["content"] = txt
		}
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		old, _ := prev["tool_calls"].([]any)
		prev["tool_calls"] = append(old, calls...)
	}
	if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
		prev["reasoning_content"] = rc
	}
	return messages
}

func reasoningText(item map[string]any) string {
	if s := stringValue(item["summary"]); s != "" {
		return s
	}
	if summary, ok := item["summary"].([]any); ok {
		var parts []string
		for _, p := range summary {
			if m, ok := p.(map[string]any); ok {
				if s := stringValue(m["text"]); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if s := stringValue(item["content"]); s != "" {
		return s
	}
	if c := flattenContent(item["content"]); c != nil {
		return contentText(c)
	}
	return ""
}

func additionalTools(obj map[string]any) []any {
	var out []any
	input, _ := obj["input"].([]any)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || strings.ToLower(stringValue(item["type"])) != "additional_tools" {
			continue
		}
		if tools, ok := item["tools"].([]any); ok {
			out = append(out, tools...)
		}
	}
	return out
}

func requestInputText(obj map[string]any) string {
	var parts []string
	if s := stringValue(obj["instructions"]); s != "" {
		parts = append(parts, s)
	}
	switch input := obj["input"].(type) {
	case string:
		parts = append(parts, input)
	case []any:
		for _, item := range input {
			if m, ok := item.(map[string]any); ok {
				parts = append(parts, contentText(flattenContent(m["content"])))
			} else if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func responsesTools(raw []any) ([]any, map[string]bool, map[string]string, error) {
	raw, namespaces, err := expandNamespaceTools(raw)
	if err != nil {
		return nil, nil, nil, err
	}
	out := make([]any, 0, len(raw))
	custom := map[string]bool{}
	for i, v := range raw {
		tool, ok := v.(map[string]any)
		if !ok {
			return nil, nil, nil, badRequest("invalid_tools", "tools[%d] must be an object", i)
		}
		typ := strings.ToLower(stringValue(tool["type"]))
		if fn, ok := tool["function"].(map[string]any); ok {
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		name := stringValue(tool["name"])
		if name == "" {
			return nil, nil, nil, badRequest("invalid_tools", "tools[%d].name is required", i)
		}
		desc := stringValue(tool["description"])
		params := tool["parameters"]
		if params == nil {
			params = tool["input_schema"]
		}
		if typ == "custom" {
			custom[name] = true
			_, ns := resolveNamespacedName(name, namespaces)
			bare := name
			if ns != "" {
				prefix := ns + "__"
				if strings.HasPrefix(name, prefix) {
					bare = strings.TrimPrefix(name, prefix)
				}
			}
			if bare != "" {
				custom[bare] = true
			}
			if desc != "" {
				desc += "\n\n"
			}
			desc += customToolHint
			params = map[string]any{
				"type": "object",
				"properties": map[string]any{"input": map[string]any{
					"type": "string", "description": "Complete raw payload for this tool, verbatim.",
				}},
				"required": []any{"input"},
			}
		} else if typ != "" && typ != "function" {
			return nil, nil, nil, badRequest("unsupported_tool_type", "tools[%d] type %q is not supported", i, typ)
		}
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		fn := map[string]any{"name": name, "parameters": params}
		if desc != "" {
			fn["description"] = desc
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, custom, namespaces, nil
}

// expandNamespaceTools flattens Responses namespace tools into regular
// function declarations and records both the bare and flattened names so the
// return trip can restore namespace/name. Nested namespaces are combined into
// a stable namespace path; definitions deeper than namespaceMaxDepth are
// rejected instead of silently disappearing.
func expandNamespaceTools(raw []any) ([]any, map[string]string, error) {
	flat := make([]any, 0, len(raw))
	namespaces := map[string]string{}
	seen := map[string]bool{}
	var collect func(any, int, string) error
	collect = func(v any, depth int, ns string) error {
		tool, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if depth > namespaceMaxDepth {
			return badRequest("invalid_tools", "namespace nesting exceeds %d levels", namespaceMaxDepth)
		}
		if strings.ToLower(stringValue(tool["type"])) == "namespace" {
			childNS := stringValue(tool["name"])
			if childNS == "" {
				childNS = ns
			} else if ns != "" && childNS != ns && !strings.HasPrefix(childNS, ns+"__") {
				childNS = ns + "__" + childNS
			}
			children, _ := tool["tools"].([]any)
			if children == nil {
				children, _ = tool["children"].([]any)
			}
			if children == nil {
				children, _ = tool["functions"].([]any)
			}
			for _, child := range children {
				if err := collect(child, depth+1, childNS); err != nil {
					return err
				}
			}
			return nil
		}
		fn, _ := tool["function"].(map[string]any)
		name := stringValue(tool["name"])
		if name == "" && fn != nil {
			name = stringValue(fn["name"])
		}
		if name == "" {
			return nil
		}
		exportName := name
		if ns != "" && !strings.HasPrefix(name, ns+"__") && !strings.HasPrefix(name, ns+"::") {
			exportName = ns + "__" + name
		}
		if seen[exportName] {
			return nil
		}
		seen[exportName] = true
		if ns != "" && exportName != name && !seen[name] {
			if _, exists := namespaces[name]; !exists {
				namespaces[name] = ns
			}
		}
		typ := strings.ToLower(stringValue(tool["type"]))
		if fn != nil || ns == "" || typ == "custom" {
			if exportName != name {
				copyTool := make(map[string]any, len(tool)+1)
				for k, v := range tool {
					copyTool[k] = v
				}
				copyTool["name"] = exportName
				flat = append(flat, copyTool)
			} else {
				flat = append(flat, tool)
			}
			return nil
		}
		// Namespace children often omit type and use Responses' flat shape.
		// Keep the existing function converter responsible for validation.
		copyTool := make(map[string]any, len(tool)+2)
		for k, v := range tool {
			copyTool[k] = v
		}
		copyTool["type"] = "function"
		copyTool["name"] = exportName
		flat = append(flat, copyTool)
		return nil
	}
	for _, tool := range raw {
		if err := collect(tool, 0, ""); err != nil {
			return nil, nil, err
		}
	}
	return flat, namespaces, nil
}

// resolveNamespacedName accepts the three names observed from models:
// bare, namespace__tool, and namespace::tool.
func resolveNamespacedName(name string, namespaces map[string]string) (string, string) {
	if name == "" {
		return "", ""
	}
	if ns, ok := namespaces[name]; ok {
		return name, ns
	}
	if i := strings.Index(name, "::"); i > 0 {
		tail, head := name[i+2:], name[:i]
		if ns, ok := namespaces[tail]; ok {
			return tail, ns
		}
		if ns, ok := namespaces[name]; ok {
			return tail, ns
		}
		return tail, head
	}
	bestNS, bestBare := "", ""
	for _, ns := range namespaces {
		prefix := ns + "__"
		if ns != "" && strings.HasPrefix(name, prefix) && len(ns) > len(bestNS) {
			bestNS, bestBare = ns, strings.TrimPrefix(name, prefix)
		}
	}
	if bestNS != "" {
		return bestBare, bestNS
	}
	return name, ""
}

// lookupToolName resolves a provider-returned tool name to its bare name and
// namespace. It accepts bare, namespace__tool, and namespace::tool forms.
func lookupToolName(name string, meta ResponsesMeta) (string, string) {
	return resolveNamespacedName(name, meta.Namespaces)
}

// toolIsCustom reports whether a provider-returned name refers to a custom
// tool, including namespaced calls such as ns__apply_patch.
func toolIsCustom(name string, meta ResponsesMeta) bool {
	if meta.CustomTools[name] {
		return true
	}
	bare, _ := lookupToolName(name, meta)
	return bare != "" && meta.CustomTools[bare]
}

func stampNamespace(item map[string]any, namespaces map[string]string) {
	if len(namespaces) == 0 || item == nil {
		return
	}
	if s := stringValue(item["namespace"]); s != "" {
		return
	}
	bare, ns := resolveNamespacedName(stringValue(item["name"]), namespaces)
	if ns != "" {
		item["name"] = bare
		item["namespace"] = ns
	}
}

// flattenContent converts Responses content parts into a Chat Completions
// content value: plain text when possible, structured parts for images.
func flattenContent(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	parts, ok := v.([]any)
	if !ok {
		return fmt.Sprint(v)
	}
	var texts []string
	var chatParts []any
	hasImage := false
	for _, raw := range parts {
		if s, ok := raw.(string); ok {
			texts = append(texts, s)
			chatParts = append(chatParts, map[string]any{"type": "text", "text": s})
			continue
		}
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.ToLower(stringValue(part["type"]))
		switch {
		case typ == "input_text" || typ == "output_text" || typ == "text" || typ == "summary_text":
			s := stringValue(part["text"])
			texts = append(texts, s)
			chatParts = append(chatParts, map[string]any{"type": "text", "text": s})
		case typ == "input_image" || typ == "image_url" || typ == "image":
			url := imageURL(part)
			if url != "" {
				hasImage = true
				chatParts = append(chatParts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		}
	}
	if hasImage {
		return chatParts
	}
	return strings.Join(texts, "\n")
}

func imageURL(part map[string]any) string {
	for _, key := range []string{"image_url", "url"} {
		switch v := part[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			if s := stringValue(v["url"]); s != "" {
				return s
			}
		}
	}
	if data := stringValue(part["data"]); data != "" {
		mime := stringValue(part["mime_type"])
		if mime == "" {
			mime = stringValue(part["mimeType"])
		}
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + data
	}
	return ""
}

func contentText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			if m, ok := raw.(map[string]any); ok {
				if s := stringValue(m["text"]); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(c)
	}
}

// ChatToResponse folds one Chat Completions object into a Responses object.
func ChatToResponse(body []byte, meta ResponsesMeta) ([]byte, error) {
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
	text := contentText(msg["content"])
	reasoning := stringValue(msg["reasoning_content"])
	finish := stringValue(choice["finish_reason"])
	if finish == "" {
		finish = "stop"
	}
	output := make([]any, 0, 4)
	if reasoning != "" {
		output = append(output, map[string]any{
			"id": newID("rs_"), "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		})
	}
	for _, call := range chatMessageToolCalls(msg) {
		name := stringValue(call["name"])
		if name == "" {
			continue
		}
		args := stringValue(call["arguments"])
		if args == "" {
			args = "{}"
		}
		callID := stringValue(call["id"])
		if callID == "" {
			callID = newID("call_")
		}
		if toolIsCustom(name, meta) {
			item := map[string]any{
				"id": newID("ctc_"), "type": "custom_tool_call", "status": "completed",
				"call_id": callID, "name": name, "input": unwrapCustomInput(args),
			}
			stampNamespace(item, meta.Namespaces)
			output = append(output, item)
		} else {
			item := map[string]any{
				"id": newID("fc_"), "type": "function_call", "status": "completed",
				"call_id": callID, "name": name, "arguments": args,
			}
			stampNamespace(item, meta.Namespaces)
			output = append(output, item)
		}
	}
	if text != "" {
		output = append(output, map[string]any{
			"id": newID("msg_"), "type": "message", "status": "completed",
			"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	if len(output) == 0 {
		return nil, &ConvertError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned an empty assistant message"}
	}
	status := "completed"
	var incomplete any
	if finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	resp := map[string]any{
		"id": newID("resp_"), "object": "response", "created_at": time.Now().Unix(),
		"status": status, "model": meta.Model, "output": output, "output_text": text,
		"metadata": map[string]any{},
		"tools":    meta.Tools, "tool_choice": meta.ToolChoice, "parallel_tool_calls": defaultParallel(meta),
	}
	if incomplete != nil {
		resp["incomplete_details"] = incomplete
	}
	if u := responsesUsage(chat["usage"]); u != nil {
		resp["usage"] = u
	}
	return json.Marshal(resp)
}

func responseToolsValue(tools []any) []any {
	if tools == nil {
		return []any{}
	}
	return tools
}

func responseToolChoice(v any) any {
	if v == nil {
		return "auto"
	}
	return v
}

func defaultParallel(meta ResponsesMeta) bool {
	if b, ok := boolValue(meta.ParallelToolCalls); ok {
		return b
	}
	return true
}

func unwrapCustomInput(args string) string {
	var parsed any
	if json.Unmarshal([]byte(args), &parsed) != nil {
		return args
	}
	if m, ok := parsed.(map[string]any); ok {
		if s, ok := m["input"].(string); ok {
			return s
		}
		if v, ok := m["input"]; ok {
			b, _ := json.Marshal(v)
			return string(b)
		}
	}
	if s, ok := parsed.(string); ok {
		return s
	}
	return args
}

func responsesUsage(raw any) any {
	u, ok := raw.(map[string]any)
	if !ok || len(u) == 0 {
		return nil
	}
	prompt, _ := intValue(u["prompt_tokens"])
	completion, _ := intValue(u["completion_tokens"])
	total, _ := intValue(u["total_tokens"])
	details, _ := u["completion_tokens_details"].(map[string]any)
	pdetails, _ := u["prompt_tokens_details"].(map[string]any)
	cached, _ := intValue(firstNonNil(u["prompt_cache_hit_tokens"], pdetails["cached_tokens"]))
	reasoning, _ := intValue(details["reasoning_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	return map[string]any{
		"input_tokens": prompt, "input_tokens_details": map[string]any{"cached_tokens": cached},
		"output_tokens": completion, "output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens": total,
	}
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}

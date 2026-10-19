// Package protocol translates OpenAI Responses and Anthropic Messages requests
// onto the gateway's Chat Completions pipeline. The translation layer is
// deliberately free of pool/rotation logic: callers reuse the existing chat
// handler for accounting, auth, cooldowns and upstream error classification.
package protocol

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ConvertError describes a client-visible protocol conversion failure.
type ConvertError struct {
	Status  int
	Code    string
	Message string
}

func (e *ConvertError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

func badRequest(code, format string, args ...any) *ConvertError {
	return &ConvertError{Status: http.StatusBadRequest, Code: code, Message: fmt.Sprintf(format, args...)}
}

func decodeObject(body []byte) (map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, badRequest("invalid_json", "request body must be a JSON object: %v", err)
	}
	if obj == nil {
		return nil, badRequest("invalid_json", "request body must be a JSON object")
	}
	return obj, nil
}

func stringValue(v any) string {
	return strings.TrimSpace(rawStringValue(v))
}

// rawStringValue preserves payload bytes. Text, reasoning and tool arguments
// may arrive as whitespace-only deltas or contain meaningful indentation.
// stringValue is reserved for identifiers and other normalized metadata.
func rawStringValue(v any) string {
	s, _ := v.(string)
	return s
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case json.Number:
		return x.String()
	default:
		return strings.TrimSpace(fmt.Sprint(x))
	}
}

func boolValue(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// ErrorDetail is the provider-neutral view of one upstream error body. It
// keeps the original decoded object as well as the request id, because the chat
// stage wraps upstream JSON into error.message but clients still need the
// provider's requestId and extError fields when reporting failures upstream.
type ErrorDetail struct {
	Message   string
	Code      string
	Type      string
	RequestID string
	// Original is the most specific error object found in the body. It is the
	// top-level provider envelope when no error object exists, or the nested
	// upstream object when error.message itself contains JSON.
	Original map[string]any
}

// ExtractError parses an OpenAI-compatible error body while preserving the
// provider's request id and original error object.
func ExtractError(body []byte) ErrorDetail {
	var detail ErrorDetail
	var outer map[string]any
	if json.Unmarshal(body, &outer) != nil || outer == nil {
		detail.Message = strings.TrimSpace(string(body))
		if detail.Message == "" {
			detail.Message = "upstream request failed"
		}
		return detail
	}

	src := outer
	detail.Original = outer
	if rawErr, exists := outer["error"]; exists {
		switch e := rawErr.(type) {
		case map[string]any:
			if len(e) > 0 {
				src = e
				detail.Original = e
			}
		case string:
			if nested := decodeErrorMap(e); nested != nil {
				src = nested
				detail.Original = nested
			} else if strings.TrimSpace(e) != "" {
				detail.Message = strings.TrimSpace(e)
			}
		}
	}

	detail.Message = firstErrorString(src["message"], src["msg"])
	detail.Code = scalarString(src["code"])
	detail.Type = stringValue(src["type"])
	if detail.Message == "" {
		detail.Message = firstErrorString(outer["msg"], outer["message"])
	}
	if detail.Code == "" {
		detail.Code = scalarString(outer["code"])
	}
	if detail.Type == "" {
		detail.Type = stringValue(outer["type"])
	}
	detail.RequestID = firstErrorRequestID(outer, src)

	// The chat handler intentionally passes upstream error bodies through as
	// error.message. Look one level into that JSON so requestId/extError survive
	// even when the client-visible message must remain the raw provider text.
	if nested := decodeErrorMap(detail.Message); nested != nil {
		nsrc := nested
		if e, ok := nested["error"].(map[string]any); ok && len(e) > 0 {
			nsrc = e
		}
		if detail.RequestID == "" {
			detail.RequestID = firstErrorRequestID(nested, nsrc)
		}
		if detail.Code == "" {
			detail.Code = scalarString(nsrc["code"])
		}
		if detail.Type == "" {
			detail.Type = stringValue(nsrc["type"])
		}
		if len(nsrc) > 0 {
			detail.Original = nsrc
		}
	}

	if detail.Message == "" {
		detail.Message = strings.TrimSpace(string(body))
	}
	if detail.Message == "" {
		detail.Message = "upstream request failed"
	}
	return detail
}

// ExtractErrorMessage is the compatibility wrapper used by the streaming
// translators.
func ExtractErrorMessage(body []byte) (message, code string) {
	detail := ExtractError(body)
	return detail.Message, detail.Code
}

func decodeErrorMap(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" || (raw[0] != '{' && raw[0] != '[') {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil || obj == nil {
		return nil
	}
	return obj
}

func firstErrorString(values ...any) string {
	for _, v := range values {
		if s := stringValue(v); s != "" {
			return s
		}
	}
	return ""
}

func firstErrorRequestID(maps ...map[string]any) string {
	for _, m := range maps {
		if m == nil {
			continue
		}
		for _, key := range []string{"requestId", "request_id", "requestID"} {
			if s := stringValue(m[key]); s != "" {
				return s
			}
		}
		if e, ok := m["error"].(map[string]any); ok {
			for _, key := range []string{"requestId", "request_id", "requestID"} {
				if s := stringValue(e[key]); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// IsErrorChunk reports whether a Chat Completions chunk is an error frame.
// It mirrors the upstream package's two real shapes: a non-nil top-level
// error object, and a provider business envelope {code,msg} without choices.
func IsErrorChunk(payload []byte) bool {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return false
	}
	if v, ok := obj["error"]; ok && v != nil {
		if m, ok := v.(map[string]any); ok {
			return len(m) > 0
		}
		return true
	}
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		return false
	}
	if code, ok := obj["code"]; ok && code != nil {
		trimmed := strings.TrimSpace(fmt.Sprint(code))
		return trimmed != "" && trimmed != "0"
	}
	return false
}

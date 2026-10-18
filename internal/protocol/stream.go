package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sseEmitter writes protocol-native SSE events. Headers and the 200 status are
// committed only on the first actual event: an upstream 200 that carries only
// role/heartbeat/[DONE] frames must be reported as a real HTTP error instead of
// a 200 SSE stream ending in an error event.
type sseEmitter struct {
	w       http.ResponseWriter
	fl      http.Flusher
	started bool
}

func newSSEEmitter(w http.ResponseWriter) *sseEmitter {
	return &sseEmitter{w: w}
}

func (e *sseEmitter) start() {
	if e.started {
		return
	}
	e.started = true
	h := e.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	e.w.WriteHeader(http.StatusOK)
	e.fl, _ = e.w.(http.Flusher)
}

func (e *sseEmitter) event(name string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	e.start()
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", name, raw)
	if e.fl != nil {
		e.fl.Flush()
	}
}

func (e *sseEmitter) data(payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	e.start()
	_, _ = fmt.Fprintf(e.w, "data: %s\n\n", raw)
	if e.fl != nil {
		e.fl.Flush()
	}
}

// StreamError is a protocol stream failure that happened before any byte was
// committed to the client. The HTTP layer can render it as a native JSON error
// envelope; once content has been streamed, failures remain in-band SSE events.
type StreamError struct {
	Status  int
	Code    string
	Message string
	Detail  ErrorDetail
}

func (e *StreamError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// streamErrorStatus maps an upstream error frame to an HTTP status: rate-limit
// signals stay 429, everything else (including empty streams) is 502.
func streamErrorStatus(code, message string) int {
	lower := strings.ToLower(message)
	if code == "6004" || code == "rate_limit_exceeded" || strings.Contains(lower, "rate limit") || strings.Contains(lower, "频率限制") {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

// ResponsesStream translates Chat Completions chunks into Responses SSE events.
type ResponsesStream struct {
	em   *sseEmitter
	meta ResponsesMeta

	started bool
	failed  bool
	respID  string
	msgID   string
	rsID    string
	created int64
	seq     int

	reasoningParts []string
	reasonIndex    int
	reasonOpen     bool
	textParts      []string
	msgIndex       int
	msgOpen        bool

	tools    map[int]*responseTool
	toolSeq  []int
	lastTool int
	nextIdx  int

	usage      any
	finish     string
	sawUse     bool
	pendingErr *StreamError
}

type responseTool struct {
	index   int
	output  int
	itemID  string
	callID  string
	name    string
	args    strings.Builder
	pending strings.Builder
	opened  bool
}

// NewResponsesStream creates a stream translator. Finish must be called once.
func NewResponsesStream(w http.ResponseWriter, meta ResponsesMeta) *ResponsesStream {
	return &ResponsesStream{
		em: newSSEEmitter(w), meta: meta,
		respID: newID("resp_"), msgID: newID("msg_"), rsID: newID("rs_"),
		created: time.Now().Unix(), reasonIndex: -1, msgIndex: -1,
		tools: map[int]*responseTool{}, lastTool: -1,
	}
}

func (s *ResponsesStream) nextSeq() int { s.seq++; return s.seq }

func (s *ResponsesStream) emit(name string, payload map[string]any) {
	payload["type"] = name
	payload["sequence_number"] = s.nextSeq()
	s.em.event(name, payload)
}

// Feed consumes one Chat Completions SSE data payload.
func (s *ResponsesStream) Feed(payload []byte) {
	if s.failed {
		return
	}
	if IsErrorChunk(payload) {
		msg, code := ExtractErrorMessage(payload)
		if code == "" {
			code = "upstream_error"
		}
		s.failed = true
		if s.em.started {
			// Content already reached the client: keep the error in-band.
			s.em.event("error", map[string]any{"type": "error", "message": msg, "code": code})
			return
		}
		// Nothing has been committed yet: hold the error so Finish can let the
		// HTTP layer emit a real 502/429 JSON envelope.
		s.pendingErr = &StreamError{Status: streamErrorStatus(code, msg), Code: code, Message: msg, Detail: ExtractError(payload)}
		return
	}
	var chunk map[string]any
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		s.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return
	}
	if fr := stringValue(choice["finish_reason"]); fr != "" {
		s.finish = fr
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		delta, _ = choice["message"].(map[string]any)
	}
	if delta == nil {
		return
	}
	if r := stringValue(delta["reasoning_content"]); r != "" {
		s.sawUse = true
		s.openReasoning()
		s.reasoningParts = append(s.reasoningParts, r)
		s.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id": s.rsID, "output_index": s.reasonIndex, "summary_index": 0, "delta": r,
		})
	}
	if txt := stringValue(delta["content"]); txt != "" {
		s.sawUse = true
		s.openMessage()
		s.textParts = append(s.textParts, txt)
		s.emit("response.output_text.delta", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "delta": txt,
		})
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			s.sawUse = true
			s.feedToolCall(call)
		}
	}
	if fc, ok := delta["function_call"].(map[string]any); ok && stringValue(fc["name"]) != "" {
		s.sawUse = true
		s.feedToolCall(map[string]any{"index": float64(0), "function": fc})
	}
}

func (s *ResponsesStream) ensureStart() {
	if s.started {
		return
	}
	s.started = true
	s.em.start()
	s.emit("response.created", map[string]any{"response": s.responseObject("in_progress")})
	s.emit("response.in_progress", map[string]any{"response": s.responseObject("in_progress")})
}

func (s *ResponsesStream) openReasoning() {
	if s.reasonOpen {
		return
	}
	s.ensureStart()
	s.reasonOpen = true
	s.reasonIndex = s.nextIdx
	s.nextIdx++
	s.emit("response.output_item.added", map[string]any{
		"output_index": s.reasonIndex,
		"item":         map[string]any{"id": s.rsID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
	})
	s.emit("response.reasoning_summary_part.added", map[string]any{
		"item_id": s.rsID, "output_index": s.reasonIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (s *ResponsesStream) openMessage() {
	if s.msgOpen {
		return
	}
	s.ensureStart()
	s.msgOpen = true
	s.msgIndex = s.nextIdx
	s.nextIdx++
	s.emit("response.output_item.added", map[string]any{
		"output_index": s.msgIndex,
		"item":         map[string]any{"id": s.msgID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
	})
	s.emit("response.content_part.added", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (s *ResponsesStream) feedToolCall(call map[string]any) bool {
	usable := false
	idx := s.resolveToolIndex(call)
	entry := s.tools[idx]
	if entry == nil {
		entry = &responseTool{index: idx, itemID: newID("fc_"), output: -1}
		s.tools[idx] = entry
		s.toolSeq = append(s.toolSeq, idx)
	}
	if id := stringValue(call["id"]); id != "" {
		entry.callID = id
	}
	fn, _ := call["function"].(map[string]any)
	if name := stringValue(fn["name"]); name != "" {
		entry.name = name
		usable = true
	}
	args := stringValue(fn["arguments"])
	if args != "" {
		usable = true
		if entry.opened {
			entry.args.WriteString(args)
			if toolIsCustom(entry.name, s.meta) {
				s.emit("response.custom_tool_call_input.delta", map[string]any{
					"output_index": entry.output, "item_id": entry.itemID,
					"call_id": entry.callID, "delta": args,
				})
			} else {
				s.emit("response.function_call_arguments.delta", map[string]any{
					"output_index": entry.output, "item_id": entry.itemID,
					"call_id": entry.callID, "delta": args,
				})
			}
		} else {
			entry.pending.WriteString(args)
		}
	}
	if !entry.opened && entry.name != "" {
		s.openTool(entry)
	}
	return usable
}

// resolveToolIndex follows upstream.mergeToolCallsChunk: a missing index with
// a new id starts a new slot; a fragment with no id extends the most recent slot.
func (s *ResponsesStream) resolveToolIndex(call map[string]any) int {
	idx := -1
	if n, ok := intValue(call["index"]); ok {
		idx = int(n)
	} else if id := stringValue(call["id"]); id != "" {
		for i, entry := range s.tools {
			if entry.callID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = len(s.tools)
			for s.tools[idx] != nil {
				idx++
			}
		}
	} else if s.lastTool >= 0 {
		idx = s.lastTool
	} else {
		idx = 0
		for s.tools[idx] != nil {
			idx++
		}
	}
	s.lastTool = idx
	return idx
}

func (s *ResponsesStream) openTool(entry *responseTool) {
	if entry.opened {
		return
	}
	s.ensureStart()
	if entry.callID == "" {
		entry.callID = newID("call_")
	}
	custom := toolIsCustom(entry.name, s.meta)
	if custom {
		entry.itemID = newID("ctc_")
	}
	entry.opened = true
	entry.output = s.nextIdx
	s.nextIdx++
	item := map[string]any{"id": entry.itemID, "type": "function_call", "status": "in_progress", "call_id": entry.callID, "name": entry.name, "arguments": ""}
	if custom {
		item = map[string]any{"id": entry.itemID, "type": "custom_tool_call", "status": "in_progress", "call_id": entry.callID, "name": entry.name, "input": ""}
	}
	stampNamespace(item, s.meta.Namespaces)
	s.emit("response.output_item.added", map[string]any{"output_index": entry.output, "item": item})
	if pending := entry.pending.String(); pending != "" {
		entry.args.WriteString(pending)
		entry.pending.Reset()
		if custom {
			s.emit("response.custom_tool_call_input.delta", map[string]any{
				"output_index": entry.output, "item_id": entry.itemID, "call_id": entry.callID, "delta": pending,
			})
		} else {
			s.emit("response.function_call_arguments.delta", map[string]any{
				"output_index": entry.output, "item_id": entry.itemID, "call_id": entry.callID, "delta": pending,
			})
		}
	}
}

func (s *ResponsesStream) responseOutput() []any {
	type item struct {
		idx int
		v   any
	}
	var items []item
	if s.reasonOpen {
		items = append(items, item{s.reasonIndex, map[string]any{
			"id": s.rsID, "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": strings.Join(s.reasoningParts, "")}},
		}})
	}
	for _, idx := range s.toolSeq {
		entry := s.tools[idx]
		if !entry.opened {
			continue
		}
		args := entry.args.String()
		if toolIsCustom(entry.name, s.meta) {
			out := map[string]any{
				"id": entry.itemID, "type": "custom_tool_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "input": unwrapCustomInput(args),
			}
			stampNamespace(out, s.meta.Namespaces)
			items = append(items, item{entry.output, out})
		} else {
			if args == "" {
				args = "{}"
			}
			out := map[string]any{
				"id": entry.itemID, "type": "function_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "arguments": args,
			}
			stampNamespace(out, s.meta.Namespaces)
			items = append(items, item{entry.output, out})
		}
	}
	if s.msgOpen {
		content := []any{}
		if text := strings.Join(s.textParts, ""); text != "" {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		items = append(items, item{s.msgIndex, map[string]any{
			"id": s.msgID, "type": "message", "status": "completed", "role": "assistant", "content": content,
		}})
	}
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].idx < items[j-1].idx; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.v)
	}
	return out
}

func (s *ResponsesStream) responseObject(status string) map[string]any {
	obj := map[string]any{
		"id": s.respID, "object": "response", "created_at": s.created, "status": status,
		"model": s.meta.Model, "output": s.responseOutput(),
		"output_text": strings.Join(s.textParts, ""), "metadata": map[string]any{},
		"tools": responseToolsValue(s.meta.Tools), "tool_choice": responseToolChoice(s.meta.ToolChoice),
		"parallel_tool_calls": defaultParallel(s.meta),
	}
	if u := responsesUsage(s.usage); u != nil {
		obj["usage"] = u
	}
	if status == "incomplete" {
		obj["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return obj
}

// Finish closes the stream. Empty streams and errors that arrive before any
// content are returned as *StreamError without writing a 200 SSE header; the
// HTTP layer turns them into native JSON error responses.
func (s *ResponsesStream) Finish() error {
	if s.failed {
		return s.pendingErr
	}
	if !s.em.started {
		s.failed = true
		return &StreamError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned an empty stream"}
	}
	if s.reasonOpen {
		full := strings.Join(s.reasoningParts, "")
		s.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": s.rsID, "output_index": s.reasonIndex, "summary_index": 0, "text": full,
		})
		s.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": s.rsID, "output_index": s.reasonIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": full},
		})
		s.emit("response.output_item.done", map[string]any{
			"output_index": s.reasonIndex,
			"item": map[string]any{"id": s.rsID, "type": "reasoning", "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": full}}},
		})
	}
	for _, idx := range s.toolSeq {
		entry := s.tools[idx]
		if !entry.opened {
			if entry.name == "" {
				continue
			}
			s.openTool(entry)
		}
		args := entry.args.String()
		if toolIsCustom(entry.name, s.meta) {
			input := unwrapCustomInput(args)
			s.emit("response.custom_tool_call_input.done", map[string]any{
				"output_index": entry.output, "item_id": entry.itemID, "call_id": entry.callID, "input": input,
			})
			item := map[string]any{"id": entry.itemID, "type": "custom_tool_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "input": input}
			stampNamespace(item, s.meta.Namespaces)
			s.emit("response.output_item.done", map[string]any{"output_index": entry.output, "item": item})
		} else {
			if args == "" {
				args = "{}"
			}
			s.emit("response.function_call_arguments.done", map[string]any{
				"output_index": entry.output, "item_id": entry.itemID, "call_id": entry.callID, "arguments": args,
			})
			item := map[string]any{"id": entry.itemID, "type": "function_call", "status": "completed",
				"call_id": entry.callID, "name": entry.name, "arguments": args}
			stampNamespace(item, s.meta.Namespaces)
			s.emit("response.output_item.done", map[string]any{"output_index": entry.output, "item": item})
		}
	}
	if s.msgOpen {
		full := strings.Join(s.textParts, "")
		s.emit("response.output_text.done", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "text": full,
		})
		s.emit("response.content_part.done", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		})
		s.emit("response.output_item.done", map[string]any{
			"output_index": s.msgIndex,
			"item": map[string]any{"id": s.msgID, "type": "message", "status": "completed",
				"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}}}},
		})
	}
	status := "completed"
	if s.finish == "length" {
		status = "incomplete"
	}
	s.emit("response.completed", map[string]any{"response": s.responseObject(status)})
	return nil
}

// emitError renders a held failure as a native SSE error event. Used by the
// non-HTTP translation helper; HTTP handlers return a JSON envelope instead.
func (s *ResponsesStream) emitError(e *StreamError) {
	s.em.event("error", map[string]any{"type": "error", "code": e.Code, "message": e.Message})
}

// MessagesStream translates Chat Completions chunks into Anthropic SSE events.
type MessagesStream struct {
	em      *sseEmitter
	model   string
	started bool
	failed  bool
	id      string
	stop    string
	next    int

	thinkingOpen  bool
	thinkingIndex int
	textOpen      bool
	textIndex     int
	tools         map[int]*messagesTool
	toolSeq       []int
	lastTool      int
	usage         any
	sawUse        bool
	pendingErr    *StreamError
}

type messagesTool struct {
	index   int
	block   int
	id      string
	name    string
	args    strings.Builder
	pending strings.Builder
	opened  bool
}

// NewMessagesStream creates a Messages stream translator.
func NewMessagesStream(w http.ResponseWriter, model string) *MessagesStream {
	return &MessagesStream{em: newSSEEmitter(w), model: model, id: newID("msg_"), stop: "end_turn", textIndex: -1, thinkingIndex: -1, tools: map[int]*messagesTool{}}
}

func (s *MessagesStream) event(name string, payload map[string]any) {
	payload["type"] = name
	s.em.event(name, payload)
}

func (s *MessagesStream) ensureStart() {
	if s.started {
		return
	}
	s.started = true
	s.em.start()
	s.event("message_start", map[string]any{"message": map[string]any{
		"id": s.id, "type": "message", "role": "assistant", "content": []any{},
		"model": s.model, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}})
}

// Feed consumes one Chat Completions SSE data payload.
func (s *MessagesStream) Feed(payload []byte) {
	if s.failed {
		return
	}
	if IsErrorChunk(payload) {
		msg, code := ExtractErrorMessage(payload)
		s.failed = true
		status := streamErrorStatus(code, msg)
		if s.em.started {
			// Content already reached the client: keep the error in-band.
			s.event("error", map[string]any{"error": map[string]any{"type": anthropicErrorType(status), "message": msg}})
			return
		}
		// Nothing committed yet: Finish returns this so the HTTP layer can send
		// a native 502/429 JSON envelope.
		if code == "" {
			code = "upstream_error"
		}
		s.pendingErr = &StreamError{Status: status, Code: code, Message: msg, Detail: ExtractError(payload)}
		return
	}
	var chunk map[string]any
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		s.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return
	}
	if fr := stringValue(choice["finish_reason"]); fr != "" {
		s.stop = anthropicStopReason(fr)
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		delta, _ = choice["message"].(map[string]any)
	}
	if delta == nil {
		return
	}
	if r := stringValue(delta["reasoning_content"]); r != "" {
		s.sawUse = true
		s.ensureThinking()
		s.event("content_block_delta", map[string]any{
			"index": s.thinkingIndex, "delta": map[string]any{"type": "thinking_delta", "thinking": r},
		})
	}
	if txt := stringValue(delta["content"]); txt != "" {
		s.sawUse = true
		s.ensureText()
		s.event("content_block_delta", map[string]any{
			"index": s.textIndex, "delta": map[string]any{"type": "text_delta", "text": txt},
		})
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			s.sawUse = true
			s.feedToolCall(call)
		}
	}
	if fc, ok := delta["function_call"].(map[string]any); ok && stringValue(fc["name"]) != "" {
		s.sawUse = true
		s.feedToolCall(map[string]any{"index": float64(0), "function": fc})
	}
}

func (s *MessagesStream) ensureThinking() {
	if s.thinkingOpen {
		return
	}
	s.ensureStart()
	s.closeText()
	s.thinkingIndex = s.next
	s.next++
	s.thinkingOpen = true
	s.event("content_block_start", map[string]any{
		"index": s.thinkingIndex, "content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

func (s *MessagesStream) ensureText() {
	if s.textOpen {
		return
	}
	s.ensureStart()
	s.closeThinking()
	s.textIndex = s.next
	s.next++
	s.textOpen = true
	s.event("content_block_start", map[string]any{
		"index": s.textIndex, "content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *MessagesStream) closeThinking() {
	if s.thinkingOpen {
		s.thinkingOpen = false
		s.event("content_block_stop", map[string]any{"index": s.thinkingIndex})
	}
}

func (s *MessagesStream) closeText() {
	if s.textOpen {
		s.textOpen = false
		s.event("content_block_stop", map[string]any{"index": s.textIndex})
	}
}

func (s *MessagesStream) feedToolCall(call map[string]any) bool {
	usable := false
	idx := s.resolveToolIndex(call)
	entry := s.tools[idx]
	if entry == nil {
		entry = &messagesTool{index: idx, block: -1}
		s.tools[idx] = entry
		s.toolSeq = append(s.toolSeq, idx)
	}
	if id := stringValue(call["id"]); id != "" {
		entry.id = id
	}
	fn, _ := call["function"].(map[string]any)
	if name := stringValue(fn["name"]); name != "" {
		entry.name = name
		usable = true
	}
	args := stringValue(fn["arguments"])
	if args != "" {
		usable = true
		if entry.opened {
			entry.args.WriteString(args)
			s.event("content_block_delta", map[string]any{
				"index": entry.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
		} else {
			entry.pending.WriteString(args)
		}
	}
	if !entry.opened && entry.name != "" {
		s.openTool(entry)
	}
	return usable
}

func (s *MessagesStream) resolveToolIndex(call map[string]any) int {
	idx := -1
	if n, ok := intValue(call["index"]); ok {
		idx = int(n)
	} else if id := stringValue(call["id"]); id != "" {
		for i, entry := range s.tools {
			if entry.id == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			idx = len(s.tools)
			for s.tools[idx] != nil {
				idx++
			}
		}
	} else if s.lastTool >= 0 {
		idx = s.lastTool
	} else {
		idx = 0
		for s.tools[idx] != nil {
			idx++
		}
	}
	s.lastTool = idx
	return idx
}

func (s *MessagesStream) openTool(entry *messagesTool) {
	if entry.opened {
		return
	}
	s.ensureStart()
	s.closeThinking()
	s.closeText()
	entry.opened = true
	entry.block = s.next
	s.next++
	if entry.id == "" {
		entry.id = newID("toolu_")
	}
	s.event("content_block_start", map[string]any{
		"index":         entry.block,
		"content_block": map[string]any{"type": "tool_use", "id": entry.id, "name": entry.name, "input": map[string]any{}},
	})
	if pending := entry.pending.String(); pending != "" {
		entry.args.WriteString(pending)
		entry.pending.Reset()
		s.event("content_block_delta", map[string]any{
			"index": entry.block, "delta": map[string]any{"type": "input_json_delta", "partial_json": pending},
		})
	}
}

// Finish closes all content blocks and emits message_delta/message_stop.
// Empty streams and pre-content errors return *StreamError without committing a
// 200 SSE header; the HTTP layer renders a native JSON error envelope.
func (s *MessagesStream) Finish() error {
	if s.failed {
		return s.pendingErr
	}
	if !s.em.started {
		s.failed = true
		return &StreamError{Status: http.StatusBadGateway, Code: "upstream_parse", Message: "upstream returned an empty stream"}
	}
	s.closeThinking()
	s.closeText()
	for _, idx := range s.toolSeq {
		entry := s.tools[idx]
		if !entry.opened && entry.name != "" {
			s.openTool(entry)
		}
		if entry.opened {
			s.event("content_block_stop", map[string]any{"index": entry.block})
		}
	}
	s.event("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": s.stop, "stop_sequence": nil},
		"usage": anthropicUsage(s.usage),
	})
	s.event("message_stop", map[string]any{})
	return nil
}

// emitError renders a held failure as a native Anthropic SSE error event.
func (s *MessagesStream) emitError(e *StreamError) {
	s.event("error", map[string]any{"error": map[string]any{"type": anthropicErrorType(e.Status), "message": e.Message}})
}

// TranslateResponsesStream is a reader-based convenience wrapper used by tests
// and non-HTTP callers.
func TranslateResponsesStream(r *bytes.Reader, w http.ResponseWriter, meta ResponsesMeta) {
	stream := NewResponsesStream(w, meta)
	consumeChatSSE(r, stream.Feed)
	if err := stream.Finish(); err != nil {
		if se, ok := err.(*StreamError); ok {
			stream.emitError(se)
		}
	}
}

// TranslateMessagesStream is the Messages counterpart.
func TranslateMessagesStream(r *bytes.Reader, w http.ResponseWriter, model string) {
	stream := NewMessagesStream(w, model)
	consumeChatSSE(r, stream.Feed)
	if err := stream.Finish(); err != nil {
		if se, ok := err.(*StreamError); ok {
			stream.emitError(se)
		}
	}
}

func consumeChatSSE(r *bytes.Reader, feed func([]byte)) {
	data := make([]byte, r.Len())
	_, _ = io.ReadFull(r, data)
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		var line []byte
		if i < 0 {
			line, data = data, nil
		} else {
			line, data = data[:i], data[i+1:]
		}
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		feed(payload)
	}
}

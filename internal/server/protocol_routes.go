package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/protocol"
)

// responses serves POST /v1/responses by translating to Chat Completions and
// reusing chatCompletions end-to-end (auth, realm/model gates, rotation,
// cooldowns, usage and request logging are intentionally not duplicated).
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProtocolError(w, protoResponses, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, meta, err := protocol.ResponsesToChat(body)
	if err != nil {
		writeProtocolConvertError(w, protoResponses, err)
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(chatBody, &peek)
	if !h.checkProtocolModel(w, protoResponses, r, peek.Model) {
		return
	}
	next := r.Clone(r.Context())
	next.Body = io.NopCloser(bytes.NewReader(chatBody))
	next.ContentLength = int64(len(chatBody))
	if peek.Stream {
		pw := newProtocolStreamWriter(w, protoResponses, meta)
		h.chatCompletions(pw, next)
		pw.Finish()
		return
	}
	cap := &captureResponseWriter{header: http.Header{}}
	h.chatCompletions(cap, next)
	writeProtocolNonStream(w, protoResponses, cap.status, cap.header, cap.buf.Bytes(), meta)
}

// protocolInputModel resolves the model from either the protocol body or the
// chat body. It exists so realm/model gates run on the exact value that will
// be routed, including the realm: prefix.
func (h *Handler) checkProtocolModel(w http.ResponseWriter, proto protocolKind, r *http.Request, model string) bool {
	realm, bare := resolveModel(model)
	if id, ok := httpauth.IdentityFrom(r); ok {
		if id.Realm != "" && realm != id.Realm {
			writeProtocolError(w, proto, http.StatusBadRequest, "realm_not_allowed",
				"api key "+id.ID+" is restricted to realm "+id.Realm)
			return false
		}
		if !id.AllowsModel(bare) {
			writeProtocolError(w, proto, http.StatusBadRequest, "model_not_allowed",
				"api key "+id.ID+" is not allowed to use model "+bare)
			return false
		}
	}
	return true
}

// messages serves POST /v1/messages.
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProtocolError(w, protoMessages, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, err := protocol.MessagesToChat(body)
	if err != nil {
		writeProtocolConvertError(w, protoMessages, err)
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(chatBody, &peek)
	if !h.checkProtocolModel(w, protoMessages, r, peek.Model) {
		return
	}
	next := r.Clone(r.Context())
	next.Body = io.NopCloser(bytes.NewReader(chatBody))
	next.ContentLength = int64(len(chatBody))
	if peek.Stream {
		pw := newProtocolStreamWriter(w, protoMessages, peek.Model)
		h.chatCompletions(pw, next)
		pw.Finish()
		return
	}
	cap := &captureResponseWriter{header: http.Header{}}
	h.chatCompletions(cap, next)
	writeProtocolNonStream(w, protoMessages, cap.status, cap.header, cap.buf.Bytes(), peek.Model)
}

// countMessagesTokens answers POST /v1/messages/count_tokens locally. The
// response is an explicit estimate; no upstream (billable) request is made.
func (h *Handler) countMessagesTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeProtocolError(w, protoMessages, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		writeProtocolError(w, protoMessages, http.StatusBadRequest, "invalid_json", "request body must be a JSON object: "+err.Error())
		return
	}
	if !h.checkProtocolModel(w, protoMessages, r, obj.Model) {
		return
	}
	if _, err := protocol.MessagesToChat(body); err != nil {
		writeProtocolConvertError(w, protoMessages, err)
		return
	}
	n, err := protocol.CountMessagesTokens(body)
	if err != nil {
		writeProtocolConvertError(w, protoMessages, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Token-Count-Estimate", "true")
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": n,
		"estimate":     true,
		"note":         "local conservative estimate; no upstream tokenizer was called",
	})
}

// withProtocolAuth authenticates exactly like /v1/chat/completions but renders
// the native error envelope of the selected protocol.
func (h *Handler) withProtocolAuth(proto protocolKind, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Anthropic SDKs send x-api-key. Normalize it to the Bearer scheme the
		// gateway's single auth implementation understands; an explicit
		// Authorization header always wins.
		if r.Header.Get("Authorization") == "" {
			if key := strings.TrimSpace(r.Header.Get("x-api-key")); key != "" {
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer "+key)
			}
		}
		id, ok := h.authenticate(r)
		if !ok {
			writeProtocolError(w, proto, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		if id.ID != "" {
			r = r.WithContext(httpauth.WithIdentity(r.Context(), id))
		}
		next(w, r)
	}
}

type protocolKind int

const (
	protoResponses protocolKind = iota
	protoMessages
)

func writeProtocolError(w http.ResponseWriter, proto protocolKind, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if proto == protoMessages {
		_, _ = w.Write(protocol.AnthropicError(status, msg))
		return
	}
	_, _ = w.Write(protocol.OpenAIError(status, code, msg))
}

func writeProtocolConvertError(w http.ResponseWriter, proto protocolKind, err error) {
	if ce, ok := err.(*protocol.ConvertError); ok {
		writeProtocolError(w, proto, ce.Status, ce.Code, ce.Message)
		return
	}
	writeProtocolError(w, proto, http.StatusBadRequest, "invalid_request", err.Error())
}

func writeProtocolNonStream(w http.ResponseWriter, proto protocolKind, status int, header http.Header, body []byte, meta any) {
	if status == 0 {
		// The captured writer can legitimately end with no explicit status when
		// the chat stage wrote nothing; net/http's implicit status is 200.
		status = http.StatusOK
	}
	for k, vals := range header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if status >= 400 {
		detail := protocol.ExtractError(body)
		code := detail.Code
		// The chat stage may have wrapped an upstream error into its own
		// upstream_parse/upstream_error code while the original body is still
		// available; prefer the upstream's own code when present.
		if code == "" && strings.Contains(string(body), "upstream_parse") {
			code = "upstream_parse"
		}
		if code == "" {
			code = "upstream_error"
		}
		detail.Code = code
		w.Header().Set("Content-Type", "application/json")
		if proto == protoMessages {
			w.WriteHeader(status)
			_, _ = w.Write(protocol.AnthropicErrorDetail(status, detail))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(protocol.OpenAIErrorDetail(status, code, detail))
		return
	}
	var out []byte
	var err error
	if proto == protoMessages {
		out, err = protocol.ChatToMessages(body)
	} else {
		m, _ := meta.(protocol.ResponsesMeta)
		out, err = protocol.ChatToResponse(body, m)
	}
	if err != nil {
		writeProtocolConvertError(w, proto, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// captureResponseWriter buffers the non-streaming Chat Completions response so
// it can be folded into the protocol-native object without changing handlers.
type captureResponseWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (c *captureResponseWriter) Header() http.Header { return c.header }
func (c *captureResponseWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}
func (c *captureResponseWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(p)
}

// UpstreamMessage returns the upstream error body verbatim so protocol
// envelopes retain requestId/extError. ok is false for non-error statuses.
func (c *captureResponseWriter) UpstreamMessage() (string, bool) {
	if c.status < 400 {
		return "", false
	}
	return strings.TrimSpace(c.buf.String()), true
}

// protocolStreamWriter buffers Chat Completions SSE and feeds complete data
// frames into the protocol translator. Non-2xx JSON responses are buffered and
// converted at Finish; they are never emitted as a fake successful stream.
type protocolStreamWriter struct {
	dst   http.ResponseWriter
	proto protocolKind
	meta  any

	status    int
	mode      int // 0 undecided, 1 sse, 2 json
	buf       bytes.Buffer
	pending   []byte
	resp      *protocol.ResponsesStream
	msg       *protocol.MessagesStream
	rawWriter http.ResponseWriter
}

const (
	protocolModeUndecided = iota
	protocolModeSSE
	protocolModeJSON
)

// protocolPendingLimit bounds a single un-terminated SSE line. A stuck frame
// must not grow the writer's buffer without limit; 1 MiB is far above normal
// provider chunks while still failing fast on malformed/hostile streams.
const protocolPendingLimit = 1 << 20

func newProtocolStreamWriter(dst http.ResponseWriter, proto protocolKind, meta any) *protocolStreamWriter {
	return &protocolStreamWriter{dst: dst, proto: proto, meta: meta, rawWriter: &sseStageWriter{dst: dst}}
}

func (p *protocolStreamWriter) Header() http.Header { return p.dst.Header() }

// Unwrap keeps http.ResponseController able to reach Flusher/CloseNotifier on
// the real writer when the chat stage unwraps wrappers.
func (p *protocolStreamWriter) Unwrap() http.ResponseWriter { return p.dst }

func (p *protocolStreamWriter) WriteHeader(code int) {
	if p.status != 0 {
		return
	}
	p.status = code
}

func (p *protocolStreamWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	if p.status != http.StatusOK {
		p.mode = protocolModeJSON
		return p.buf.Write(b)
	}
	if p.mode == protocolModeUndecided {
		// Chat's non-stream path aggregates into JSON, its stream path forwards
		// SSE. Detect JSON by its first non-whitespace byte across writes. SSE
		// may begin with comments, event/id/retry fields or blank lines, so the
		// first line need not be data:.
		p.buf.Write(b)
		trimmed := bytes.TrimLeft(p.buf.Bytes(), " \t\r\n")
		if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
			p.mode = protocolModeJSON
			return len(b), nil
		}
		if len(trimmed) > 0 {
			p.startSSE()
			if err := p.consume(p.buf.Bytes()); err != nil {
				return 0, err
			}
			p.buf.Reset()
		} else if p.buf.Len() > protocolPendingLimit {
			return 0, fmt.Errorf("upstream stream line exceeds %d bytes", protocolPendingLimit)
		}
		return len(b), nil
	}
	if p.mode == protocolModeJSON {
		return p.buf.Write(b)
	}
	if err := p.consume(b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *protocolStreamWriter) Flush() {
	// Only forward Flush once the translator actually wrote a byte. Flushing an
	// untouched ResponseWriter commits the implicit 200 header, which would
	// turn a later native 502/429 JSON error into an invalid 200 response.
	if stage, ok := p.rawWriter.(*sseStageWriter); ok && !stage.wrote {
		return
	}
	if f, ok := p.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func (p *protocolStreamWriter) startSSE() {
	p.mode = protocolModeSSE
	if p.proto == protoMessages {
		model := ""
		if s, ok := p.meta.(string); ok {
			model = s
		}
		p.msg = protocol.NewMessagesStream(p.rawWriter, model)
	} else {
		meta, _ := p.meta.(protocol.ResponsesMeta)
		p.resp = protocol.NewResponsesStream(p.rawWriter, meta)
	}
}

// consume splits arbitrary Write chunks into SSE lines; only complete data:
// lines are forwarded, so a frame split across writes is not misparsed.
func (p *protocolStreamWriter) consume(b []byte) error {
	if len(p.pending) > protocolPendingLimit {
		return fmt.Errorf("upstream SSE frame exceeds %d bytes", protocolPendingLimit)
	}
	p.pending = append(p.pending, b...)
	for {
		i := bytes.IndexByte(p.pending, '\n')
		if i < 0 {
			if len(p.pending) > protocolPendingLimit {
				return fmt.Errorf("upstream SSE frame exceeds %d bytes", protocolPendingLimit)
			}
			return nil
		}
		line := bytes.TrimRight(p.pending[:i], "\r")
		p.pending = p.pending[i+1:]
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		if p.resp != nil {
			p.resp.Feed(payload)
		} else if p.msg != nil {
			p.msg.Feed(payload)
		}
	}
}

// Finish closes the translated stream or converts a buffered JSON error. A
// stream error that arrives before any SSE byte is committed is rendered as a
// native JSON error envelope, never as a 200 SSE stream ending in an error
// event (which SDKs report as a successful-but-empty response).
func (p *protocolStreamWriter) Finish() {
	if p.mode == protocolModeUndecided {
		if p.status != 0 && p.status != http.StatusOK {
			p.mode = protocolModeJSON
		} else if p.buf.Len() > 0 {
			p.mode = protocolModeJSON
		} else if p.status == http.StatusOK {
			p.startSSE()
		} else {
			writeProtocolError(p.dst, p.proto, http.StatusBadGateway, "upstream_parse", "upstream returned no response")
			return
		}
	}
	if p.mode == protocolModeJSON {
		// Headers were already set on dst by the chat stage; passing an empty
		// header map avoids Add-ing a map onto itself while ranging it.
		writeProtocolNonStream(p.dst, p.proto, p.status, http.Header{}, p.buf.Bytes(), p.meta)
		return
	}
	var err error
	if p.resp != nil {
		err = p.resp.Finish()
	}
	if p.msg != nil {
		err = p.msg.Finish()
	}
	if err != nil {
		writeProtocolStreamError(p.dst, p.proto, err)
	}
}

// writeProtocolStreamError renders a pre-commit protocol failure as the native
// JSON error envelope. protocol.StreamError carries the intended status/code;
// anything else is an upstream parse failure (502).
func writeProtocolStreamError(w http.ResponseWriter, proto protocolKind, err error) {
	var se *protocol.StreamError
	if errors.As(err, &se) {
		code := se.Code
		if code == "" {
			code = "upstream_error"
		}
		detail := se.Detail
		if detail.Message == "" {
			detail.Message = se.Message
		}
		detail.Code = code
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(se.Status)
		if proto == protoMessages {
			_, _ = w.Write(protocol.AnthropicErrorDetail(se.Status, detail))
			return
		}
		_, _ = w.Write(protocol.OpenAIErrorDetail(se.Status, code, detail))
		return
	}
	writeProtocolError(w, proto, http.StatusBadGateway, "upstream_parse", err.Error())
}

// sseStageWriter lets the protocol emitters own the final response bytes while
// the Chat Completions writer sees a normal http.ResponseWriter. wrote records
// whether the response is already committed, so Flush never commits it early.
type sseStageWriter struct {
	dst   http.ResponseWriter
	wrote bool
}

func (s *sseStageWriter) Header() http.Header  { return s.dst.Header() }
func (s *sseStageWriter) WriteHeader(code int) { s.wrote = true; s.dst.WriteHeader(code) }
func (s *sseStageWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.dst.Write(b)
}
func (s *sseStageWriter) Flush() {
	if !s.wrote {
		return
	}
	if f, ok := s.dst.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *sseStageWriter) Unwrap() http.ResponseWriter { return s.dst }

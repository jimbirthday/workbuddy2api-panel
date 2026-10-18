package server

import (
	"encoding/json"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// chatEnvelope 是 chat 请求入口的一次性解码视图。只在 handler 内使用，包含
// 路由/会话/工具/hint 判定需要的字段；原始 body 始终原样保留给上游，本结构
// 不参与出站改写。
type chatEnvelope struct {
	Stream              bool                    `json:"stream"`
	Model               string                  `json:"model"`
	Metadata            map[string]any          `json:"metadata"`
	ConversationID      string                  `json:"conversation_id"`
	ConversationIDCamel string                  `json:"conversationId"`
	PromptCacheKey      string                  `json:"prompt_cache_key"`
	UserID              string                  `json:"user_id"`
	Tools               []upstream.ToolDecl     `json:"tools"`
	Functions           []upstream.FunctionDecl `json:"functions"`
	Messages            []chatEnvelopeMessage   `json:"messages"`

	// keyMessages 是 session 包直接消费的视图，在解码后一次性投影，避免会话键、
	// 轮级键各重建一遍切片。Content 复用 RawMessage，零复制。
	keyMessages []session.KeyMessage
}

type chatEnvelopeMessage struct {
	Role         string                     `json:"role"`
	Content      json.RawMessage            `json:"content"`
	ToolCalls    []upstream.ToolCallDecl    `json:"tool_calls"`
	FunctionCall *upstream.FunctionCallDecl `json:"function_call"`
}

// prepare 在解码后填充一次性派生视图。只在正常结构化路径调用；解析失败时
// handler 走旧解析器，不依赖这里。
func (e *chatEnvelope) prepare() {
	e.keyMessages = make([]session.KeyMessage, 0, len(e.Messages))
	for _, m := range e.Messages {
		e.keyMessages = append(e.keyMessages, session.KeyMessage{Role: m.Role, Content: m.Content})
	}
}

func (e chatEnvelope) peek() struct {
	Stream bool
	Model  string
} {
	return struct {
		Stream bool
		Model  string
	}{Stream: e.Stream, Model: e.Model}
}

func (e chatEnvelope) keyFields() session.KeyFields {
	return session.KeyFields{
		ConversationID:      e.ConversationID,
		ConversationIDCamel: e.ConversationIDCamel,
		PromptCacheKey:      e.PromptCacheKey,
		UserID:              e.UserID,
		Metadata:            e.Metadata,
		Messages:            e.keyMessages,
	}
}

func (e chatEnvelope) convFields() session.ConversationIDFields {
	return session.ConversationIDFields{
		ConversationID:      e.ConversationID,
		ConversationIDCamel: e.ConversationIDCamel,
		Metadata:            e.Metadata,
	}
}

func (e chatEnvelope) turnMessages() []session.KeyMessage { return e.keyMessages }

func (e chatEnvelope) toolDecls() []upstream.ToolDecl         { return e.Tools }
func (e chatEnvelope) functionDecls() []upstream.FunctionDecl { return e.Functions }

func (e chatEnvelope) messageToolNames() []upstream.MessageToolNames {
	totalCalls := 0
	for _, m := range e.Messages {
		totalCalls += len(m.ToolCalls)
	}
	out := make([]upstream.MessageToolNames, 0, len(e.Messages))
	calls := make([]upstream.ToolCallDecl, totalCalls)
	next := 0
	for _, m := range e.Messages {
		item := upstream.MessageToolNames{FunctionCall: m.FunctionCall}
		if len(m.ToolCalls) > 0 {
			item.ToolCalls = calls[next : next+len(m.ToolCalls)]
			next += len(m.ToolCalls)
			for i, c := range m.ToolCalls {
				item.ToolCalls[i].Function = c.Function
			}
		}
		out = append(out, item)
	}
	return out
}

func (e chatEnvelope) hasImage() bool {
	for _, m := range e.Messages {
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

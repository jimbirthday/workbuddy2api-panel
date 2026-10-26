// rawchat.go 上游「原始转发」chat 方法：请求体与响应均不经过网关任何改写。
//
// 与 ChatStreamContext 的本质区别：那条链路负责 OpenAI 兼容化（effort 规范化、
// prompt_cache_key 注入、会话头族、模型名重写、响应聚合/白名单重建、usage 别名
// 归一），而本方法只为「透传端点」服务——body 逐字节原样出站，响应（状态码 +
// 响应头 + body）逐字节原样回传，绝不读 body 做错误分类、绝不改写出站字节。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// RawChat 把 body 原样 POST 到 chat 上游并返回**未读的** *http.Response。
// 调用方负责 resp.Body.Close() 与把响应逐字节透传给下游。
//
// 出站头仍走 ChatHeaders（Authorization / X-User-Id / 会话头族等账号与指纹声明
// 一个不少）——透传端点透传的是**内容**，账号凭证与上游风控指纹仍需网关补全，
// 否则上游根本无法受理。body 不做 prepareBody：不注入 prompt_cache_key、不做
// effort/role 规范化、不重写 model、不加 global system 兜底——客户端发什么
// 上游就收到什么。
//
// 错误语义：transport 层失败返回 (nil, err)；上游返回 ≥400 时**不读 body、不
// 分类**，直接把 resp 交回调用方透传——这是透传端点与主链路的唯一分歧点。
func (c *Client) RawChat(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	paths := c.chatPaths(a)
	if len(paths) == 0 {
		panic("unreachable: chatPaths is never empty")
	}
	endpoint := c.chatBase(a) + paths[0]
	// 透传链路的唯一改写：强制 stream=true（见 ensureStream 注释）。非流式必被
	// 上游 400 拒绝，这是「让请求能通过」的最低限度修正，与主链路同口径。
	body = ensureStream(body)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.ChatHeaders(req, a, clientIP, meta)
	req = req.WithContext(ctx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		// 传输层失败：清空共享连接池空闲连接（与 ChatStreamContext 同口径），
		// 避免下一个请求继续捡到坏连接。
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, err
	}
	return resp, nil
}

// ensureStream 透传链路的唯一改写：强制 stream=true。
//
// 上游（CN /v2 与 global /console 同源）拒绝非流式请求，返回
// "Non-stream chat request is currently not supported"（code 11101）。
// 透传 key 本应「客户端发什么上游就收到什么」，但非流式必然 400——
// 这里只做「让请求能通过」的最低限度修正，与主链路 payload.go 的
// normalizeBodyObject obj["stream"]=true 同口径。
//
// 关键：已是流式的请求（stream==true）直接原样返回，不做 marshal——
// 保证真正透传的请求一个字节都不变；只有缺 stream / stream=false 才改写。
// 解析失败 / 非 JSON 对象 body 原样返回，绝不二次错误化。
func ensureStream(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if s, ok := obj["stream"].(bool); ok && s {
		return body
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

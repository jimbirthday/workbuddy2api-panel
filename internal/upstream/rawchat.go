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

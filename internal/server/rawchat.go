// rawchat.go 「完全透传」链路：由配置的透传 key（api_keys[].passthrough=true）
// 触发，挂在 POST /v1/chat/completions 入口上。
//
// 与主链路 chatCompletions 的本质区别：
//   - 请求体不做任何改写（不注入 prompt_cache_key、不规范化 effort/role、不重写
//     model、不加 system 兜底、不脱敏指纹），客户端发什么上游就收到什么；
//   - 响应不做任何转换（不聚合流、不重建白名单帧、不归一 usage 别名、不修复
//     DSML 标记、不分类错误），上游的状态码/响应头/body 逐字节透传给下游。
//
// 触发条件：命中的 api key 带 Passthrough=true。主链路在完成 key 的 realm/模型
// 白名单校验后、任何改写之前分流到这里——「透传」只放宽**内容转换**，不放宽
// 鉴权与账号路由。
//
// 保留的网关职责（与「透传内容」不冲突）：
//   - Bearer 鉴权与 key 的 realm/模型白名单校验（安全边界，不可绕过）；
//   - 账号池选号与在途名额占用（多账号网关的本职）；
//   - 账号健康记账（transport 失败罚号、成功记健康）。
//
// 不做：会话粘性、请求轮转换号、降级重试、用量/reqlog 记账——透传链路是一次
// 性「拿上游原始内容」的观测通道，不该复用主链路的复杂语义。
package server

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rawChatBody 处理透传 key 的 chat 请求：选号后把 body 原样转发上游，响应原样
// 透传。入参 body/realm/bareModel/clientIP 已在主链路入口解析好，避免二次解码。
func (h *Handler) rawChatBody(w http.ResponseWriter, r *http.Request, body []byte, realm, bareModel, clientIP string) {
	start := time.Now()

	acct := h.cfg.Pool.PickExcludingForRealm(nil, bareModel, realm)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"no healthy account available for model "+bareModel)
		return
	}
	if !h.cfg.Pool.Acquire(acct.UID) {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"no in-flight capacity for account")
		return
	}
	defer h.cfg.Pool.Release(acct.UID)

	resp, err := h.cfg.Upstream.RawChat(r.Context(), acct, body, clientIP, upstream.ChatMeta{})
	if err != nil {
		// transport 层失败：与主链路同口径记一次账号失败，让池子尽快把坏号换走。
		h.cfg.Pool.NoteFailures(acct.UID)
		logx.Warnf("[server] rawchat acct=%s model=%s: upstream transport error: %v",
			logfmt.Label(acct.UID, acct.Nickname), bareModel, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_transport", err.Error())
		return
	}
	defer resp.Body.Close()

	// 成功记账：上游已受理（含 4xx/5xx 业务响应——透传链路不分类，只记「请求送达
	// 且拿到响应」这一层健康）。4xx/5xx 是客户端/上游业务问题，不罚号。
	if resp.StatusCode < 500 {
		h.cfg.Pool.NoteSuccess(acct.UID)
	}
	logx.Infof("[server] rawchat acct=%s model=%s upstream_status=%d dur=%s body=%dB",
		logfmt.Label(acct.UID, acct.Nickname), bareModel, resp.StatusCode,
		time.Since(start).Round(time.Millisecond), len(body))

	// 响应完全透传：复制全部响应头（剔除 hop-by-hop 头），原样写状态码与 body。
	// 剔除集合 = 静态 hop-by-hop 名单 + 上游 Connection 头点名的额外逐跳头
	// （RFC 7230 §6.1：Connection 列出的字段也视为 hop-by-hop，代理必须剥掉）。
	hop := hopByHopSet(resp.Header)
	dst := w.Header()
	for k, vv := range resp.Header {
		if hop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// 客户端已断开/写失败：内容已无法送达，静默（日志仅 debug 级，避免噪音）。
		logx.Debugf("[server] rawchat acct=%s: copy upstream body: %v", logfmt.UID8(acct.UID), err)
	}
}

// hopByHopSet 构造需要从透传响应里剔除的逐跳头集合：
// 静态名单（RFC 7230 §6.1）+ 上游 Connection 头点名的额外字段。
// 返回 map 以头名的小写为键，O(1) 判定；名字来自 net/http 已规范化的
// 头部键（首字母大写），但统一小写比较最稳。
func hopByHopSet(h http.Header) map[string]bool {
	hop := map[string]bool{}
	for _, name := range []string{
		"connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailers", "transfer-encoding", "upgrade",
	} {
		hop[name] = true
	}
	// 上游 Connection 头可能逗号分隔多个字段名（RFC 7230 §6.1）。
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if name := strings.TrimSpace(tok); name != "" {
				hop[strings.ToLower(name)] = true
			}
		}
	}
	return hop
}

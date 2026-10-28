// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// 白名单头组的 X-IDE-Version）。对齐本地 WorkBuddy Desktop 5.9.3（Info.plist 与真实出站 UA 一致）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.9.3"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐本地 WorkBuddy 5.9.3 内置 CLI（2.165.0）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.165.0"

	originRefererCN     = "https://www.workbuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.workbuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 applicationName 同值 `WorkBuddy`，
// global 用官方国际版 productName `WorkBuddy AI`（intl 项目逆向证据：
// `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`）。
// global 账号送错平台段（`WorkBuddy` 非 `WorkBuddy AI`）可能触发上游 403 code 11140
// "request illegal" 风控。官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + c.clientVersion() + " " + platform + "/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `WorkBuddy AI` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA() string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
// accountStableIDCache 缓存 deriveAccountStableID 的结果。派生对 (uid, purpose)
// 恒定，出站每请求（含换号重试跳）都要算 2 次 sha256 + hex；缓存后只做一次查表。
var accountStableIDCache sync.Map // key: uid+"\x00"+purpose -> string

func deriveAccountStableID(uid, purpose string) string {
	key := uid + "\x00" + purpose
	if v, ok := accountStableIDCache.Load(key); ok {
		return v.(string)
	}
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	v := hex.EncodeToString(sum[:18]) // 36 hex chars
	actual, _ := accountStableIDCache.LoadOrStore(key, v)
	return actual.(string)
}

// injectAccountStableHeaders 在 req 注入 X-Machine-ID / X-Session-ID：按 uid 稳定
// 派生，跨重启固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// resetRequestHeaders 清空请求头，保证出站请求只包含 WorkBuddy 标准头。
// 任何调用方在 ChatHeaders/CommonHeaders/BillingHeaders 之前设置的头（例如
// 从入站请求克隆来的 OpenAI/Anthropic 平台头）都会被丢弃，避免把其他平台的
// 请求头透传到上游触发风控拦截。
func (c *Client) resetRequestHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header = make(http.Header)
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	c.resetRequestHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（去掉宽松的 text/plain, */*）。
	// chat 路径在 ChatHeaders 自行设置 Accept（对齐 VLM 标准），不复用本函数。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；chat 出站按 VLM 标准自行生成 X-Trace-ID（TraceID 字段保留但不再透传）。
type ChatMeta struct {
	ConversationID string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	// PromptCacheConversationID 是图片请求派生出的会话源。非空时 ChatStreamContext
	// 会把 X-Conversation-ID 与 prompt_cache_key 一起切到该值（上游按会话头绑定
	// 图片状态，只改缓存键仍会 11135）；无图/空则两者都回落 ConversationID。
	PromptCacheConversationID string
	ConversationRequestID     string // X-Conversation-Request-ID：对话轮级聚合主键，必发
	// TraceID 保留字段：当前 chat 出站按 WorkBuddy VLM 标准自行生成 X-Trace-ID，
	// 不再透传入站 X-Trace-ID（保留字段以避免调用方/协议层编译断裂）。
	TraceID string
}

// ChatHeaders 设置 chat 出站请求头，对齐 WorkBuddy 桌面端 VLM 网关
// (VlmUpstream.send) 的真实出站头族：
//   - 基础：Accept/Content-Type=application/json、X-Requested-With、x-codebuddy-request、
//     Accept-Encoding=identity、User-Agent（WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>）
//   - 鉴权与身份：Authorization、X-User-Id、X-Enterprise-Id+X-Tenant-Id、X-Domain（有则发，无则不发）
//   - 用量归属：X-Agent-Intent/Purpose、X-IDE-*、X-Private-Data、X-Product
//   - 会话/链路：X-Conversation-*、acp-connection-id、X-Request-ID、traceparent/b3/X-B3-*、X-Trace-ID
//
// 不再发送任何 OpenAI/Anthropic 平台头，也不发送 X-No-* / Origin / Referer /
// Accept-Language / X-Machine-ID / X-Session-ID 等非 VLM 标准头。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.resetRequestHeaders(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", c.chatUserAgent())
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if eid := a.EnterpriseIDValue(); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 用量归属头：WorkBuddy 标准指纹（X-Agent-Intent/Purpose + X-IDE-* + X-Private-Data）。
	c.injectAttribution(req)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 会话头族（对话/请求/消息/B3 链路），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
}

// chatUserAgent 返回 chat 出站 UA：`WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`。
// WorkBuddy VLM 网关 UA 硬编码平台段为 WorkBuddy（不做 realm 切换），因此 chat 路径
// 不走 CommonHeaders 的按 realm UA；显式 config user_agent 仍可覆盖。
func (c *Client) chatUserAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return "WorkBuddy/" + c.clientVersion() + " WorkBuddy/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// newRequestUUID 生成带连字符的随机 UUID v4，对齐 WorkBuddy VLM 中 randomUUID()。
// crypto/rand 失败（理论上不可能）时回落 session.NewMessageID() 的 32 hex 格式化。
func newRequestUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // variant 10
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	h := session.NewMessageID()
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// randomHex 生成 n 字节的随机 hex（2*n 字符）。crypto/rand 失败时回落
// session.NewMessageID() 的前 2*n 字符。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	return session.NewMessageID()[:2*n]
}

// injectConversationHeaders 注入 WorkBuddy VLM 会话/链路头族。
//   - X-Conversation-ID：会话级（body 提取的 conversationId）。空则不发——透传优先，
//     不伪造，避免上游按会话头建错会话。
//   - X-Conversation-Request-ID：对话轮级聚合主键，必发（轮转循环外生成复用）。
//   - acp-connection-id：会话级连接 ID（随机 UUID，官方 VLM 每个 session 稳定）。
//   - X-Conversation-Message-ID / X-Request-ID：消息/请求级，各自独立随机 UUID。
//   - traceparent / b3 / X-B3-* / X-Trace-ID：与官方 applyTraceHeaders 一致——
//     traceID（32 hex）、spanID（16 hex）、parentSpanID（16 hex）各自随机生成。
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发。
		convReqID = session.NewMessageID()
	}
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("acp-connection-id", newRequestUUID())
	req.Header.Set("X-Conversation-Message-ID", newRequestUUID())
	req.Header.Set("X-Request-ID", newRequestUUID())

	traceID := session.NewMessageID() // 32 hex
	spanID := randomHex(8)            // 16 hex
	parentSpanID := randomHex(8)      // 16 hex
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("X-B3-TraceId", traceID)
	req.Header.Set("X-B3-SpanId", spanID)
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	req.Header.Set("X-Trace-ID", traceID)
}

// attributionClientName 生效的用量归属名（billing UA 用）：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// attributionProduct chat 出站的 X-Product 取值：WorkBuddy VLM 网关按部署形态
// 上报，本地客户端为 SaaS。为与真实出站一比一对齐，这里恒为 SaaS（不再用
// client_name 覆盖 X-Product；client_name 只影响 billing UA 的旧行为兼容）。
func (c *Client) attributionProduct() string {
	return "SaaS"
}

// injectAttribution 注入 WorkBuddy 标准用量归属头（chat 路径专用）。
// 对齐本地 WorkBuddy 桌面端出站：X-Agent-Intent/Purpose、X-IDE-Name/Type=WorkBuddy、
// X-IDE-Version=client_version、X-Private-Data=true、X-Product=SaaS。
func (c *Client) injectAttribution(req *http.Request) {
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Private-Data", "true")
	req.Header.Set("X-Product", c.attributionProduct())
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	c.resetRequestHeaders(req)
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if eid := a.EnterpriseIDValue(); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if eid := a.EnterpriseIDValue(); eid != "" {
		req.Header.Set("X-Enterprise-Id", eid)
		req.Header.Set("X-Tenant-Id", eid)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

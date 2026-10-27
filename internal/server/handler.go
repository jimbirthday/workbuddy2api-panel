// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	// APIKeys 静态 keyring（仅 Live 为 nil 的裸用/测试路径使用）。Live 快照
	// 中的 Keys 是生产路径权威值。
	APIKeys   []httpauth.Key
	MaxRotate int // 单请求最多换号次数，默认 3。限流换号不受此上限约束：池里还有未限流的号就继续打。
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool

	// DebugLog 请求级脱敏抓包器（可选；nil = 关）。开启时只在错误/修复相关
	// 挂载点落盘，不抓正常出站正文。
	DebugLog *DebugLog
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:           h.cfg.APIKey,
		Keys:             h.cfg.APIKeys,
		SoftCooldown:     h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withProtocolAuth(protoResponses, h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withProtocolAuth(protoMessages, h.messages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withProtocolAuth(protoMessages, h.countMessagesTokens))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost && isTracedChatPath(r.URL.Path) {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: r.URL.Path}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// isTracedChatPath reports which API entrypoints share the chat request
// accounting pipeline. count_tokens is excluded: it never touches an account
// and has no token/attempt observations to record.
func isTracedChatPath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/v1/responses", "/v1/messages":
		return true
	}
	return false
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.authenticate(r)
		if !ok {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		if id.ID != "" {
			r = r.WithContext(httpauth.WithIdentity(r.Context(), id))
		}
		next(w, r)
	}
}

// authenticate 校验请求并返回命中身份。livecfg 快照优先；快照没有 Keys 时
// 回落静态 APIKey（兼容旧装配路径）。未启用鉴权时返回空身份 + true。
func (h *Handler) authenticate(r *http.Request) (httpauth.Identity, bool) {
	live := h.loadLive()
	if len(live.Keys) > 0 {
		return httpauth.VerifyAny(r, live.Keys)
	}
	if strings.TrimSpace(live.APIKey) == "" {
		return httpauth.Identity{}, true
	}
	return httpauth.VerifyBearerKey(r, httpauth.Key{ID: httpauth.DefaultLegacyKeyID, Key: live.APIKey, Enabled: true})
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, realmServable := h.cfg.Pool.HealthSummary()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !realmServable["cn"] && !realmServable["global"] {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	// 管理面需要完整账号列表；业务 Key 只拿聚合计数与模型锁摘要。先判定可见性，
	// 让一次池快照按需决定是否构建 Status 行，而不是先算完整列表再丢弃。
	isClientKey := h.isClientRequest(r)
	snap := h.cfg.Pool.StatusSnapshot(!isClientKey)
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	status := map[string]any{
		"total":          snap.Total,
		"healthy":        snap.Healthy,
		"cooling":        snap.Cooling,
		"disabled":       snap.Disabled,
		"in_flight_full": snap.InFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     realmCountsMap(snap.Realms["cn"]),
			"global": realmCountsMap(snap.Realms["global"]),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单：账号池视图回答
		// 「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。零回归只增键。
		"model_locks": snap.ModelLocks,
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	}
	if !isClientKey {
		status["accounts"] = snap.Accounts
	} else {
		// 业务 Key 的可见性口径：不回传 accounts 键（连空数组都不给，避免
		// 客户端把“空池”误读成没有账号）；model_locks 只保留模型与恢复时间。
		status["accounts_visible"] = false
		status["model_locks"] = publicModelLocks(snap.ModelLocks)
	}
	writeJSON(w, http.StatusOK, status)
}

// realmCountsMap 把一次快照中的单域计数转成既有 /status JSON 形状。
func realmCountsMap(rc pool.RealmCounts) map[string]int {
	return map[string]int{
		"total":          rc.Total,
		"healthy":        rc.Healthy,
		"cooling":        rc.Cooling,
		"disabled":       rc.Disabled,
		"in_flight_full": rc.InFlightFull,
	}
}

// isClientRequest 判断当前请求是否只能看到业务面字段。管理面身份以顶层
// api_key 为准：legacy 条目本身只是兼容包装，不能仅凭 IdentityFrom 命中就
// 认定其为客户端 Key，否则管理密钥会被误降级而看不到账号池详情。
// 未启用鉴权（无身份）时保持旧的单机自用行为。
func (h *Handler) isClientRequest(r *http.Request) bool {
	if _, ok := httpauth.IdentityFrom(r); !ok {
		return false
	}
	if key := strings.TrimSpace(h.loadLive().APIKey); key != "" && httpauth.VerifyBearer(r, key) {
		return false
	}
	return true
}

// publicModelLocks 把模型级锁摘要降到业务 Key 也能安全看到的字段：模型名、
// realm、锁定的账号数量与恢复时间。完整明细可能含账号身份与内部原因字符串，
// 不对客户端暴露。
func publicModelLocks(rows []pool.ModelLockRow) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"model":        row.Model,
			"realm":        row.Realm,
			"state":        row.State,
			"servable":     row.Servable,
			"locked":       row.Locked,
			"unlock_at":    row.UnlockAt,
			"fully_unlock": row.FullyUnlockAt,
		})
	}
	return out
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
	// assembled 是 modelList 组装好的基础列表缓存（与 ids 同 TTL）。/v1/models
	// 会被客户端周期轮询，组装涉及 ContextWindow/MaxOutput/Effort 三级查找与
	// 大量 map 分配；缓存后每次轮询只做浅拷贝 + 按 key 过滤。
	// assembledAt 记录 assembled 所依据的 fetched 时刻：ids 与 fetched 总是
	// 一起被替换（生产 fetchDynamicModels / 测试 setup），两者不一致即重建，
	// 避免陈旧名单。
	assembled   []map[string]any
	assembledAt time.Time
}

const (
	// dynamicModelsTTL 模型目录缓存时长。曾是 1h；缩到 10min 对齐「面板实时、
	// API 缓存」的漂移痛点（PR #38 报告）：目录新增模型时面板立即可见，公开
	// /v1/models 最多滞后一个 TTL。再短就不值得——每次失效都是 2 次上游探测。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 10min），失败/无号返回空列表（无静态兜底——
// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	list := h.modelList()
	// realm/models 受限的 key 只看到自己可用的模型：realm 过滤前缀，models
	// 白名单按裸名匹配（显示值仍是带前缀的完整 id）。这样客户端不会先选到
	// 必然 400 的模型；限制只在请求前过滤，不改变上游模型目录本身。
	if id, ok := httpauth.IdentityFrom(r); ok {
		filtered := make([]map[string]any, 0, len(list))
		for _, entry := range list {
			full, _ := entry["id"].(string)
			realm, bare := resolveModel(full)
			if id.Realm != "" && realm != id.Realm {
				continue
			}
			if !id.AllowsModel(bare) {
				continue
			}
			filtered = append(filtered, entry)
		}
		list = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   list,
	})
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底。
func (h *Handler) modelList() []map[string]any {
	// 组装结果随动态目录同 TTL 缓存：轮询 /v1/models 时省去每模型的三级查找
	// 与 map 分配。缓存只在 fetch 成功后刷新；失败时目录不变、缓存继续可用。
	dynamicModelsCache.RLock()
	fresh := len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL
	if fresh && dynamicModelsCache.assembled != nil && dynamicModelsCache.assembledAt.Equal(dynamicModelsCache.fetched) {
		base := dynamicModelsCache.assembled
		out := make([]map[string]any, len(base))
		copy(out, base)
		dynamicModelsCache.RUnlock()
		return out
	}
	dynamicModelsCache.RUnlock()

	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
		// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
		// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
		// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 纯动态探测结果（fetchGlobalModels，失败/无号 → 空）。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（与 CN 动态分支同口径）。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	// 缓存组装结果：与动态目录同一 TTL；key 过滤在 models() 里对副本做，不污染缓存。
	dynamicModelsCache.Lock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		dynamicModelsCache.assembled = out
		dynamicModelsCache.assembledAt = dynamicModelsCache.fetched
	}
	dynamicModelsCache.Unlock()
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 缓存/失败回落封在 upstream.FetchGlobalModels（内部 1h + 5min 负缓存）。
// 本方法只负责"何时探测"：池中无 global 账号 → 空名单 + nil 账号（零上游调用）。
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从第一个可用 CN 账号拉模型列表（含 contextWindow/maxTokens），
// 缓存 10min。
// 选号与 /panel/api/models 完全同口径（AvailableUIDsForRealm("cn") 首个 + AuthByUID），
// 而非 Pool.Pick()：Pick 无 realm 过滤，混合池里可能选中 global 号去打 CN 端点，
// 表现为偶发失败/面板与 /v1/models 两套目录（PR #38 报告并给出的选号修复）。
// 缓存 + 5min 负缓存按既有语义**保留**（#38 原案整体删除缓存被拒）：公开端点逐请求
// 实时拉取 = 每次 2 个上游探测，客户端周期性刷新模型列表会持续打上游；上游故障时
// 无冷却窗口，客户端重试即放大请求量——负缓存正是为此设计（见 handler_test 吸收
// 上游 9832283 的注释）；且 cachedModelsSnapshot（gateway_hint 判定）依赖缓存写入。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError：NoteError 喂的是 chat
		// 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道健康的账号；
		// models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（hint 判定用：错误路径加一次 FetchModels 网络调用既拖慢
// 错误响应、又污染上游调用语义）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体无大小上限（max_body_mb 已移除，对齐上游）：完整读入，超限类问题交由
	// 上游自然返回错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断
	// 防御语义保留在读错误路径——移除预拦截后，截断只可能来自客户端自己断流，
	// 读 body 出错就地 400，不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	// 一次解析 + 多路复用：下面所有前置判定（model/stream、工具名单、会话键/
	// 轮级键、图片形态、conversationId）都来自同一次结构化解码。此前每个辅助
	// 函数各自 Unmarshal 一遍，64KB body 的前置链会多出约 5 次全量解码与千级分配。
	// 结构化解析失败时退回一次宽松 map 解析，保证畸形/边界 JSON 仍按原语义提取
	// 可读字段；两份都失败时各提取器返回空值，原始 body 照常透传给上游。
	var envelope chatEnvelope
	envelopeOK := json.Unmarshal(body, &envelope) == nil
	if !envelopeOK {
		// 解析失败完全回退旧解析器：各提取器对畸形 JSON 的宽容度并不相同
		//（例如一条 role 类型异常会让 TurnKey 整体失败），用 map 宽松恢复会改变
		// 路由键。异常请求不追求热点性能，优先保持逐字节旧语义。
		envelope = chatEnvelope{Stream: jsonBoolField(body, "stream"), Model: jsonStringField(body, "model")}
	}
	if envelopeOK {
		envelope.prepare()
	}
	peek := envelope.peek()

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」
	// 与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNamesFromFields(envelope.toolDecls(), envelope.functionDecls(), envelope.messageToolNames())
	if !envelopeOK {
		declaredTools = upstream.ToolNameAllowlist(body)
	}

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveModel(peek.Model)
	// 多 key 路由约束必须在读 body 之后、选号/上游调用之前生效：跨域请求明确
	// 400，不静默改走另一个域；models 白名单同样在请求前拒绝。这里只处理 chat；
	// /v1/models 的可见性过滤见 models handler。
	if keyID, ok := httpauth.IdentityFrom(r); ok {
		if keyID.Realm != "" && realm != keyID.Realm {
			writeOpenAIError(w, http.StatusBadRequest, "realm_not_allowed",
				"api key "+keyID.ID+" is restricted to realm "+keyID.Realm)
			return
		}
		if !keyID.AllowsModel(bareModel) {
			writeOpenAIError(w, http.StatusBadRequest, "model_not_allowed",
				"api key "+keyID.ID+" is not allowed to use model "+bareModel)
			return
		}
		// 透传 key：白名单校验通过后直接走完全透传链路（请求体原样出站、
		// 响应逐字节透传）。必须放在所有改写（后台模型重定向/会话粘性/
		// 轮转换号）之前，保证「透传」不掺任何网关转换。
		if keyID.Passthrough {
			h.rawChatBody(w, r, body, realm, bareModel, clientIP)
			return
		}
	}
	// 客户端后台任务模型重定向：必须在 Key 的 realm/模型白名单校验之后执行，
	// 否则受限 Key 可以用白名单内的别名跳到白名单外的付费模型。命中后同步改写
	// 路由变量与请求体，后续选号、冷却、粘性、用量都按实际目标模型记账。
	originalModel := peek.Model
	if live := h.loadLive(); live.BackgroundModelTarget != "" {
		if rw := backgroundModelAlias(originalModel, live.BackgroundModelNames, live.BackgroundModelTarget); !rw.Empty {
			targetRealm, targetBare := resolveModel(rw.Target)
			// 目标模型必须同样满足 Key 的 realm / 模型白名单。只在原始模型上
			// 做一次校验，会让受限 Key 用「白名单内的后台模型名」借别名访问
			// 白名单外的付费模型——这是安全边界，不是配置便利性问题。
			if keyID, ok := httpauth.IdentityFrom(r); ok {
				if keyID.Realm != "" && targetRealm != keyID.Realm {
					writeOpenAIError(w, http.StatusBadRequest, "realm_not_allowed",
						"api key "+keyID.ID+" background model target is restricted to realm "+keyID.Realm)
					return
				}
				if !keyID.AllowsModel(targetBare) {
					writeOpenAIError(w, http.StatusBadRequest, "model_not_allowed",
						"api key "+keyID.ID+" is not allowed to use background model target "+targetBare)
					return
				}
			}
			if next, ok := rewriteBackgroundModel(body, originalModel, rw.Target); ok {
				body = next
				peek.Model = rw.Target
				realm, bareModel = targetRealm, targetBare
				if envelopeOK {
					envelope.Model = rw.Target
				}
				logx.Infof("[server] background model alias: %s -> %s", originalModel, rw.Target)
			}
		}
	}
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	// 日志展示原始模型名（客户端实际请求的那个），否则「后台请求被改写成免费
	// 模型」这件事在请求流水/归档里完全不可见，用户也无从核对省了多少额度。
	st := newChatStatWithModel(time.Now(), originalModel, peek.Stream)
	if keyID, ok := httpauth.IdentityFrom(r); ok && keyID.ID != "" {
		st.keyID = keyID.ID
	}
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
		st.rid = tr.id
	}
	defer st.done()

	// debug 抓包：入站 body（脱敏后）只按需落盘，不抓正常出站正文。
	if h.cfg.DebugLog != nil && h.cfg.DebugLog.Enabled() {
		h.cfg.DebugLog.Capture(st.rid, "req_in", body)
	}

	tried := map[string]bool{}
	var lastErr error
	// lastRateLimited 为 true 时，下一跳不再捞冷却中的号（见选号处注释）。
	lastRateLimited := false
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	sessKey := session.ExtractKeyFromFields(envelope.keyFields())
	if !envelopeOK {
		sessKey = session.ExtractKey(body)
	}
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（对齐官方桌面 CLI
	// 的 X-Conversation-Request-ID 轮级语义——TraceStartHook 每次 USER_PROMPT_SUBMIT
	// 清空重生成），故不再限 sessKey=="" 才计算；sessKey 由下方派生处以复合键方式
	// 入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := session.TurnKeyFromFields(envelope.turnMessages())
	if !envelopeOK {
		turnKey = session.TurnKey(body)
	}

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := envelope.hasImage()
	if !envelopeOK {
		reqHasImage = hasImagePart(body)
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, ok bool, started time.Time, ttfb time.Duration) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 用量时序记录。ok 是本次尝试的协议结果，不能以「上游是否给了 usage」代替：
		// 上游成功但省略 usage 仍是成功，反之错误体里夹带 usage 也不能算成功。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := h.cfg.Pool.RealmByUID(uid)
			if realm == "" {
				realm = "cn"
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				HasCacheTokens:   st.hasCache,
				CacheHitTokens:   st.cacheHit,
				CacheMissTokens:  st.cacheMiss,
				ModelRate:        modelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
				KeyID:            st.keyID,
			}, ok)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	// ConversationID passes the client dialog id through. Clients that have none
	// (Anthropic / Claude Code traffic) fall back to the session key, otherwise
	// the upstream prompt_cache_key degrades into one constant per account and
	// every client on that account ends up sharing a single, constantly evicted
	// prefix-cache slot.
	convID := session.ResolveConversationIDFromFields(envelope.convFields())
	if !envelopeOK {
		convID = session.ResolveConversationID(body)
	}
	if convID == "" {
		convID = sessKey
	}
	// 图片请求按「图片集合指纹」派生 prompt_cache_key 的会话源：同一张图重试
	// 保持同一上游会话，换图/追加图自动进入新会话，避免上游 11135 把新图判成
	// 旧会话的图片状态冲突。图片请求由 upstream.ChatStreamContext 把会话头与
	// prompt_cache_key 一起切到派生值（上游按头绑定图片状态，只改缓存键不够）；
	// 无图片时两源一致，存量行为零漂移。
	// 图片指纹只算一次；具体派生会话必须在选号后按 acct.UID 生成（见下方
	// ImageSessionFor）。不能在这里用首轮选的账号做隔离段，否则限流换号后
	// 第二个账号仍会收到带第一个账号隔离段的会话头。
	imgFP := session.ImageFingerprintFromMessages(envelope.turnMessages())
	if !envelopeOK {
		imgFP = session.ImageFingerprint(body)
	}
	chatMeta := upstream.ChatMeta{ConversationID: convID}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞（#170 统一轮级）。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合，好于请求级随机。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// rotates 是已换号次数。限流（ErrSoftRate）且池里还有未冷却的号时不计入上限：
	// 账号很多时，一次正常请求不该在试完默认 3 个号后被 429 打断，让用户手动重试。
	// 其他失败仍受 MaxRotate 约束，避免传输错误把一次请求放大到全池。
	rotates := 0
	for rotates < h.cfg.MaxRotate {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑，
				// 本次回落普通轮换。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 限流之后只打还没冷却的号。Pick 在全冷却时会捞最早到期的冷却号，
			// 那一跳对用户就是一次必然的 429，这里直接停。
			if lastRateLimited && !h.poolHasRotatable(tried, bareModel, realm) {
				modelBlock = h.cfg.Pool.ModelBlocked(bareModel)
				break
			}
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			modelBlock = h.cfg.Pool.ModelBlocked(bareModel)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUIDForModel 往返（语义与 fail()/PickByUIDForModel-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(rotates, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			rotates++
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(rotates, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避）
				}
				rotates++
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				st.logf("error", "[server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 图片会话按本轮实际出站账号派生：上游按 X-Conversation-ID 绑定图片
		// 状态，同一会话换号重试时必须落到该账号自己的图片会话，不能把 A
		// 账号的图片上下文发给 B。无图请求 ResolveImageSession 返回空串，
		// chatMeta 保持原有透传语义（含上一轮从未带图的普通会话）。
		attemptMeta := chatMeta
		attemptMeta.PromptCacheConversationID = session.ResolveImageSession(convID, acct.UID, imgFP)

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, attemptMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，
			// 只会把客户端拖到 MaxRotate × header_timeout（部署值 600s 时最坏
			// 约半小时），期间还给一串健康号喂连败计数。此前全仓没有任何超时
			// 识别，超时和"网络抖动"共用同一条换号路径。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				st.logf("warn", "[server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), terr)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			// 客户端已断连（用户点了「停止生成」/客户端网络抖动）：这是客户端侧的问题，
			// 不是账号的问题——喂连败计数会让健康号 5 次后被临时踢出选号池 10 分钟，
			// 多号池表现成「可用号越来越少」。跳过 NoteFailures 与粘性解绑，直接止损。
			if r.Context().Err() != nil {
				st.logf("info", "[server] client gone acct=%s: %v (account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), terr)
				break
			}
			h.cfg.Pool.NoteFailures(acct.UID, bareModel)
			fail(acct.UID)
			if !rotateBackoff(rotates, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避）
			}
			rotates++
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				st.logf("info", "content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				if summary := session.ImageSummary(body); summary != "" {
					st.logf("warn", "[server] image rejected acct=%s model=%s conv=%q %s upstream=%s",
						logfmt.Label(acct.UID, acct.Nickname), bareModel, logfmt.Truncate(convID, 48),
						summary, logfmt.Truncate(string(respBody), 200))
				}
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 请求体解析失败（11101）：与 11115 / 图片无效同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到
			// 「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			if kind == upstream.ErrBadParams {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11148「tool calls and tool results do not match」：会话历史里的工具调用
			// 记录已损坏，上游明示「直接重试无效，请新建任务」。同一份坏历史换任何账号
			// 都是同样的 400，轮转只会把坏历史重放给每个健康号并最终误报 503。
			// 立即透传上游原文回 400（含 requestId），让客户端新建会话重试。
			if kind == upstream.ErrToolSequence {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "tool calls and tool results do not match, please start a new conversation"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "tool_sequence_broken", msg,
					h.hintOf(upstream.ErrToolSequence, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(rotates, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			// 限流族（429 及各类「账号/额度暂时不可用」的 429 形态）且还有未冷却的
			// 号：不消耗 MaxRotate。其余失败照常计数。
			if isRateLimitOutcome(kind, uerr) {
				lastRateLimited = true
				if h.poolHasRotatable(tried, bareModel, realm) {
					continue
				}
			}
			rotates++
			continue
		}
		// 防御：ChatStreamContext 对「≥400 但分类为 ErrNone」的异常信封会返回
		// (nil, status, raw, nil)。正常路径不会到这里，但上游/代理返回畸形
		// 状态码时不能假设 err==nil 就一定有 body，否则下面 stats/StreamHint
		// 会 nil-panic，整条请求链 500 且账号状态不确定。按上游空响应处理。
		if rc == nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			lastErr = &upstream.Error{Kind: upstream.ErrServer, Status: http.StatusBadGateway, Msg: "upstream returned an empty response"}
			st.logf("warn", "[server] upstream returned status %d with empty body acct=%s model=%s body=%s",
				status, logfmt.Label(acct.UID, acct.Nickname), bareModel, logfmt.Truncate(string(respBody), 200))
			break
		}
		// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
		// 记成健康、粘性把会话钉死在它身上，后续每一轮都打同一个限流号。
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			stats.setDebugCapture(st.rid, h.cfg.DebugLog)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
			var errFrame string
			// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
			// 这里在透传前还原成 delta.tool_calls（见 upstream/dsml.go）。
			// 第二个参数为 true = 名单为空时启用弱判定——tools 声明在链路上丢失是
			// 实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
			repair := upstream.NewMarkupRepair(declaredTools, true)
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				upstream.WithMarkupRepair(repair), upstream.HoldErrorUntilContent())
			// attemptOK 是本次尝试的协议结果，默认成功，但空流/错误帧/读上游失败都
			// 必须显式置 false：此前这里无条件传 true，导致错误流也计入 usage 成功数。
			// 客户端主动断连不算账号失败（人已走），保持 true 与账号健康语义一致。
			attemptOK := true
			// 正文写出前的 error 帧：客户端还没看到失败。可换号的限流在此换号，
			// 不把 429 帧交给用户。不可换号（池子没有别的号，或错误与账号无关）
			// 则把扣住的帧补写出去，语义与「直接透传」一致。
			if held, ok := sErr.(*upstream.ErrStreamHeld); ok {
				rc.Close()
				kind := upstream.FrameKind(held.Payload)
				st.errKind = kind.String()
				if h.cfg.DebugLog != nil {
					h.cfg.DebugLog.Capture(st.rid, "upstream_error", []byte(held.Payload))
				}
				h.applyErrorPolicy(acct.UID, kind, held.Payload, bareModel, nil)
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
				fail(acct.UID)
				// 内容拦截误报（passthrough/append 首遇）：与非流式分支（见 status>=400
				// 处）同一套降级重试——换 Degraded 中性提示词同请求内重试，避免流式
				// 客户端（多数 agent CLI 默认 stream:true）直接拿到 error 帧失败而非流式
				// 客户端却被自动救回的不对称。degradedApplied 守卫防重复降级。
				if kind == upstream.ErrContentBlocked && !degradedApplied &&
					(h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") {
					h.degrade.Trigger()
					body = prompt.Rewrite(body, prompt.Degraded)
					degradedApplied = true
					delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
					st.logf("warn", "[server] stream content-blocked before content -> degraded prompt retry")
					if !rotateBackoff(rotates, r.Context()) {
						break
					}
					rotates++
					continue
				}
				if isRateLimitKind(kind) && h.poolHasRotatable(tried, bareModel, realm) {
					lastRateLimited = true
					st.logf("warn", "[server] stream acct=%s model=%s: rate-limit frame before content kind=%s, rotating",
						logfmt.Label(acct.UID, acct.Nickname), bareModel, kind)
					if !rotateBackoff(rotates, r.Context()) {
						lastErr = &upstream.Error{Kind: kind, Status: http.StatusTooManyRequests, Msg: held.Payload}
						break
					}
					rotates++
					continue
				}
				st.logf("warn", "[server] stream acct=%s model=%s: error frame before content kind=%s (no rotate) payload=%s",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(held.Payload, 200))
				// 正文前且无可换账号：客户端一个字节都还没收到（StreamHint 现在
				// 延迟提交 200），返回真实状态码 + 上游原文，而不是 200 SSE error 帧
				// ——后者会被部分 SDK 当作「成功但空回答」。协议入口会在 Finish 时
				// 再翻成 native 错误信封（requestId/extError 随原文保留）。
				status, code := heldStreamStatus(kind)
				msg := strings.TrimSpace(held.Payload)
				if msg == "" {
					msg = "upstream request failed before any content"
				}
				writeOpenAIErrorHint(w, status, code, msg,
					h.hintOf(kind, held.Payload, bareModel, reqHasImage, nil))
				st.status = status
				st.outcome = reqlog.OutcomeStreamError
				return
			}
			// Protocol completion can itself write buffered tool input and final
			// events. Finalize before marking this attempt/account successful.
			if finisher, ok := w.(interface{ FinishStream(error) error }); ok && !upstream.IsEmptyStreamError(sErr) {
				finishErr := sErr
				if r.Context().Err() != nil {
					finishErr = r.Context().Err()
				}
				sErr = finisher.FinishStream(finishErr)
			}
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：StreamHint 现在一个字节都不写
				//（200 成功头延迟到首个正文帧），因此这里能返回真实 502 JSON，
				// 而不是先提交 200 再在流里塞 error 帧。与非流式 Aggregate 空流
				// →502 upstream_parse 同语义。协议入口经 protocolStreamWriter 会
				// 把它翻成 native error 信封。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				attemptOK = false
				writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", "upstream returned an empty stream")
				st.logf("warn", "[server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				st.errKind = kind.String()
				if h.cfg.DebugLog != nil {
					h.cfg.DebugLog.Capture(st.rid, "upstream_error", []byte(errFrame))
				}
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
				// 正文已经写出，不能换号；但本次尝试的租约必须释放，且若失败的正是
				// 粘性号，必须解绑——否则下一轮仍会被 ResolveForModel 钉在这个号上
				// （6004 只锁模型，账号对其他模型仍健康，粘性校验未必能挡住）。
				fail(acct.UID)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				attemptOK = false
				st.logf("warn", "[server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case upstream.IsStreamReadError(sErr):
				// 读上游失败（连接中断 / 空闲看门狗掐流 / 读超时）：**不是**客户端写失败。
				// 此前两类错误共用本分支，上游半途断流时账号被记成功、清 11102 负缓存、
				// 绑粘性，坏号长期留在池内且会话被钉在它身上。读侧失败按失败观测处理：
				// 不记成功、不清负缓存、不绑粘性；传输层已由上游 client 记日志。
				//
				// 但客户端已断连（用户点了「停止生成」/客户端网络抖动）时读上游会随之
				// 以 StreamReadError 收场——那是客户端侧的问题，不是账号的问题。与传输层
				// 抖动分支（见 r.Context().Err() 守卫）同口径：客户端在场才罚号，否则喂
				// 连败计数会让健康号 5 次后被临时踢出选号池，多号池表现成「可用号越来越少」。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				attemptOK = false
				if r.Context().Err() != nil {
					st.outcome = reqlog.OutcomeInterrupted
					// 客户端已断连不是账号失败：与下面的写失败分支一致，usage 不记错误。
					attemptOK = true
					st.logf("info", "[server] client gone during stream acct=%s: %v (account not penalized)",
						logfmt.Label(acct.UID, acct.Nickname), sErr)
				} else {
					h.cfg.Pool.NoteFailures(acct.UID, bareModel)
					st.logf("warn", "[server] stream acct=%s model=%s: upstream read error: %v",
						logfmt.Label(acct.UID, acct.Nickname), bareModel, sErr)
				}
			case sErr != nil:
				// 客户端写失败（断连）：账号侧**不洗白**——写失败发生在哪个时刻都可能，
				// 上游可能只发了一半甚至刚开流；没有完整读完就不能证明该号成功。
				// 与读侧「客户端已断连」分支同口径：不罚号、不记 usage 错误，但也绝不
				// NoteSuccess / 清 11102 / 绑粘性（否则一个限流号会在用户点停止时被
				// 记成健康，粘性把会话继续钉在它身上）。
				st.outcome = reqlog.OutcomeInterrupted
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				st.outcome = reqlog.OutcomeSuccess
				h.cfg.Pool.NoteSuccess(acct.UID)
				h.cfg.Pool.ClearModelDegrade(acct.UID, bareModel)
				// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
				// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
				// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
				if sessKey != "" && h.cfg.Session != nil {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
			}
			credit, hasCredit := stats.Credit()
			if hit, miss, ok := stats.CacheTokens(); ok {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptOK, attemptStarted, stats.TTFB())
			// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
			}
			if n := repair.Converted(); n > 0 {
				// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
				// 打一行便于运维确认「这次不再空转」以及统计发生率。
				st.logf("info", "[server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
				stats.flushDebugDSML()
			} else if m := repair.Seen(); m > 0 {
				// 识别到标记却没还原：说明判定没过（工具名不合规 / 块内夹带正文 /
				// 参数畸形）。单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
				st.logf("warn", "[server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if hasCredit {
				st.credit = credit
				st.hasCredit = true
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
				}
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		// 上游以 error 帧报错（HTTP 200 已开流 + 首帧 error，真实形态）：
		// 复用**流式**同一套分类（FrameKind 优先命中 6004 模型级限流 / 6020·6021
		// 模型队列忙）与冷却分支——模型级信号只锁该模型（CooldownSoftForModel /
		// CooldownModelQueue），账号对其他模型仍可选，**绝不**停整号调度；池里还有
		// 可换号则同样换号重试，与流式的 ErrStreamHeld 分支语义严格对齐。
		// 此前非流式把 error 帧当有效事件，返回 200 空内容假成功且不处置账号。
		if payload, isFrame := upstream.IsErrorFrameError(err); isFrame {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
			kind := upstream.FrameKind(payload)
			st.errKind = kind.String()
			if h.cfg.DebugLog != nil {
				h.cfg.DebugLog.Capture(st.rid, "upstream_error", []byte(payload))
			}
			h.applyErrorPolicy(acct.UID, kind, payload, bareModel, nil)
			releaseHeld()
			fail(acct.UID)
			// 内容拦截误报（passthrough/append 首遇）：与 status>=400 分支、流式
			// ErrStreamHeld 分支同一套降级重试——换 Degraded 中性提示词同请求内重试，
			// 避免非流式客户端拿到 error 帧直接失败、而流式客户端却被自动救回的不对称。
			// degradedApplied 守卫防重复降级。
			if kind == upstream.ErrContentBlocked && !degradedApplied &&
				(h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				st.logf("warn", "[server] non-stream content-blocked -> degraded prompt retry")
				if !rotateBackoff(rotates, r.Context()) {
					break
				}
				rotates++
				continue
			}
			if isRateLimitKind(kind) && h.poolHasRotatable(tried, bareModel, realm) {
				lastRateLimited = true
				st.logf("warn", "[server] non-stream acct=%s model=%s: error frame kind=%s before content, rotating",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind)
				if !rotateBackoff(rotates, r.Context()) {
					lastErr = &upstream.Error{Kind: kind, Status: http.StatusTooManyRequests, Msg: payload}
					break
				}
				rotates++
				continue
			}
			st.status = http.StatusServiceUnavailable
			st.outcome = reqlog.OutcomeHTTPError
			lastErr = &upstream.Error{Kind: kind, Status: http.StatusServiceUnavailable, Msg: payload}
			st.logf("warn", "[server] non-stream acct=%s model=%s: error frame kind=%s (no rotate) payload=%s",
				logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(payload, 200))
			break
		}
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, false, attemptStarted, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
		// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
		// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
		mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, true)
		if n := mrepair.Converted(); n > 0 {
			st.logf("info", "[server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
				bareModel, n, len(declaredTools))
			// debug 抓包：只落「这次真的修到了什么」的正文片段（脱敏后），不抓正常正文。
			if h.cfg.DebugLog != nil && n > 0 {
				if frag := dsmlRepairFragment(resp); len(frag) > 0 {
					h.cfg.DebugLog.Capture(st.rid, "dsml_repair", frag)
				}
			}
		} else if m := mrepair.Seen(); m > 0 {
			st.logf("warn", "[server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
				bareModel, m, len(declaredTools))
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		if usage, ok := resp["usage"].(map[string]any); ok {
			if hit, okH := upstream.UsageCacheHitTokens(usage); okH {
				miss, _ := upstream.UsageCacheMissTokens(usage)
				st.cacheHit, st.cacheMiss, st.hasCache = int64(hit), int64(miss), true
			}
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, true, attemptStarted, 0)
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.ClearModelDegrade(acct.UID, bareModel)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		st.errKind = ue.Kind.String()
		st.upstreamStatus = ue.Status
		st.errMsg = ue.Msg
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		// 上游给的是 429 时，无论分类到哪个 Kind（普通限流 / 积分耗尽 14018 /
		// trial 未激活 14017 / 账号风控 11140 / 会话失效 12153），对下游都保持
		// 可重试的 429 语义，绝不落回默认 503——「换不动号」只是本池没有可用号，
		// 不代表服务端过载。
		if ue.Status == http.StatusTooManyRequests {
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
		}
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、
				// 轮转已止损、窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；
				// 有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			code = "model_unavailable"
			status = http.StatusBadRequest
		case upstream.ErrContentBlocked:
			// 内容策略拦截（非流式降级重试后仍被拦，或 custom 模式直透）：内容问题非
			// 账号问题，轮转无意义、不该报 no_healthy_account（那是"号全挂了"的语义，
			// 客户端会不断重试）。与 status>=400 分支同口径回 400 content_blocked。
			code = "content_blocked"
			status = http.StatusBadRequest
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		st.modelBlocked = true
		if s := strings.TrimSpace(modelBlock.Reason); s != "" && st.errMsg == "" {
			st.errMsg = s
		}
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
	st.errCode = code
	if st.errMsg == "" {
		st.errMsg = msg
	}
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（issue #127）：ttfb 量的是「首个 SSE 帧
// 到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——首帧与
// 末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成
// 时长不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。
// 真流式下首帧到末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实
// 快速输出（如缓存命中后的爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
//
// 判据是「usage.credit 键存在且为数值」，不是 total_tokens>0：此前把两者绑在
// 一起，上游偶尔只回 credit 不回 total 时整笔积分观测被丢弃——用量页少记一次
// 扣减，「积分 / 1M Token」的分母也跟着缺样本。token 为 0 时返回 0，由调用方
// （NoteModelCost）自行跳过单价归一，不影响 credit 入账。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	raw, exists := usage["credit"]
	if !exists {
		return 0, 0, false
	}
	c, isNum := raw.(float64)
	if !isNum {
		// credit 可能是 null（上游没给）或字符串（不可信）：都不入账。
		return 0, 0, false
	}
	t, _ := usage["total_tokens"].(float64)
	if t < 0 {
		t = 0
	}
	return c, int(t), true
}

// poolHasRotatable 报告本 realm、本模型上是否还有未试过且未冷却的号。
// 限流换号用它决定要不要继续：有就继续，没有才把 429 交回客户端。
// 不能用 Pick 的返回值：全冷却时 Pick 会捞「最早到期」的冷却号再试一次，
// 那一跳几乎必然再 429，等于把限流错误交回用户。
func (h *Handler) poolHasRotatable(tried map[string]bool, model, realm string) bool {
	return h.cfg.Pool.HasRotatable(tried, model, realm)
}

// isRateLimitOutcome 报告本次上游结果是否属于「限流族」：应以可重试的 429 语义
// 换号继续，且换号不受 MaxRotate 上限约束。
//
// 只认两个权威信号，绝不把「限流族」扩大到非 429 状态码（402 余额耗尽 /
// 401 会话失效 / 403 账号风控各有其不可自愈语义，仍受 MaxRotate 约束）：
//   - ue.Status == 429：任何 429 都算（14018/14017/11140/12153 等被分到
//     ErrHardCredit/ErrAccountFault/ErrSessionDead 的形态一并覆盖）；
//   - kind == ErrSoftRate：保留既有语义——非 429 状态码（如 400）但 body 带
//     "rate limit" 文案时，Classify 会归 ErrSoftRate，同样免扣 MaxRotate。
func isRateLimitOutcome(kind upstream.ErrKind, ue *upstream.Error) bool {
	if ue != nil && ue.Status == http.StatusTooManyRequests {
		return true
	}
	return kind == upstream.ErrSoftRate
}

// isRateLimitKind 报告该分类是否属于「账号/额度暂时不可用」的限流族。用于
// HTTP 200 首帧 error（无状态码）的流式/非流式帧路径：ErrHardCredit 覆盖
// 14018 积分耗尽，ErrAccountFault 覆盖 14017/11140 request illegal，
// ErrSessionDead 覆盖 12153——这些都该换号继续，而非中断后给下游 502/503。
func isRateLimitKind(kind upstream.ErrKind) bool {
	switch kind {
	case upstream.ErrSoftRate, upstream.ErrHardCredit, upstream.ErrAccountFault, upstream.ErrSessionDead:
		return true
	}
	return false
}

// heldStreamStatus maps a pre-content upstream error frame to the real HTTP
// status/code returned when no other account can be rotated in. A model-level
// rate limit stays retryable 429; request-level failures are 400; everything
// else is an upstream fault (502).
func heldStreamStatus(kind upstream.ErrKind) (int, string) {
	switch kind {
	case upstream.ErrSoftRate:
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case upstream.ErrHardCredit, upstream.ErrAccountFault, upstream.ErrSessionDead:
		// 限流族：这些分类在「HTTP 200 首帧 error」形态下同样代表账号暂时不可用
		//（积分耗尽 14018 / trial 未激活 14017 / 账号风控 11140 / 会话失效 12153），
		// 下游应拿到可重试的 429，而不是 502 upstream_error。
		return http.StatusTooManyRequests, "rate_limit_exceeded"
	case upstream.ErrContentBlocked:
		return http.StatusBadRequest, "content_blocked"
	case upstream.ErrModelBlocked:
		return http.StatusBadRequest, "model_unavailable"
	case upstream.ErrPromptTooLong:
		return http.StatusBadRequest, "prompt_too_long"
	case upstream.ErrImageInvalid:
		return http.StatusBadRequest, "image_invalid"
	case upstream.ErrBadParams:
		return http.StatusBadRequest, "bad_params"
	case upstream.ErrToolSequence:
		return http.StatusBadRequest, "tool_sequence_broken"
	}
	return http.StatusBadGateway, "upstream_error"
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		logx.Warnf("[server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 6020（queue.waiting）/ 6021（queue.full）只锁触发模型，不冻整号。
//     其余优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、非 6004 走
//     账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信 Retry-After 头
//     （uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrBadParams → 不罚账号，且与 ErrPromptTooLong/ErrImageInvalid 同待遇：
//     调用方在轮转循环内即刻 400 透传原文终止（换号必然同样失败）。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 6020 queue.waiting / 6021 queue.full 都是这个模型的上游队列，不是账号配额。
		// 多个号会在同一秒一起撞上。只写 modelCooldowns[model]，不写 until、不推进
		// softStreak。时长优先用 body 里的 data.retry_after（秒）；没有再用 soft_rate
		// 基数。不采用 estimated_wait——那是排位预计等待，队列很长时会到数小时。
		// 冷却中不延长，封顶 soft_rate_max。切模型仍可选。
		// 没有模型名时退回下面的账号级路径，避免同一个未知请求立刻再打这号。
		if upstream.IsModelQueueFull(body) && model != "" {
			d := h.softCooldown()
			if ra, ok := upstream.ModelQueueRetryAfter(body); ok {
				d = ra
			}
			h.cfg.Pool.CooldownModelQueue(uid, model, d, upstream.ModelQueueReason(body))
			return
		}
		// 6004 = 模型级限流：账号健康，只是这个模型此刻被限。**只锁该模型、绝不冻整号**。
		// 用户硬约束：单模型限流绝不能停整个账号的调度。此前只有「带可解析重置时间」时
		// 才走模型级路径；无重置时间/仅有 Retry-After 头的 6004 会退回账号级
		// CooldownSoftRate，把整号冻住（其他模型也一起不可选），与约束相悖。
		modelRateLimited := upstream.IsModelRateLimit(body)
		if modelRateLimited && model != "" {
			// 6004 一律模型级：
			//   - 有「将在 … 重置」→ 对齐该墙钟（CooldownSoftForModel，切模型豁免）；
			//   - 另有 Retry-After 头 → 冷却到该时刻（模型级）；
			//   - 都没有 → 模型级有界退避（base 起、封顶 soft_rate_max）。
			// 三条路径都只写 modelCooldowns[model]，不写账号级 until、不推进账号 softStreak。
			if resetAt, ok := upstream.ParseRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			if uerr != nil && uerr.RetryAfter > 0 {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), model, "6004 model rate limit (retry-after)")
				return
			}
			h.cfg.Pool.CooldownSoftForModelNoReset(uid, model, h.softCooldown(), "6004 model rate limit (reset unknown)")
			return
		}
		// 非 6004（账号级限流，如 11140 rate-limiting）：对齐上游重置墙钟或 Retry-After
		// 头，绝不指数堆加；无时间的走账号级有界退避（soft_rate 基数起、softStreak 翻倍、
		// 封顶 soft_rate_max；已在冷却中的兜底探测不翻倍）。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			return
		}
		// 无模型名（model==""）的 6004：无法做模型级路由（RecordModelRateLimitAudit
		// 要求 model 非空，此处调用必然是空操作，故不记审计台账）。但不能退回账号级
		// CooldownSoftRate：那会冻结整号，违反「单模型限流不得停整个账号调度」。
		// 无模型上下文无法做模型级路由，保持账号健康，仅让本次请求透传/换号。
		if modelRateLimited {
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：基数
		// wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
		// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。
		// Retry-After 头优先（WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：硬禁用（Disable）。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关侧不再截断，均为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；chatCompletions
		// 已 fail-fast 400 透传原文、终止轮转——「换号可能有不同模型权限」属 11102
		// （ErrModelBlocked）的分类域，与本类无关。
	case upstream.ErrToolSequence:
		// 11148 工具调用记录不匹配：会话历史损坏，是请求（会话状态）的问题不是账号的
		// 问题。零动作（不冷却/不熔断/不 NoteError），chatCompletions 已 fail-fast
		// 400 透传原文、终止轮转——换任何账号重放同一份坏历史都必然同样 400。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid, model)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// jsonStringField / jsonBoolField 只用于结构化解析失败时的 model/stream
// 快速兜底；工具、会话与图片等仍由旧解析器各自处理，保持原宽容度。
func jsonStringField(body []byte, key string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal(obj[key], &v)
	return v
}

func jsonBoolField(body []byte, key string) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	var v bool
	_ = json.Unmarshal(obj[key], &v)
	return v
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 10min 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}

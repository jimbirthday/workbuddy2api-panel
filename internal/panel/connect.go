package panel

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// connectModel 是接入页暴露的模型条目（仅 id/name，不含凭证与内部路由信息）。
type connectModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// connectInfo 返回客户端接入所需的安全信息。
//
// 安全边界：这里永远不返回 api_key、access/refresh token、device token，
// 也不回显 URL userinfo。示例统一使用 $WB_API_KEY 占位符，即使请求方已通过
// 管理密钥鉴权也不把真实密钥写进响应体——管理密钥只用于面板 API 鉴权。
type connectInfo struct {
	OK                bool            `json:"ok"`
	BaseURL           string          `json:"base_url"`
	V1URL             string          `json:"v1_url"`
	AnthropicURL      string          `json:"anthropic_url"`
	AuthRequired      bool            `json:"auth_required"`
	PanelAuthRequired bool            `json:"panel_auth_required"`
	APIKeyPlaceholder string          `json:"api_key_placeholder"`
	Models            []connectModel  `json:"models"`
	ModelsSource      string          `json:"models_source"`
	Examples          connectExamples `json:"examples"`
	Notes             []string        `json:"notes"`
}

type connectExamples struct {
	OpenAISDK         string `json:"openai_sdk"`
	Curl              string `json:"curl"`
	AnthropicEnv      string `json:"anthropic_env"`
	AnthropicMessages string `json:"anthropic_messages"`
	ResponsesAPI      string `json:"responses_api"`
}

// connect 返回接入信息。base_url 由请求推导：反代/容器部署时 Host 可能不是
// 对外地址，因此同时给出 relative（/v1）示例，前端用 location.origin 拼出可见地址。
func (p *Panel) connect(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// 反代常用 X-Forwarded-Proto；只取首段并白名单 http/https，避免把任意头
	// 原样拼进示例（Host 同理只作为展示，不参与鉴权或上游调用）。
	if xf := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); xf == "https" || xf == "http" {
		scheme = xf
	}
	host := connectHost(r.Host)
	base := scheme + "://" + host
	v1 := base + "/v1"

	models, modelsSource := p.connectModels()
	modelID := "cn:glm-5.3"
	if len(models) > 0 && models[0].ID != "" {
		modelID = models[0].ID
	}

	writeJSON(w, http.StatusOK, connectInfo{
		OK:                true,
		BaseURL:           base,
		V1URL:             v1,
		AnthropicURL:      base,
		AuthRequired:      p.clientAuthRequired(),
		PanelAuthRequired: p.authRequired(),
		APIKeyPlaceholder: "$WB_API_KEY",
		Models:            models,
		ModelsSource:      modelsSource,
		Examples: connectExamples{
			OpenAISDK: "import os\nfrom openai import OpenAI\n\n" +
				"client = OpenAI(\n" +
				"    base_url=\"" + v1 + "\",\n" +
				"    api_key=os.environ[\"WB_API_KEY\"],\n" +
				")\n\n" +
				"resp = client.chat.completions.create(\n" +
				"    model=\"" + modelID + "\",\n" +
				"    messages=[{\"role\": \"user\", \"content\": \"你好\"}],\n" +
				")",
			Curl: "curl " + v1 + "/chat/completions \\\n" +
				"  -H \"Authorization: Bearer $WB_API_KEY\" \\\n" +
				"  -H \"Content-Type: application/json\" \\\n" +
				"  -d '{\"model\":\"" + modelID + "\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'",
			AnthropicEnv: "export ANTHROPIC_BASE_URL=\"" + base + "\"\n" +
				"export ANTHROPIC_AUTH_TOKEN=\"${WB_API_KEY:?set WB_API_KEY first}\"\n" +
				"# Claude Code 等 Anthropic Messages 客户端可直接使用；网关会把请求\n" +
				"# 转换为 OpenAI Chat Completions 后转发。",
			AnthropicMessages: "curl " + base + "/v1/messages \\\n" +
				"  -H \"x-api-key: $WB_API_KEY\" \\\n" +
				"  -H \"anthropic-version: 2023-06-01\" \\\n" +
				"  -H \"Content-Type: application/json\" \\\n" +
				"  -d '{\"model\":\"" + modelID + "\",\"max_tokens\":256,\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}'",
			ResponsesAPI: "curl " + v1 + "/responses \\\n" +
				"  -H \"Authorization: Bearer $WB_API_KEY\" \\\n" +
				"  -H \"Content-Type: application/json\" \\\n" +
				"  -d '{\"model\":\"" + modelID + "\",\"input\":\"你好\",\"max_output_tokens\":256}'",
		},
		Notes: []string{
			"面板不会在此页回显任何真实密钥；请从部署环境的 API Key 配置中取值并设置到 $WB_API_KEY。",
			"base_url 按当前浏览器访问地址推导；经反向代理时请以对外域名为准。",
			"模型 id 可带 cn: / global: 前缀选择账号域，也可使用 /v1/models 返回的裸名。",
			"支持 OpenAI Chat Completions、OpenAI Responses 与 Anthropic Messages 三种入口。",
			"模型列表来自本机观测（用量/成本台账）或内置建议，不触发上游探测；完整目录以 /v1/models 为准。",
		},
	})
}

// connectHost 清洗请求 Host 再用于接入示例。安全边界：
//   - 绝不接受 userinfo（会形成 https://user:pass@host 的展示/复制内容）；
//   - 拒绝空白、控制字符与路径/查询分隔符，避免把任意请求头原样拼进示例；
//   - 空值回落到本机地址，保证示例始终可读。
//
// 这里只做展示，不参与鉴权或上游调用；反代部署仍应以对外域名为准。
func connectHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "@/\\?#") {
		return "127.0.0.1"
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "127.0.0.1"
		}
	}
	return raw
}

// clientAuthRequired 表示 /v1/* 是否需要客户端 Key。运行时 keyring 非空
// （含多租户 api_keys[]）就要求鉴权；只有完全未配置时才匿名放行。
func (p *Panel) clientAuthRequired() bool {
	if p.cfg.Live != nil && len(p.cfg.Live.Load().Keys) > 0 {
		return true
	}
	return strings.TrimSpace(p.apiKey()) != ""
}

// connectModelsCacheTTL 是接入页模型列表的短缓存窗口。模型观测不会秒级变化，
// 但接入页可能被多个浏览器同时刷新；30s 足以合并重复构建，又不会让新模型
// 长时间不可见。缓存按面板实例隔离、有固定上限（见 connectModelCacheMax）。
const connectModelsCacheTTL = 30 * time.Second

// connectModelCacheMax 限制缓存保留的面板实例数。生产通常只有一个面板，
// 但测试/多租户嵌入可能创建多个 Panel；固定上限保证指针键不会无限增长。
const connectModelCacheMax = 16

type connectModelCache struct {
	mu     sync.Mutex
	at     time.Time
	models []connectModel
	source string
}

var connectModelCaches = struct {
	sync.Mutex
	byPanel map[*Panel]*connectModelCache
	order   []*Panel
}{byPanel: map[*Panel]*connectModelCache{}}

// connectModelCacheFor 返回面板实例对应的有界缓存槽。只有同一面板的首个
// 构建者会进入 buildConnectModels，其余并发请求在该槽的 mutex 上等待并复用
// 结果，即单飞语义。槽位按 FIFO 淘汰，避免测试/嵌入场景下内存无界。
func connectModelCacheFor(p *Panel) *connectModelCache {
	connectModelCaches.Lock()
	defer connectModelCaches.Unlock()
	if c := connectModelCaches.byPanel[p]; c != nil {
		return c
	}
	if len(connectModelCaches.order) >= connectModelCacheMax {
		oldest := connectModelCaches.order[0]
		connectModelCaches.order = connectModelCaches.order[1:]
		delete(connectModelCaches.byPanel, oldest)
	}
	c := &connectModelCache{}
	connectModelCaches.byPanel[p] = c
	connectModelCaches.order = append(connectModelCaches.order, p)
	return c
}

// connectModels 只读展示模型 id。优先使用本机观测到且带域前缀的模型
// （成本台账 / 今日用量）；没有任何观测时回退到少量静态建议，避免接入页因
// 一次上游探测阻塞。完整目录仍以 /v1/models 为准。
//
// 构建路径使用 Pool.ConnectModelRows（轻量投影），不再调用 Pool.List 完整
// Status 构建；结果按 connectModelsCacheTTL 缓存，并带单飞保护。
func (p *Panel) connectModels() ([]connectModel, string) {
	c := connectModelCacheFor(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.at.IsZero() && now.Sub(c.at) < connectModelsCacheTTL {
		return c.models, c.source
	}
	models, source := p.buildConnectModels(now)
	c.at, c.models, c.source = now, models, source
	return models, source
}

// buildConnectModels 是未缓存的构建主体，测试可直接调用固定 now 的第二入口。
func (p *Panel) buildConnectModels(now time.Time) ([]connectModel, string) {
	if p.cfg.Pool != nil {
		seen := map[string]bool{}
		out := make([]connectModel, 0)
		add := func(realm, model string) {
			model = strings.TrimSpace(model)
			if model == "" {
				return
			}
			id := model
			if realm == "cn" || realm == "global" {
				id = realm + ":" + model
			}
			if seen[id] {
				return
			}
			seen[id] = true
			out = append(out, connectModel{ID: id, Name: model})
		}
		for _, row := range p.cfg.Pool.ConnectModelRows(now) {
			for _, mc := range row.ModelCosts {
				add(row.Realm, mc.Model)
			}
			add(row.Realm, row.LastModel)
		}
		if p.cfg.Usage != nil {
			if daily, ok := p.cfg.Usage.DailyTokenUsageAt(now); ok {
				for _, u := range daily {
					for model := range u.Models {
						add("", model)
					}
				}
			}
		}
		if len(out) > 0 {
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
			return out, "observed"
		}
	}
	return []connectModel{
		{ID: "cn:glm-5.3", Name: "GLM 5.3（国内）"},
		{ID: "cn:deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash（国内）"},
		{ID: "global:kimi-k3", Name: "Kimi K3（国际）"},
	}, "suggested"
}

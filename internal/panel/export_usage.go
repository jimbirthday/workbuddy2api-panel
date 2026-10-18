package panel

import (
	"net/http"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// usageWindow 与 /panel/api/usage 共用同一套区间解析，确保导出的数字与页面
// 当前窗口一致。调用前已确认 p.cfg.Usage 非空。
func (p *Panel) usageWindow(r *http.Request) usage.Window {
	q := r.URL.Query()
	win := usage.Window{From: parseTimeParam(q.Get("from")), To: parseTimeParam(q.Get("to"))}
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	return win
}

// usageSnapshot 复用 usage 接口的昵称与当前倍率注入，避免导出与页面口径漂移。
func (p *Panel) usageSnapshot(r *http.Request) usage.Snapshot {
	nicks := p.cfg.Pool.Nicknames()
	var currentRate func(realm, model string) string
	var rateRevision uint64
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
		rateRevision = p.cfg.Upstream.RateRevision()
	}
	return p.cfg.Usage.SnapshotWindowRates(p.usageWindow(r), nicks, currentRate, rateRevision)
}

// usageExport 导出当前窗口的聚合明细。dim 只接受 account/model/realm；
// 默认 account。导出的是聚合数字，不包含提示词、响应正文或任何凭证。
func (p *Panel) usageExport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	snap := p.usageSnapshot(r)
	dim := r.URL.Query().Get("dim")
	header := []string{
		"dimension", "realm", "key", "nickname", "requests", "errors",
		"prompt_tokens", "completion_tokens", "total_tokens",
		"credits", "credit_samples", "credit_tokens", "credits_per_1m_tokens",
		"cache_hit_tokens", "cache_miss_tokens", "cache_hit_rate",
		"avg_latency_ms", "avg_tokens_per_second",
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	rowFor := func(dimName, key, realm, nick string, a usage.Agg) []string {
		return []string{
			dimName, csvText(realm), csvText(key), csvText(nick),
			strconv.FormatInt(a.Requests, 10), strconv.FormatInt(a.Errors, 10),
			strconv.FormatInt(a.PromptTokens, 10), strconv.FormatInt(a.CompletionTok, 10), strconv.FormatInt(a.TotalTokens, 10),
			f(a.Credits), strconv.FormatInt(a.CreditSamples, 10), strconv.FormatInt(a.CreditTokens, 10), f(a.CreditsPer1MTokens),
			strconv.FormatInt(a.CacheHitTokens, 10), strconv.FormatInt(a.CacheMissTokens, 10), f(a.CacheHitRate),
			f(a.AvgLatencyMs), f(a.AvgTPS),
		}
	}
	// 账号维度 key 是完整 UID：导出改写成脱敏展示标签，避免运维随手转发 CSV
	// 时带出可登录标识；模型/域维度本身不带账号凭证，保持原值。
	writeCSVRows(w, exportFilename("usage-"+dim, "csv"), header, func(yield func([]string) bool) {
		switch dim {
		case "model":
			for _, x := range snap.ByModel {
				if !yield(rowFor("model", x.Key, x.Realm, "", x.Agg)) {
					return
				}
			}
		case "realm":
			for _, x := range snap.ByRealm {
				if !yield(rowFor("realm", x.Key, "", "", x.Agg)) {
					return
				}
			}
		default:
			for _, x := range snap.ByAccount {
				label := logfmt.Label(x.Key, x.Extra)
				if !yield(rowFor("account", label, x.Realm, x.Extra, x.Agg)) {
					return
				}
			}
		}
	})
}

package panel

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// creditHistoryRoutePattern 集中定义路径，前端请求与测试共用，避免拼写漂移。
const creditHistoryRoutePattern = "GET /panel/api/credits/history"

// creditHistoryRoutes 注册积分获取/扣减历史。panel.go 只负责装配；
// 具体参数解析与脱敏放在本文件，便于单独测试。
func (p *Panel) creditHistoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc(creditHistoryRoutePattern, p.withAuth(p.creditHistory))
}

// creditHistory 返回当前时间窗内的积分扣减历史（只读）。kind 只接受
// spend/all，缺省 all；grant 没有可靠数据源，明确返回 501 而不是伪造领取记录。
// limit 默认 200、上限 1000。账号输出「昵称(uid8)」标签，不读取或回传
// access/refresh/device token。
func (p *Panel) creditHistory(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()

	kind := usage.CreditKind(strings.ToLower(strings.TrimSpace(q.Get("kind"))))
	switch kind {
	case "":
		kind = usage.CreditKindAll
	case usage.CreditKindGrant:
		// 现有分片桶只有按请求聚合的 credit 观测，无法区分签到奖励、活动发放
		// 与冲正；宁可明确不可用，也不能把余额变化伪造成领取流水。
		writeErr(w, http.StatusNotImplemented, "grant history is unavailable: no reliable per-grant source")
		return
	case usage.CreditKindSpend, usage.CreditKindAll:
	default:
		writeErr(w, http.StatusBadRequest, "kind must be spend or all")
		return
	}

	limit := usage.CreditHistoryDefaultLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
		if limit > usage.CreditHistoryMaxLimit {
			limit = usage.CreditHistoryMaxLimit
		}
	}

	win := usage.Window{From: parseTimeParam(q.Get("from")), To: parseTimeParam(q.Get("to"))}
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if raw := strings.TrimSpace(q.Get("hours")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				writeErr(w, http.StatusBadRequest, "hours must be a non-negative integer")
				return
			}
			win.Hours = n
		}
		// 与 /usage 的滚动窗口上限一致；hours=0 保留为全部历史。
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}

	var nicks map[string]string
	if p.cfg.Pool != nil {
		nicks = p.cfg.Pool.Nicknames()
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.CreditHistoryWithNicks(usage.CreditHistoryQuery{
		Window: win,
		Kind:   kind,
		Limit:  limit,
	}, nicks))
}

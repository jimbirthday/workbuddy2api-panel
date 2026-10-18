package panel

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// perfRoutePattern 是性能分位接口在面板 mux 上的挂载路径。集中在这里是为了让
// 路由注册、前端请求与测试共用同一个常量，避免上线时路径拼写漂移。
const perfRoutePattern = "GET /panel/api/usage/perf"

// perfRoutes 注册性能分位只读接口。panel.go 的 routes() 只需调用本函数
// （p.perfRoutes(p.mux)）；保留独立装配函数可避免主路由文件承担分位实现细节。
func (p *Panel) perfRoutes(mux *http.ServeMux) {
	mux.HandleFunc(perfRoutePattern, p.withAuth(p.perfUsage))
}

// perfUsage 返回请求延迟/吞吐分位（只读）。时间窗与筛选参数与运行日志保持
// 同一套语义：from/to 为闭区间，缺省时按 hours 取最近窗口（默认 24h）；model /
// account 为大小写不敏感包含匹配；realm 只接受 cn/global。分位直接从归档原始
// 样本计算，不复用请求列表的 1000 条上限。
//
// 响应只含聚合数字与脱敏账号标签；错误不拼接归档行或凭证。
func (p *Panel) perfUsage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	q := r.URL.Query()
	hours := 0
	if raw := strings.TrimSpace(q.Get("hours")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "hours must be a non-negative integer")
			return
		}
		// 用量页「全部历史」会显式传 hours=0。归档只保留有限天数，性能
		// 查询用服务端最大窗口解释它（而不是报 400 或悄悄退回 24h）。
		if n == 0 || n > reqlog.MaxPerfHours {
			n = reqlog.MaxPerfHours
		}
		hours = n
	}
	// 不传 hours 时仍由 reqlog.PerfQuery 统一采用默认 24h；显式 0 表示
	// 「尽可能全部」，上限与服务端窗口一致。
	samples := 0
	if raw := strings.TrimSpace(q.Get("samples")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "samples must be a positive integer")
			return
		}
		if n > reqlog.MaxPerfSamples {
			n = reqlog.MaxPerfSamples
		}
		samples = n
	}
	realm := strings.ToLower(strings.TrimSpace(q.Get("realm")))
	if realm != "" && realm != "cn" && realm != "global" {
		writeErr(w, http.StatusBadRequest, "realm must be cn or global")
		return
	}
	stats, err := p.cfg.RequestLog.PerfStats(reqlog.PerfQuery{
		From:           parseTimeParam(q.Get("from")),
		To:             parseTimeParam(q.Get("to")),
		Hours:          hours,
		Model:          q.Get("model"),
		KeyID:          q.Get("key_id"),
		Account:        q.Get("account"),
		Realm:          realm,
		Samples:        samples,
		RealmByAccount: p.perfRealmByAccount(),
	})
	if err != nil {
		// 归档路径/底层错误可能包含部署目录；只回稳定文案，细节留给服务端日志
		// 与归档状态接口。这样错误响应不会成为路径或凭证的旁路。
		writeErr(w, http.StatusInternalServerError, "read request archive failed")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// perfRealmByAccount 用池内账号的脱敏展示标签建立 realm 映射。归档中的
// Event.Account 是 logfmt.Label(uid, nick)，Pool.RealmLabels 用同一拼法，
// 故无需把完整 UID 暴露给归档或响应。Pool 为 nil（部分测试/只读面板）时
// 返回空映射，裸模型按 cn。
func (p *Panel) perfRealmByAccount() map[string]string {
	if p.cfg.Pool == nil {
		return nil
	}
	return p.cfg.Pool.RealmLabels()
}

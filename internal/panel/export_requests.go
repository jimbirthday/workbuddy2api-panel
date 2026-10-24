package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// requestLogFilter 把列表页与导出页的筛选参数解析到同一个 Filter：
// 两处若各写一份，导出的「与当前筛选一致」就只是文案承诺。
func requestLogFilter(r *http.Request) reqlog.Filter {
	q := r.URL.Query()
	return reqlog.Filter{
		Outcome:   q.Get("outcome"),
		KeyID:     q.Get("key_id"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		ErrorCode: q.Get("error_code"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	}
}

// requestLogsExport 导出当前筛选条件下的请求记录。JSONL 与归档使用同构的
// 脱敏 Event（无 prompt/响应正文/Authorization），CSV 是同一批字段的表格形态。
func (p *Panel) requestLogsExport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := parseExportLimit(r.URL.Query().Get("limit"))
	rows, err := p.cfg.RequestLog.ReadArchive(limit, requestLogFilter(r))
	if err != nil {
		log.Printf("panel: 导出请求归档失败: %v", err)
		writeErr(w, http.StatusInternalServerError, "read request archive failed")
		return
	}
	if rows == nil {
		rows = []reqlog.Event{}
	}
	if r.URL.Query().Get("format") == "jsonl" {
		writeDownloadHeaders(w, "application/x-ndjson; charset=utf-8", exportFilename("requests", "jsonl"))
		enc := json.NewEncoder(w)
		for _, e := range rows {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
		return
	}
	header := []string{
		"time", "request_id", "path", "key_id", "account", "model", "status", "ok", "outcome",
		"duration_ms", "ttfb_ms", "attempts", "prompt_tokens", "completion_tokens",
		"total_tokens", "credit", "credit_known", "cache_hit_tokens", "cache_miss_tokens",
		"client_ip", "user_agent",
	}
	writeCSVRows(w, exportFilename("requests", "csv"), header, func(yield func([]string) bool) {
		for _, e := range rows {
			credit := ""
			if e.HasCredit {
				credit = strconv.FormatFloat(e.Credit, 'f', -1, 64)
			}
			row := []string{
				e.Time.Format("2006-01-02 15:04:05"),
				csvText(e.RequestID), csvText(e.Path), csvText(e.KeyID), csvText(e.Account), csvText(e.Model),
				strconv.Itoa(e.Status), strconv.FormatBool(e.OK), csvText(e.Outcome),
				strconv.FormatInt(e.DurationMs, 10), strconv.FormatInt(e.TTFBMs, 10), strconv.Itoa(e.Attempts),
				strconv.FormatInt(e.PromptTokens, 10), strconv.FormatInt(e.CompletionTokens, 10), strconv.FormatInt(e.TotalTokens, 10),
				credit, strconv.FormatBool(e.HasCredit),
				strconv.FormatInt(e.CacheHitTokens, 10), strconv.FormatInt(e.CacheMissTokens, 10),
				csvText(e.ClientIP), csvText(e.UserAgent),
			}
			if !yield(row) {
				return
			}
		}
	})
}

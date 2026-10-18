package panel

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
)

// activityHistory 账号活动历史（签到 / 活跃 / 旅行 / 保活）的每一次真实尝试。
// 过滤在服务端完成，返回最新在前的记录；Store 由 main 以 state 同目录的
// activity_history.jsonl 装配，未装配时接口返回 501。
func (p *Panel) activityHistory(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Activity == nil {
		writeErr(w, http.StatusNotImplemented, "activity history not available")
		return
	}
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if len(query) > 64 {
		writeErr(w, http.StatusBadRequest, "q must be at most 64 characters")
		return
	}
	// uid 过滤仍保留给脚本/排障，但它只接受 Store 内部的 uid8 口径；完整 UID
	// 永远不该从浏览器或日志进入查询串。
	uidFilter := strings.TrimSpace(q.Get("uid"))
	if len(uidFilter) > 64 {
		writeErr(w, http.StatusBadRequest, "uid must be at most 64 characters")
		return
	}
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	res, err := p.cfg.Activity.Query(activity.Filter{
		UID:    uidFilter,
		Task:   activity.Task(q.Get("task")),
		Result: q.Get("result"),
		Query:  query,
		Since:  parseTimeParam(q.Get("from")),
		Until:  parseTimeParam(q.Get("to")),
		Limit:  limit,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, activity.ErrInvalidFilter) {
			status = http.StatusBadRequest
		} else {
			// os.Stat/os.Open 的错误会带出活动历史文件完整路径；响应只用稳定
			// 文案，细节留服务端日志。
			log.Printf("panel: 读取活动历史失败: %v", err)
			writeErr(w, status, "read activity history failed")
			return
		}
		writeErr(w, status, err.Error())
		return
	}
	// Store 的 Record.UID 是内部定位键：即便 normalizeRecord 已截成 uid8，
	// 也不应把 uid8 当作公开字段返回。面板只需要昵称/域/任务/结果/消息；
	// 账号标签用昵称，昵称为空时退化成“未命名账号”，绝不回显任何 UID。
	type recordView struct {
		TS       time.Time        `json:"ts"`
		Nickname string           `json:"nickname,omitempty"`
		Realm    string           `json:"realm,omitempty"`
		Task     activity.Task    `json:"task"`
		Trigger  activity.Trigger `json:"trigger"`
		OK       bool             `json:"ok"`
		Message  string           `json:"message,omitempty"`
	}
	views := make([]recordView, 0, len(res.Records))
	for _, rec := range res.Records {
		views = append(views, recordView{
			TS:       rec.Time,
			Nickname: rec.Nickname,
			Realm:    rec.Realm,
			Task:     rec.Task,
			Trigger:  rec.Trigger,
			OK:       rec.OK,
			Message:  rec.Message,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records":  views,
		"total":    res.Total,
		"limit":    res.Limit,
		"has_more": res.Total > len(views),
	})
}

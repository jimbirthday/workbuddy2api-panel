package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// taskRunTimeout is the hard cap for one panel task action. Hub's /tasks/run
// is synchronous, but a full growth-task pass includes real conversations and
// can take minutes; match the single-account auto_all timeout. On HTTP
// timeout the background goroutine still finishes and releases the lock.
const taskRunTimeout = 5 * time.Minute

// tasksAccount is the compact account shape consumed by the Hub-style task
// page. The browser only sees uid8 and the opaque server-side ref.
type tasksAccount struct {
	UID        string `json:"uid"`
	Ref        string `json:"ref"`
	Nickname   string `json:"nickname,omitempty"`
	Realm      string `json:"realm"`
	Enterprise bool   `json:"enterprise,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
}

// tasksSummary mirrors Hub's summary cards above the growth table. Counts are
// for the selected scope so the page does not recompute them from a paged list.
type tasksSummary struct {
	Accounts         int   `json:"accounts"`
	CheckinDone      int   `json:"checkin_done"`
	CheckinPending   int   `json:"checkin_pending"`
	StreakDays       int   `json:"streak_days"`
	MakeupCards      int   `json:"makeup_cards"`
	LotteryChances   int   `json:"lottery_chances"`
	Energy           int64 `json:"energy"`
	CatReward        int64 `json:"cat_reward"`
	CatDailyDone     int   `json:"cat_daily_done"`
	WelfareCredits   int64 `json:"welfare_credits_total"`
	WelfareAvailable bool  `json:"welfare_grants_available"`
}

// tasksRunResult is one per-account outcome. Message is the upstream result
// or local rejection reason, rendered verbatim so failures are actionable.
type tasksRunResult struct {
	Ref          string `json:"ref,omitempty"`
	UID          string `json:"uid,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Realm        string `json:"realm,omitempty"`
	Action       string `json:"action,omitempty"`
	OK           bool   `json:"ok"`
	Skipped      bool   `json:"skipped,omitempty"`
	Message      string `json:"message,omitempty"`
	RewardCredit int64  `json:"reward_credit,omitempty"`
}

// tasksOverview returns the Hub-shaped task page snapshot. It reuses the
// existing growth query instead of duplicating a second upstream contract:
// focus=all returns every CN personal account, a single focus returns that
// account's task rows.
func (p *Panel) tasksOverview(w http.ResponseWriter, r *http.Request) {
	statuses := p.cfg.Pool.List()
	p.rememberBenefitRefs(statuses)

	focus := strings.TrimSpace(r.URL.Query().Get("focus"))
	if focus == "" {
		focus = strings.TrimSpace(r.URL.Query().Get("uid"))
	}
	all := focus == "" || strings.EqualFold(focus, "all")
	focusUID := ""
	if !all {
		focusUID = p.benefitUIDForRef(focus)
		if focusUID == "" {
			focusUID = focus
		}
	}

	accounts := make([]tasksAccount, 0, len(statuses))
	for _, st := range statuses {
		if st.Enterprise || st.Disabled || st.Realm != "cn" {
			continue
		}
		if !all && st.UID != focusUID {
			continue
		}
		accounts = append(accounts, tasksAccount{
			UID: statusUID8(st.UID), Ref: benefitRefFor(st.UID),
			Nickname: st.Nickname, Realm: st.Realm,
		})
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Nickname != accounts[j].Nickname {
			return accounts[i].Nickname < accounts[j].Nickname
		}
		return accounts[i].UID < accounts[j].UID
	})

	taskRows := []upstream.Task{}
	taskErr := ""
	var growth benefitGrowth
	if !all && len(accounts) == 1 {
		if a := p.cfg.Pool.AuthByUID(focusUID); a != nil && p.cfg.Upstream != nil {
			// Reuse the same 30s snapshot as /benefits. The browser loads both
			// endpoints in parallel; querying growth twice would double the
			// upstream RPC count for every focus change. /benefits owns the
			// refresh=1 bypass, so this endpoint only consumes the snapshot.
			snap := p.benefitsGrowthSnapshot(statuses, false)
			if g, ok := snap[focusUID]; ok {
				growth = g
			} else {
				growth = p.growthResult(r.Context(), a)
			}
			taskRows = growth.Tasks
			taskErr = growth.TasksErr
		}
	}

	summary := tasksSummary{Accounts: len(accounts)}
	for _, st := range statuses {
		if st.Realm != "cn" || st.Enterprise || st.Disabled {
			continue
		}
		if st.CheckinDone {
			summary.CheckinDone++
		} else {
			summary.CheckinPending++
		}
	}
	if !all {
		summary.StreakDays = growth.StreakDays
		summary.MakeupCards = growth.MakeupCards
		summary.LotteryChances = growth.LotteryChances
		summary.Energy = growth.Energy
		summary.CatReward = growth.CatReward
		if growth.CatDailyDone {
			summary.CatDailyDone = 1
		}
	}
	if grants := p.welfareGrantRows(); grants.available {
		summary.WelfareAvailable = true
		for _, row := range grants.rows {
			summary.WelfareCredits += row.Size
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"focus":           focus,
		"accounts":        accounts,
		"summary":         summary,
		"tasks":           taskRows,
		"tasks_error":     taskErr,
		"welfare_credits": summary.WelfareCredits,
	})
}

// tasksRunRequest accepts Hub's POST /tasks/run shape. uid/ref/scope are
// alternative selectors; all or an absent selector means every CN personal
// account.
type tasksRunRequest struct {
	Scope       string `json:"scope"`
	UID         string `json:"uid"`
	Ref         string `json:"ref"`
	Action      string `json:"action"`
	Concurrency int    `json:"concurrency"`
}

// tasksRun is the Hub-shaped synchronous action endpoint. One request can run
// checkin, growth tasks, travel or streak; per-account outcomes are returned
// so the page can render an execution log without polling a batch-state API.
func (p *Panel) tasksRun(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client unavailable")
		return
	}
	var req tasksRunRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	selector := strings.TrimSpace(req.Ref)
	if selector == "" {
		selector = strings.TrimSpace(req.UID)
	}
	if selector == "" {
		selector = strings.TrimSpace(req.Scope)
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "", "tasks", "auto", "auto_all", "run":
		action = "tasks"
	case "checkin", "travel", "streak":
	default:
		writeErr(w, http.StatusBadRequest, "unknown action: "+action)
		return
	}

	targets, msg := p.taskTargets(selector)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "action": action, "accounts_count": 0,
			"results": []tasksRunResult{}, "logs": []string{"没有可执行的账号"},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), taskRunTimeout)
	defer cancel()
	type outcome struct{ results []tasksRunResult }
	done := make(chan outcome, 1)
	if !p.GoBackground(func() {
		done <- outcome{results: p.runTaskTargets(ctx, targets, action, req.Concurrency)}
	}) {
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	}
	select {
	case res := <-done:
		credits := int64(0)
		logs := make([]string, 0, len(res.results))
		for _, item := range res.results {
			status := "OK"
			if !item.OK {
				status = "FAIL"
			}
			logs = append(logs, "["+status+"] "+item.Nickname+" · "+item.Message)
			credits += item.RewardCredit
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "action": action, "accounts_count": len(targets),
			"results": res.results, "logs": logs, "credit_added": credits,
		})
		p.invalidateBenefitsGrowth()
		p.invalidateOverview()
	case <-ctx.Done():
		writeErr(w, http.StatusGatewayTimeout, "任务执行超时（后台可能仍在继续）")
	}
}

// taskTargets resolves "all" or a short ref into eligible accounts. An
// unknown ref is rejected instead of silently falling back to the whole pool.
func (p *Panel) taskTargets(selector string) ([]*auth.Auth, string) {
	statuses := p.cfg.Pool.List()
	p.rememberBenefitRefs(statuses)
	selector = strings.TrimSpace(selector)
	all := selector == "" || strings.EqualFold(selector, "all")
	uid := ""
	if !all {
		uid = p.benefitUIDForRef(selector)
		if uid == "" {
			uid = selector
		}
	}
	var out []*auth.Auth
	for _, st := range statuses {
		if st.Enterprise || st.Disabled || st.Realm != "cn" {
			continue
		}
		if !all && st.UID != uid {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		out = append(out, a)
	}
	if !all && len(out) == 0 {
		return nil, "account not found or not eligible"
	}
	return out, ""
}

func (p *Panel) runTaskTargets(ctx context.Context, targets []*auth.Auth, action string, concurrency int) []tasksRunResult {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	results := make([]tasksRunResult, len(targets))
	for i, a := range targets {
		wg.Add(1)
		go func(idx int, acct *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = p.runTaskTarget(ctx, acct, action)
		}(i, a)
	}
	wg.Wait()
	return results
}

func (p *Panel) runTaskTarget(ctx context.Context, a *auth.Auth, action string) tasksRunResult {
	out := tasksRunResult{
		Ref: benefitRefFor(a.UID), UID: statusUID8(a.UID),
		Nickname: a.Nickname, Realm: a.Realm(), Action: action,
	}
	if ctx.Err() != nil {
		out.Message = "已取消"
		return out
	}
	if !p.tryLockAccount(a.UID) {
		out.Message = "账号有操作正在执行"
		return out
	}
	defer p.unlockAccount(a.UID)
	// The task page's execution log is backed by the durable activity store.
	// Record the three actions that already have activity task types; growth
	// tasks stay in the per-account result log because adding a new persisted
	// task enum would break old readers.
	record := func(task activity.Task, ok bool, message string) {
		if p.cfg.Activity == nil {
			return
		}
		_ = p.cfg.Activity.Append(activity.Record{
			UID: a.UID, Nickname: a.Nickname, Realm: a.Realm(),
			Task: task, Trigger: activity.TriggerManual, OK: ok, Message: message,
		})
	}

	switch action {
	case "checkin":
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				p.cfg.Pool.NoteCheckinDone(a.UID)
				out.OK, out.Message = true, "今天已签到"
			} else {
				out.Message = err.Error()
			}
			record(activity.TaskCheckin, out.OK, out.Message)
			return out
		}
		p.cfg.Pool.NoteCheckinDone(a.UID)
		out.OK, out.Message = true, "签到成功"
		record(activity.TaskCheckin, true, out.Message)
	case "travel":
		if p.cfg.Scheduler == nil {
			out.Message = "scheduler unavailable"
			record(activity.TaskTravel, false, out.Message)
			return out
		}
		if err := p.cfg.Scheduler.RunTravelFor(a); err != nil {
			out.Message = err.Error()
			record(activity.TaskTravel, false, out.Message)
			return out
		}
		out.OK, out.Message = true, "旅行巡检完成"
		record(activity.TaskTravel, true, out.Message)
	case "streak":
		if p.cfg.Scheduler == nil {
			out.Message = "scheduler unavailable"
			record(activity.TaskActivity, false, out.Message)
			return out
		}
		if err := p.cfg.Scheduler.RunStreakBonusFor(a, activity.TriggerManual); err != nil {
			out.Message = err.Error()
			record(activity.TaskActivity, false, out.Message)
			return out
		}
		out.OK, out.Message = true, "连登兑换与抽奖完成"
		record(activity.TaskActivity, true, out.Message)
	default:
		items := p.runAutoAll(a)
		okCount, failCount := 0, 0
		for _, item := range items {
			switch item["status"] {
			case "done":
				okCount++
			case "error":
				failCount++
			}
		}
		out.OK = failCount == 0
		out.Message = "完成 " + strconv.Itoa(okCount) + " 项"
		if failCount > 0 {
			out.Message += "，失败 " + strconv.Itoa(failCount) + " 项"
		}
	}
	return out
}

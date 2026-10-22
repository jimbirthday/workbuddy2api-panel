package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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
	options := make([]tasksAccount, 0, len(statuses))
	for _, st := range statuses {
		if st.Enterprise || st.Disabled || st.Realm != "cn" {
			continue
		}
		account := tasksAccount{
			UID: statusUID8(st.UID), Ref: benefitRefFor(st.UID),
			Nickname: st.Nickname, Realm: st.Realm,
		}
		options = append(options, account)
		if all || st.UID == focusUID {
			accounts = append(accounts, account)
		}
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Nickname != accounts[j].Nickname {
			return accounts[i].Nickname < accounts[j].Nickname
		}
		return accounts[i].UID < accounts[j].UID
	})
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].Nickname != options[j].Nickname {
			return options[i].Nickname < options[j].Nickname
		}
		return options[i].UID < options[j].UID
	})

	taskRows := []upstream.Task{}
	taskErr := ""
	var growth benefitGrowth
	if !all && len(accounts) == 1 {
		if a := p.cfg.Pool.AuthByUID(focusUID); a != nil && p.cfg.Upstream != nil {
			growth = p.focusGrowth(r.Context(), focusUID, statuses)
			taskRows = append(taskRows, growth.Tasks...)
			taskErr = growth.TasksErr
		}
	}

	summary := tasksSummary{Accounts: len(accounts)}
	for _, st := range statuses {
		if st.Realm != "cn" || st.Enterprise || st.Disabled {
			continue
		}
		if !all && st.UID != focusUID {
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
		"account_options": options,
		"summary":         summary,
		"tasks":           taskRows,
		"tasks_error":     taskErr,
		"welfare_credits": summary.WelfareCredits,
	})
}

// focusGrowth 取单个聚焦账号的成长明细。
//
// 本页只渲染这一个账号的任务行，而 benefitsGrowthSnapshot 会对传入的每个账号
// 并发打 2 个上游端点。早先直接把整池 statuses 传进去，等于「切一个账号」触发
// 一次全池成长扫描（470 号 = 数百次 RPC），且与并行加载的 /benefits 完全重复。
//
// 现在的三级取值：已缓存的整池快照（/benefits 通常已经就绪或正在飞）→ 单账号
// 子集快照（自成一条缓存 key，不覆盖整池快照）→ 单账号直查兜底。
func (p *Panel) focusGrowth(ctx context.Context, focusUID string, statuses []pool.Status) benefitGrowth {
	if g, ok := p.benefitsGrowthCached(focusUID); ok {
		return g
	}
	focusStatuses := make([]pool.Status, 0, 1)
	for _, st := range statuses {
		if st.UID == focusUID {
			focusStatuses = append(focusStatuses, st)
			break
		}
	}
	if snap := p.benefitsGrowthSnapshot(focusStatuses, false); snap != nil {
		if g, ok := snap[focusUID]; ok {
			return g
		}
	}
	a := p.cfg.Pool.AuthByUID(focusUID)
	if a == nil {
		return benefitGrowth{}
	}
	return p.growthResult(ctx, a)
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
	Async       bool   `json:"async"`
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
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := decoder.Decode(&req); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeErr(w, http.StatusBadRequest, "invalid task request")
			return
		}
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
	if req.Async {
		p.startTaskRun(w, targets, action, selector, req.Concurrency)
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
	return p.runTaskTargetsProgress(ctx, targets, action, concurrency, nil)
}

func (p *Panel) runTaskTargetsProgress(ctx context.Context, targets []*auth.Auth, action string, concurrency int, progress func(tasksRunResult)) []tasksRunResult {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	results := make([]tasksRunResult, len(targets))
	runBounded(concurrency, len(targets), func(i int) {
		results[i] = p.runTaskTarget(ctx, targets[i], action)
		if progress != nil {
			progress(results[i])
		}
	})
	return results
}

// taskRunConflictLocked 返回与本轮重叠的在跑批次（调用方必须持有 p.batchMu）。
// 重叠口径：scope 相同、任一方为 all（全池请求必然触碰目标账号）。
func (p *Panel) taskRunConflictLocked(action, scope string) *batchRun {
	prefix := "tasks_" + action
	for kind, run := range p.batchRuns {
		if run == nil || !run.Running {
			continue
		}
		if kind != prefix && !strings.HasPrefix(kind, prefix+"_") {
			continue
		}
		if run.Scope == scope || run.Scope == "all" || scope == "all" {
			return run
		}
	}
	return nil
}

// startTaskRun returns immediately and publishes account outcomes through the
// existing batch endpoint. A browser disconnect never cancels accepted work.
func (p *Panel) startTaskRun(w http.ResponseWriter, targets []*auth.Auth, action, scope string, concurrency int) {
	if scope == "" || strings.EqualFold(scope, "all") {
		scope = "all"
	} else {
		scope = benefitRefFor(targets[0].UID)
	}
	kind := "tasks_" + action
	// 单账号动作按账号区分子类：不同账号之间本来就有 per-account 锁保护，不该
	// 被同一把全局单飞挡住（此前账号 A 在跑时点账号 B 的「一键做任务」直接
	// 409，清 10 个账号只能串行 40 分钟）。
	if len(targets) == 1 && scope != "all" {
		kind += "_" + scope
	}
	run := &batchRun{Kind: kind, StartedAt: time.Now(), Running: true, Total: len(targets), Scope: scope}
	p.batchMu.Lock()
	// 冲突判定按 scope 重叠：同一账号重复点击仍拒绝；全池请求与任一单账号
	// 请求重叠（它会触碰该账号）也拒绝；不同账号之间互不阻塞。
	if current := p.taskRunConflictLocked(action, scope); current != nil {
		p.batchMu.Unlock()
		writeErr(w, http.StatusConflict, "该动作正在执行，请查看执行进度")
		return
	}
	if p.batchRuns == nil {
		p.batchRuns = make(map[string]*batchRun)
	}
	p.batchRuns[kind] = run
	p.batchMu.Unlock()
	if !p.GoBackground(func() {
		defer func() {
			p.batchMu.Lock()
			if v := recover(); v != nil {
				log.Printf("panel: 后台任务执行异常: %v", v)
				run.Note = "执行异常，请查看运行日志"
			}
			run.Running, run.Done = false, true
			p.batchMu.Unlock()
			p.invalidateBenefitsGrowth()
			p.invalidateOverview()
		}()
		p.runTaskTargetsProgress(context.Background(), targets, action, concurrency, func(result tasksRunResult) {
			p.batchMu.Lock()
			run.Results = append(run.Results, result)
			if result.OK {
				run.OK++
			} else {
				run.Failed++
			}
			p.batchMu.Unlock()
		})
	}) {
		p.batchMu.Lock()
		delete(p.batchRuns, kind)
		p.batchMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true, "kind": kind, "accounts_count": len(targets)})
}

func (p *Panel) runTaskTarget(ctx context.Context, a *auth.Auth, action string) (out tasksRunResult) {
	out = tasksRunResult{
		Ref: benefitRefFor(a.UID), UID: statusUID8(a.UID),
		Nickname: a.Nickname, Realm: a.Realm(), Action: action,
	}
	defer func() {
		if v := recover(); v != nil {
			log.Printf("panel: 任务执行异常: %v", v)
			out.OK, out.Message = false, "执行异常，请查看运行日志"
		}
		p.invalidateBenefitsGrowth()
		p.invalidateOverview()
	}()
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
	// Scheduler actions write their own detailed history; never append a second
	// synthetic success that could hide their actual outcome.
	record := func(task activity.Task, ok bool, message string) {
		p.recordManualActivity(a, task, ok, message)
	}

	switch action {
	case "checkin":
		out.OK, out.Message = p.checkinAccount(a)
	case "travel":
		if p.cfg.Scheduler == nil {
			out.Message = "scheduler unavailable"
			record(activity.TaskTravel, false, out.Message)
			return out
		}
		if err := p.cfg.Scheduler.RunTravelFor(a); err != nil {
			out.Message = err.Error()
			return out
		}
		out.OK, out.Message = true, "旅行巡检完成"
	case "streak":
		if p.cfg.Scheduler == nil {
			out.Message = "scheduler unavailable"
			record(activity.TaskStreak, false, out.Message)
			return out
		}
		if err := p.cfg.Scheduler.RunStreakBonusFor(a, activity.TriggerManual); err != nil {
			out.Message = err.Error()
			return out
		}
		out.OK, out.Message = true, "连登兑换与抽奖完成"
	default:
		items := p.runAutoAll(a)
		okCount, failCount := 0, 0
		firstError := ""
		for _, item := range items {
			if firstError == "" {
				if err, ok := item["claim_error"]; ok {
					firstError = fmt.Sprint(item["task_code"], "：", err)
				} else if item["status"] == "error" {
					firstError = fmt.Sprint(item["task_code"], "：", item["message"])
				}
			}
			switch item["status"] {
			case "done":
				if item["claim_error"] != nil {
					failCount++
				} else {
					okCount++
				}
			case "error":
				failCount++
			}
		}
		out.OK = failCount == 0
		out.Message = "完成 " + strconv.Itoa(okCount) + " 项"
		if failCount > 0 {
			out.Message += "，失败 " + strconv.Itoa(failCount) + " 项；" + firstError
		}
	}
	return out
}

// checkinAccount is shared by both panel entry points so their pool state and
// activity outcome cannot disagree. Balance refresh remains the caller's choice.
func (p *Panel) checkinAccount(a *auth.Auth) (bool, string) {
	err := p.cfg.Upstream.DailyCheckin(a)
	ok, message := true, "签到成功"
	if upstream.IsAlreadyCheckin(err) {
		message = "今天已签到"
	} else if err != nil {
		ok, message = false, err.Error()
	}
	if ok {
		p.cfg.Pool.NoteCheckinDone(a.UID)
	}
	p.recordManualActivity(a, activity.TaskCheckin, ok, message)
	return ok, message
}

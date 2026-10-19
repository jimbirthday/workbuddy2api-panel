package panel

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// benefitsGrowthTimeout 单账号福利详情的上游查询上限。福利页是只读快照：
// 一个账号 hang 不能把整页拖到 HTTP 客户端 120s；超时后该账号字段留空，
// 页面仍展示签到/活动等本地事实。
var benefitsGrowthTimeout = 12 * time.Second

// benefitsOverview 是「任务与福利」页的统一只读快照：页面不再从账号表、
// 任务扫描和活动历史三处各拼一半，避免概览数字与明细互相矛盾。接口只读
// 上游缓存/本地状态，不触发任何签到或对话请求。
//
// 返回结构按页面消费口径分组：
//   - summary：全池与国内/国际账号计数、今日已签到、成长待办与队列结果；
//   - accounts：每个账号的福利状态（区域、今日签到、最近活动、成长待办）；
//   - activity：最近一次活动记录，供页面首屏保留上下文。
//
// 账号 UID 只输出 uid8，避免完整 UID 进入浏览器；排序固定（区域→昵称→uid8），
// 前端无需再次排序。
func (p *Panel) benefitsOverview(w http.ResponseWriter, r *http.Request) {
	statuses := p.cfg.Pool.List()
	p.rememberBenefitRefs(statuses)
	// 聚焦对象在池的原始顺序上确定（Hub 默认第一个启用中的国内个人号），
	// 之后再按区域/昵称排序展示；否则排序会改变“默认选谁”，导致刷新后
	// 选择器跳到另一个账号。
	focusRef := strings.TrimSpace(r.URL.Query().Get("focus"))
	focusUID := p.benefitUIDForRef(focusRef)
	focusPoolUID := ""
	for _, st := range statuses {
		if st.Realm != "cn" || st.Enterprise || st.Disabled {
			continue
		}
		if focusUID != "" && st.UID == focusUID {
			focusPoolUID = st.UID
			break
		}
		if focusPoolUID == "" {
			focusPoolUID = st.UID
		}
	}
	sort.SliceStable(statuses, func(i, j int) bool {
		if statuses[i].Realm != statuses[j].Realm {
			return statuses[i].Realm < statuses[j].Realm
		}
		ni, nj := strings.TrimSpace(statuses[i].Nickname), strings.TrimSpace(statuses[j].Nickname)
		if ni != nj {
			return ni < nj
		}
		return statuses[i].UID < statuses[j].UID
	})

	// 最近一次真实尝试按账号聚合，供页面在福利表里直接显示「上次结果」。
	lastByUID := map[string]activity.Record{}
	todayFailed := 0
	todayStart := time.Now().In(time.Local)
	todayStart = time.Date(todayStart.Year(), todayStart.Month(), todayStart.Day(), 0, 0, 0, 0, todayStart.Location())
	if p.cfg.Activity != nil {
		if res, err := p.cfg.Activity.Query(activity.Filter{Limit: 1000}); err == nil {
			for _, rec := range res.Records {
				if !rec.OK && !rec.Time.Before(todayStart) {
					todayFailed++
				}
				uid := strings.TrimSpace(rec.UID)
				if uid == "" {
					continue
				}
				if _, ok := lastByUID[uid]; !ok {
					lastByUID[uid] = rec
				}
			}
		}
	}

	type accountBenefit struct {
		UID            string    `json:"uid"`
		Ref            string    `json:"ref"`
		Nickname       string    `json:"nickname,omitempty"`
		Realm          string    `json:"realm"`
		Enterprise     bool      `json:"enterprise"`
		Disabled       bool      `json:"disabled,omitempty"`
		Cooling        bool      `json:"cooling,omitempty"`
		CheckinDone    bool      `json:"checkin_done"`
		Credit         int64     `json:"credits"`
		CreditTotal    int64     `json:"credits_total"`
		LastTask       string    `json:"last_task,omitempty"`
		LastOK         bool      `json:"last_ok,omitempty"`
		LastMessage    string    `json:"last_message,omitempty"`
		LastAt         time.Time `json:"last_at,omitempty"`
		GrowthPending  int       `json:"growth_pending"`
		DailyChatAt    time.Time `json:"daily_chat_at,omitempty"`
		DailyChatOK    bool      `json:"daily_chat_ok,omitempty"`
		DailyChatToday bool      `json:"daily_chat_today"`
		// 国内成长福利（Hub 任务与福利页同口径）。企业号/国际号不查询，
		// 字段保持零值并由 available=false 让前端隐藏。
		GrowthAvailable bool   `json:"growth_available,omitempty"`
		StreakDays      int    `json:"streak_days,omitempty"`
		MakeupCards     int    `json:"makeup_cards,omitempty"`
		LotteryChances  int    `json:"lottery_chances,omitempty"`
		Energy          int64  `json:"energy,omitempty"`
		CatState        string `json:"cat_state,omitempty"`
		CatReward       int64  `json:"cat_reward,omitempty"`
		CatDailyDone    bool   `json:"cat_daily_done,omitempty"`
		Tier7d          string `json:"tier_7d,omitempty"`
		Tier14d         string `json:"tier_14d,omitempty"`
		Tier28d         string `json:"tier_28d,omitempty"`
		GrowthError     string `json:"growth_error,omitempty"`
		// Focused 标记当前选择器聚焦的账号；该账号的成长任务明细统一从
		// 顶层 task_rows 返回，避免每个账号都复制一份完整任务列表。
		Focused bool `json:"focused,omitempty"`
	}

	summary := struct {
		Accounts         int       `json:"accounts"`
		CN               int       `json:"cn"`
		Global           int       `json:"global"`
		Enterprise       int       `json:"enterprise"`
		CheckinDone      int       `json:"checkin_done"`
		CheckinPending   int       `json:"checkin_pending"`
		TodayFailed      int       `json:"today_failed"`
		GrowthPending    int       `json:"growth_pending"`
		GrowthRunning    int       `json:"growth_running"`
		GrowthDone       int       `json:"growth_done"`
		GrowthFailed     int       `json:"growth_failed"`
		LastActivityAt   time.Time `json:"last_activity_at,omitempty"`
		LastActivityTask string    `json:"last_activity_task,omitempty"`
		DailyChatDone    int       `json:"daily_chat_done"`
		DailyChatPending int       `json:"daily_chat_pending"`
		StreakDays       int       `json:"streak_days"`
		MakeupCards      int       `json:"makeup_cards"`
		LotteryChances   int       `json:"lottery_chances"`
		Energy           int64     `json:"energy"`
		// Welfare credits come from the latest credit-package snapshot (the same
		// source and filter as the Hub task/welfare page). Free plans and trial
		// packages are not welfare; when no snapshot exists, available=false so the
		// UI can ask for a refresh instead of presenting a fake zero.
		WelfareCreditsTotal    int64  `json:"welfare_credits_total"`
		WelfareGrantCount      int    `json:"welfare_grant_count"`
		WelfareGrantsAvailable bool   `json:"welfare_grants_available"`
		WelfareSnapshotAt      string `json:"welfare_snapshot_at,omitempty"`
	}{}
	dailyChatByUID := map[string]activity.Record{}
	if p.cfg.Activity != nil {
		if res, err := p.cfg.Activity.Query(activity.Filter{Task: activity.TaskDailyChat, Limit: 1000}); err == nil {
			for _, rec := range res.Records {
				uid := strings.TrimSpace(rec.UID)
				if uid == "" {
					continue
				}
				if _, ok := dailyChatByUID[uid]; !ok {
					dailyChatByUID[uid] = rec
				}
			}
		}
	}

	pendingByUID := map[string]int{}
	if st := p.scan(); st != nil {
		st.mu.Lock()
		running := st.running
		pending, errors := st.pending, st.errors
		for _, item := range st.items {
			if item.UID == "" {
				continue
			}
			n := 0
			for _, task := range item.Growth {
				if growthPending(task) {
					n++
				}
			}
			pendingByUID[item.UID] = n
		}
		st.mu.Unlock()
		summary.GrowthPending = pending
		summary.GrowthFailed = errors
		if running {
			summary.GrowthRunning = 1
		}
	}
	if q := p.queue(); q != nil {
		q.mu.Lock()
		running := q.running
		for _, item := range q.items {
			switch item.Status {
			case "done", "ok", "claimed":
				summary.GrowthDone++
			case "failed", "error":
				summary.GrowthFailed++
			}
		}
		if running {
			summary.GrowthRunning = 1
		}
		q.mu.Unlock()
	}

	// 成长福利详情对所有国内个人号并发查询（固定 4 worker），避免顺序查询
	// 把一页首屏拖成 N×4 次上游 RTT。失败只影响该行详情，不影响汇总。
	growthByUID := p.benefitsGrowthSnapshot(statuses)
	for _, g := range growthByUID {
		summary.StreakDays += g.StreakDays
		summary.MakeupCards += g.MakeupCards
		summary.LotteryChances += g.LotteryChances
		summary.Energy += g.Energy
	}

	out := make([]accountBenefit, 0, len(statuses))
	var lastAt time.Time
	var lastTask string
	for _, st := range statuses {
		summary.Accounts++
		if st.Realm == "global" {
			summary.Global++
		} else {
			summary.CN++
		}
		if st.Enterprise {
			summary.Enterprise++
		}
		uid8 := statusUID8(st.UID)
		if st.Realm == "global" && !st.Enterprise {
			rec, ok := dailyChatByUID[uid8]
			if ok && rec.OK && !rec.Time.Before(todayStart) {
				summary.DailyChatDone++
			} else {
				summary.DailyChatPending++
			}
		}
		// 企业号无个人福利体系：仍列入账号但不参与签到分母，页面据此隐藏动作。
		if !st.Enterprise && st.Realm != "global" {
			if st.CheckinDone {
				summary.CheckinDone++
			} else {
				summary.CheckinPending++
			}
		}
		row := accountBenefit{
			UID:           uid8,
			Ref:           benefitRefFor(st.UID),
			Nickname:      st.Nickname,
			Realm:         st.Realm,
			Enterprise:    st.Enterprise,
			Disabled:      st.Disabled,
			Cooling:       st.Cooling,
			CheckinDone:   st.CheckinDone,
			Credit:        st.Credits,
			CreditTotal:   st.CreditsTotal,
			GrowthPending: pendingByUID[uid8],
		}
		if rec, ok := lastByUID[uid8]; ok {
			row.LastTask = string(rec.Task)
			row.LastOK = rec.OK
			row.LastMessage = rec.Message
			row.LastAt = rec.Time
			if rec.Time.After(lastAt) {
				lastAt = rec.Time
				lastTask = string(rec.Task)
			}
		}
		if rec, ok := dailyChatByUID[uid8]; ok {
			row.DailyChatAt = rec.Time
			row.DailyChatOK = rec.OK
			row.DailyChatToday = rec.OK && !rec.Time.Before(todayStart)
		}
		if g, ok := growthByUID[st.UID]; ok {
			row.GrowthAvailable = true
			row.StreakDays = g.StreakDays
			row.MakeupCards = g.MakeupCards
			row.LotteryChances = g.LotteryChances
			row.Energy = g.Energy
			row.CatState = g.CatState
			row.CatReward = g.CatReward
			row.CatDailyDone = g.CatDailyDone
			row.Tier7d = g.Tier7d
			row.Tier14d = g.Tier14d
			row.Tier28d = g.Tier28d
			row.GrowthError = g.Err
		}
		out = append(out, row)
	}
	summary.LastActivityAt = lastAt
	summary.LastActivityTask = lastTask
	summary.TodayFailed = todayFailed

	// 选择器聚焦：Hub 的任务页一次展示一个国内个人号的成长任务明细。
	// focus 是页面持有的服务端短引用；未知/过期引用回落到第一个可用国内号。
	// 任务列表已在 benefitsGrowthSnapshot 中随成长汇总一起拉取，这里只复用
	// 快照，不再额外请求两个任务端点（否则每轮概览会多打 2N 次上游）。
	taskRows := []upstream.Task{}
	taskRowsErr := ""
	if focusPoolUID != "" {
		for i := range out {
			if benefitRefFor(focusPoolUID) != out[i].Ref {
				continue
			}
			out[i].Focused = true
			if g, ok := growthByUID[focusPoolUID]; ok {
				taskRows = g.Tasks
				taskRowsErr = g.TasksErr
				if g.TasksErr != "" && out[i].GrowthError == "" {
					out[i].GrowthError = g.TasksErr
				}
			}
			break
		}
	}

	welfareGrants := p.welfareGrantRows()
	for _, row := range welfareGrants.rows {
		summary.WelfareCreditsTotal += row.Size
	}
	summary.WelfareGrantCount = len(welfareGrants.rows)
	summary.WelfareGrantsAvailable = welfareGrants.available
	if !welfareGrants.fetchedAt.IsZero() {
		summary.WelfareSnapshotAt = welfareGrants.fetchedAt.Format(time.RFC3339)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"summary":         summary,
		"accounts":        out,
		"recent_grants":   welfareGrants.recent,
		"task_rows":       taskRows,
		"task_rows_error": taskRowsErr,
	})
}

// benefitGrantRow is the compact welfare grant record consumed by the panel.
// Full UIDs never leave the server; the row only carries display fields.
type benefitGrantRow struct {
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	Realm       string `json:"realm"`
	Name        string `json:"name"`
	GrantReason string `json:"grant_reason,omitempty"`
	Size        int64  `json:"size"`
	Remain      int64  `json:"remain"`
	CreatedAt   string `json:"created_at,omitempty"`
	ExpireAt    string `json:"expire_at,omitempty"`
	NoExpiry    bool   `json:"no_expiry"`
	Status      string `json:"status"`
}

type welfareGrantSnapshot struct {
	rows      []benefitGrantRow
	recent    []benefitGrantRow
	fetchedAt time.Time
	available bool
}

const benefitGrantRecentLimit = 30

// welfareGrantRows filters the existing credit snapshot using the Hub's
// welfare keywords. It never calls upstream and never infers welfare from a
// package amount alone, so plan/trial grants are not counted as welfare.
func (p *Panel) welfareGrantRows() welfareGrantSnapshot {
	snaps := p.creditSnapshots()
	if len(snaps) == 0 {
		return welfareGrantSnapshot{rows: []benefitGrantRow{}, recent: []benefitGrantRow{}}
	}
	out := welfareGrantSnapshot{rows: make([]benefitGrantRow, 0, 16), available: true}
	accounts := make(map[string]*auth.Auth)
	if p.cfg.Pool != nil {
		for _, st := range p.cfg.Pool.List() {
			accounts[st.UID] = p.cfg.Pool.AuthByUID(st.UID)
		}
	}
	for _, snap := range snaps {
		if snap.FetchedAt.After(out.fetchedAt) {
			out.fetchedAt = snap.FetchedAt
		}
		// Hub 的「已获取福利总积分」只看国内版个人号：企业号没有个人成长
		// 福利体系，国际版走 Bonus Pack 另一套口径。快照本身不带企业标记，
		// 这里按当前池的实际账号过滤，避免企业签到包抬高国内福利总数。
		acct := accounts[snap.UID]
		if acct != nil && (acct.IsEnterprise() || acct.IsGlobal()) {
			continue
		}
		for _, pkg := range snap.Packages {
			if !isWelfareGrant("", welfareGrantText(pkg)) {
				continue
			}
			expires, expireISO, noExpiry, _ := grantExpiry(pkg)
			status, _ := grantStatus(pkg, expires, noExpiry, time.Now())
			out.rows = append(out.rows, benefitGrantRow{
				UID:       grantUID(snap.UID),
				Nickname:  snap.Nickname,
				Realm:     snap.Realm,
				Name:      firstNonEmpty(pkg.Name, pkg.PackageCode, "Package"),
				Size:      pkg.Size,
				Remain:    pkg.Remain,
				CreatedAt: pkg.CreatedAt,
				ExpireAt:  expireISO,
				NoExpiry:  noExpiry,
				Status:    status,
			})
		}
	}
	sort.SliceStable(out.rows, func(i, j int) bool {
		ti, tj := parseGrantTime(out.rows[i].CreatedAt), parseGrantTime(out.rows[j].CreatedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		if out.rows[i].Nickname != out.rows[j].Nickname {
			return out.rows[i].Nickname < out.rows[j].Nickname
		}
		return out.rows[i].Name < out.rows[j].Name
	})
	n := len(out.rows)
	if n > benefitGrantRecentLimit {
		n = benefitGrantRecentLimit
	}
	out.recent = append([]benefitGrantRow(nil), out.rows[:n]...)
	return out
}

// isWelfareGrant mirrors the Hub task/welfare filter. Upstream has no single
// source field, so only explicit welfare keywords count; package names and
// amounts alone are never used to guess.
// welfareGrantText gathers the upstream fields that can identify a welfare
// package. The billing package payload has no dedicated grant_reason field, so
// the package name and sub-product description are the available source.
func welfareGrantText(pkg upstream.CreditPackage) string {
	return strings.Join([]string{pkg.Name, pkg.SubProductName, pkg.PackageCode}, " ")
}

func isWelfareGrant(reason, name string) bool {
	text := strings.ToLower(strings.TrimSpace(reason) + " " + strings.TrimSpace(name))
	for _, key := range []string{"签到", "成长", "活动", "裂变", "打卡", "旅行", "抽奖", "连登"} {
		if strings.Contains(text, key) {
			return true
		}
	}
	for _, key := range []string{"checkin", "check-in", "growth", "activity", "campaign", "streak", "travel", "lottery"} {
		if strings.Contains(text, key) {
			return true
		}
	}
	return false
}

// batchRun is the panel-visible outcome of the latest manual batch. The
// scheduler still owns single-flight; this record only says whether the
// batch is done and how many activity rows it produced.
type batchRun struct {
	Kind      string    `json:"kind"`
	StartedAt time.Time `json:"started_at"`
	Running   bool      `json:"running"`
	Done      bool      `json:"done"`
	Total     int       `json:"total"`
	OK        int       `json:"ok"`
	Failed    int       `json:"failed"`
	Note      string    `json:"note,omitempty"`
}

const batchRunStaleAfter = 30 * time.Minute

// startBenefitBatch starts one manual batch through the scheduler gate and
// records its outcome. It returns false when another run of the same kind is
// already active, matching the existing 409 contract.
func (p *Panel) startBenefitBatch(kind string, fn func()) bool {
	started := time.Now()
	done, ok := p.cfg.Scheduler.TryRunBatchDone(kind, fn)
	if !ok {
		return false
	}
	p.batchMu.Lock()
	if p.batchRuns == nil {
		p.batchRuns = make(map[string]*batchRun)
	}
	p.batchRuns[kind] = &batchRun{Kind: kind, StartedAt: started, Running: true}
	p.batchMu.Unlock()
	go p.finishBenefitBatch(kind, started, done)
	return true
}

// finishBenefitBatch reflects the activity rows written after the batch
// started. It deliberately uses the activity store (the durable outcome
// source) instead of inventing per-account summaries inside the scheduler.
func (p *Panel) finishBenefitBatch(kind string, started time.Time, done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(batchRunStaleAfter):
	}
	rec := batchRun{Kind: kind, StartedAt: started, Done: true}
	if p.cfg.Activity == nil {
		rec.Note = "活动记录未启用"
	} else {
		// Append is asynchronous and the persisted file is the durable source.
		// Poll a bounded window so a batch does not appear complete before the
		// final account's activity row has been flushed.
		deadline := time.Now().Add(5 * time.Second)
		for {
			res, err := p.cfg.Activity.Query(activity.Filter{Since: started, Limit: 1000})
			if err != nil {
				rec.Note = "读取活动记录失败"
				break
			}
			rec.Total = len(res.Records)
			rec.OK, rec.Failed = 0, 0
			for _, row := range res.Records {
				if row.OK {
					rec.OK++
				} else {
					rec.Failed++
				}
			}
			if rec.Total > 0 || time.Now().After(deadline) {
				if res.Total > len(res.Records) {
					rec.Note = "只统计最近 1000 条活动记录"
				}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	p.batchMu.Lock()
	p.batchRuns[kind] = &rec
	p.batchMu.Unlock()
}

// benefitsBatches returns the latest manual batch state for the welfare page.
// It never starts work or calls upstream.
func (p *Panel) benefitsBatches(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	out := make([]batchRun, 0, 6)
	p.batchMu.Lock()
	for _, rec := range p.batchRuns {
		if rec == nil {
			continue
		}
		cp := *rec
		// A process killed mid-run can leave running=true forever; present it
		// as a completed unknown result rather than a spinner that never stops.
		if cp.Running && now.Sub(cp.StartedAt) > batchRunStaleAfter {
			cp.Running = false
			cp.Done = true
			cp.Note = "执行状态未知（可能已重启）"
		}
		out = append(out, cp)
	}
	p.batchMu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runs": out})
}

// benefitGrowth 单账号成长福利快照（Hub fetch_growth_summary 同口径）。
type benefitGrowth struct {
	StreakDays     int
	MakeupCards    int
	LotteryChances int
	Energy         int64
	CatState       string
	CatReward      int64
	CatDailyDone   bool
	Tier7d         string
	Tier14d        string
	Tier28d        string
	Tasks          []upstream.Task
	TasksErr       string
	Err            string
}

// growthResult 把单账号多个成长查询收敛成一个结果，使调用方可以给整段
// 查询加 context 截止，而不是逐个接口无限等待。任一接口失败时保留其余字段，
// 与 Hub 的逐段容错一致。
func (p *Panel) growthResult(ctx context.Context, a *auth.Auth) benefitGrowth {
	type result struct {
		g benefitGrowth
	}
	done := make(chan result, 1)
	go func() {
		var g benefitGrowth
		if full, err := p.cfg.Upstream.GrowthStreakFull(a); err == nil {
			g.StreakDays = full.Streak.Days
			g.MakeupCards = full.MakeupCards.Balance
			g.Tier7d = full.RedemptionStatus.Tier7dStatus
			g.Tier14d = full.RedemptionStatus.Tier14dStatus
			g.Tier28d = full.RedemptionStatus.Tier28dStatus
		} else {
			g.Err = err.Error()
		}
		if n, err := p.cfg.Upstream.LotteryChances(a); err == nil {
			g.LotteryChances = n
		} else if g.Err == "" {
			g.Err = err.Error()
		}
		if n, err := p.cfg.Upstream.GrowthEnergy(a); err == nil {
			g.Energy = n
		} else if g.Err == "" {
			g.Err = err.Error()
		}
		if tr, err := p.cfg.Upstream.TravelStatus(a); err == nil && tr != nil {
			g.CatState = tr.State
			g.CatReward = tr.RewardCredit
			g.CatDailyDone = tr.DailyLimitReached
		} else if err != nil && g.Err == "" {
			g.Err = err.Error()
		}
		// 成长任务列表与汇总共用同一账号快照：默认口径为全量，mp 口径只补
		// 专属任务。两路都失败时才把任务明细标错；一路成功仍展示已有任务，
		// 与 Hub fetch_growth_tasks 的宽松口径一致。
		tasks, tasksErr := p.cfg.Upstream.ListTasksContext(ctx, a)
		if tasksErr == nil {
			mpTasks, mpErr := p.cfg.Upstream.ListTasksMPContext(ctx, a)
			if mpErr == nil {
				seen := make(map[string]bool, len(tasks))
				for _, t := range tasks {
					seen[t.TaskCode] = true
				}
				for _, t := range mpTasks {
					if !seen[t.TaskCode] {
						tasks = append(tasks, t)
						seen[t.TaskCode] = true
					}
				}
			}
			g.Tasks = tasks
		} else {
			g.TasksErr = tasksErr.Error()
			if g.Err == "" {
				g.Err = tasksErr.Error()
			}
		}
		done <- result{g: g}
	}()
	select {
	case r := <-done:
		return r.g
	case <-ctx.Done():
		return benefitGrowth{Err: "growth query timed out"}
	}
}

// benefitsGrowthSnapshot 并发拉取国内个人号的成长福利详情。只读上游；
// 企业号、国际号不参与（Hub 同一门控）。错误按账号记录，不写活动历史——
// 这是展示查询，不是一次福利动作。
func (p *Panel) benefitsGrowthSnapshot(statuses []pool.Status) map[string]benefitGrowth {
	type job struct {
		uid string
		a   *auth.Auth
	}
	jobs := make([]job, 0, len(statuses))
	for _, st := range statuses {
		if st.Enterprise || st.Realm == "global" || st.Disabled || p.cfg.Upstream == nil {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
			jobs = append(jobs, job{uid: st.UID, a: a})
		}
	}
	out := make(map[string]benefitGrowth, len(jobs))
	if len(jobs) == 0 {
		return out
	}
	var mu sync.Mutex
	p.runBackgroundBounded(4, len(jobs), func(i int) {
		j := jobs[i]
		ctx, cancel := context.WithTimeout(context.Background(), benefitsGrowthTimeout)
		defer cancel()
		g := p.loadBenefitGrowth(ctx, j.a)
		mu.Lock()
		out[j.uid] = g
		mu.Unlock()
	})
	return out
}

// loadBenefitGrowth 拉取一个账号的成长福利；各接口相互独立，任一失败不阻塞
// 其余字段（与 Hub fetch_growth_summary 的逐段 try 行为一致）。
func (p *Panel) loadBenefitGrowth(ctx context.Context, a *auth.Auth) benefitGrowth {
	if ctx == nil {
		ctx = context.Background()
	}
	return p.growthResult(ctx, a)
}

// statusUID8 与活动历史同一脱敏口径。完整 UID 只在服务端内部使用。
func statusUID8(uid string) string {
	uid = strings.TrimSpace(uid)
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

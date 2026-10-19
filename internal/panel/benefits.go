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

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"summary":  summary,
		"accounts": out,
	})
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

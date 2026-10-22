package panel

// mppacer.go 小程序口径成长任务的「真人节奏」事件调度器。
//
// 背景（实测，见 autotask.go 的 mpChatEventGap 注释）：Sequential_Tasks_3/6
// 等任务要求「小程序内 N 次有效对话」，判据是 mini 指纹 chat_request_send
// 事件；数秒级连发的事件会被上游反作弊计入进度后又整体回滚（claim 返回
// 400 task not completed），只有 45s 左右间隔逐条上报才稳定存活。
//
// 于是这类任务天然要跑 4~8 分钟。原先实现把这段等待塞在请求/队列 worker 里：
//   - 单账号「一键做任务」必然超过 5 分钟 HTTP 上限 → 恒 504，用户看到失败；
//   - 期间账号锁一直被占，重复点击被 409，其它账号的队列 worker 也被睡住。
//
// 这里把「等待」从执行栈里摘出来：任务动作只负责登记「还差几条」，由本调度器
// 按节奏逐条发送（每条之间不持有账号锁），达标后自动领奖。请求立即返回，
// 进度通过任务列表与活动记录可见。

import (
	"fmt"
	"log"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// mpPaceTick 调度器心跳：决定「最早可发时刻」的分辨率，与事件间隔无关。
var mpPaceTick = time.Second

// mpPaceMaxJobs 单账号同一时刻只保留一个 mp 任务作业（键含 code，故多码并行由
// 上游反作弊风险兜住，这里额外限总作业数，避免异常堆积）。
const mpPaceMaxJobs = 64

// mpPaceJob 一个待完成的小程序口径事件链。
type mpPaceJob struct {
	UID            string
	Code           string
	WithActivityID bool
	Next           time.Time // 下一条事件最早发送时刻
	Sent           int       // 本作业已发送条数（仅用于日志与终端文案）
	LastErr        string
}

type mpPaceState struct {
	mu      sync.Mutex
	jobs    map[string]*mpPaceJob // key = uid + "\x00" + code
	running bool
	stop    chan struct{}
	done    chan struct{}
}

func (p *Panel) mpPace() *mpPaceState {
	p.mpPaceOnce.Do(func() {
		p.mp = &mpPaceState{jobs: map[string]*mpPaceJob{}}
	})
	return p.mp
}

func mpJobKey(uid, code string) string { return uid + "\x00" + code }

// enqueueMPPace 登记一条 mp 事件链作业。已在队列中则只刷新目标，不重复登记。
// 返回 true 表示已登记（调用方可直接返回「排队中」文案）。
func (p *Panel) enqueueMPPace(a *auth.Auth, code string, withActivityID bool) bool {
	if a == nil || a.UID == "" || p.cfg.Upstream == nil {
		return false
	}
	st := p.mpPace()
	st.mu.Lock()
	if len(st.jobs) >= mpPaceMaxJobs {
		st.mu.Unlock()
		return false
	}
	key := mpJobKey(a.UID, code)
	if job, ok := st.jobs[key]; ok {
		job.WithActivityID = withActivityID
		st.mu.Unlock()
		return true
	}
	// 首条也等一个间隔：上一轮残留进度被反作弊回滚后立即重报同样无效（实测）。
	st.jobs[key] = &mpPaceJob{
		UID:            a.UID,
		Code:           code,
		WithActivityID: withActivityID,
		Next:           time.Now().Add(mpChatEventGap + time.Duration(rand.Int64N(int64(10*time.Second)))),
	}
	needStart := !st.running
	if needStart {
		st.running = true
		st.stop = make(chan struct{})
		st.done = make(chan struct{})
	}
	stop, done := st.stop, st.done
	st.mu.Unlock()

	if needStart {
		if !p.GoBackground(func() { p.mpPaceLoop(st, stop, done) }) {
			st.mu.Lock()
			st.running = false
			st.mu.Unlock()
			close(done)
			return false
		}
	}
	return true
}

// mpPaceSnapshot 返回某账号待完成的 mp 作业（只读快照，供面板展示）。
func (p *Panel) mpPaceSnapshot(uid string) []mpPaceJob {
	st := p.mpPace()
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]mpPaceJob, 0, 2)
	for _, job := range st.jobs {
		if job.UID == uid {
			out = append(out, *job)
		}
	}
	return out
}

// mpPacePending 报告是否还有该账号的 mp 作业在排队（任务动作据此提示「稍后自动继续」）。
func (p *Panel) mpPacePending(uid, code string) bool {
	st := p.mpPace()
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.jobs[mpJobKey(uid, code)]
	return ok
}

func (p *Panel) mpPaceLoop(st *mpPaceState, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(mpPaceTick)
	defer t.Stop()
	// bgCtx 是停机信号：StopWaiting 会先取消它再等 bgWG，本循环必须能被它唤醒，
	// 否则停机要等到下一次 tick（最长 1s，但语义上应当立即退出）。
	var bgDone <-chan struct{}
	if p.bgCtx != nil {
		bgDone = p.bgCtx.Done()
	}
	for {
		select {
		case <-stop:
			return
		case <-bgDone:
			return
		case <-t.C:
		}
		p.mpPaceRunDue(st)
	}
}

// mpPaceRunDue 处理所有到点的作业：每条作业只发一条事件（不持锁等待）。
func (p *Panel) mpPaceRunDue(st *mpPaceState) {
	now := time.Now()
	st.mu.Lock()
	due := make([]*mpPaceJob, 0, len(st.jobs))
	for _, job := range st.jobs {
		if !job.Next.After(now) {
			due = append(due, job)
		}
	}
	st.mu.Unlock()

	for _, job := range due {
		if p.bgCtx != nil && p.bgCtx.Err() != nil {
			return
		}
		a := p.cfg.Pool.AuthByUID(job.UID)
		if a == nil {
			p.dropMPJob(job.UID, job.Code, "账号已不在池中")
			continue
		}
		// 账号被别的动作占用：跳过这一拍，下一拍再试（不阻塞调度器）。
		if !p.tryLockAccount(job.UID) {
			continue
		}
		p.mpPaceStep(a, job)
		p.unlockAccount(job.UID)
	}
}

// mpPaceStep 在持有账号锁的前提下推进一条作业一步：回读 → 达标领奖 / 发一条事件。
func (p *Panel) mpPaceStep(a *auth.Auth, job *mpPaceJob) {
	task, err := p.taskByCodeFresh(a, job.Code)
	if err != nil {
		p.rescheduleMPJob(job, "查询失败: "+err.Error())
		return
	}
	if task == nil {
		p.dropMPJob(job.UID, job.Code, "上游不再下发该任务")
		return
	}
	target := task.Target
	if target <= 0 {
		target = 1
	}
	if task.Claimed {
		p.finishMPJob(a, job, "已领取")
		return
	}
	if task.Current >= target || task.Claimable || task.AcceptStatus == "completed" {
		credit, energy, cerr := p.cfg.Upstream.ClaimRewardMP(a, job.Code)
		p.invalidateTaskSnap(a.UID)
		if cerr != nil {
			p.rescheduleMPJob(job, "领奖失败: "+cerr.Error())
			return
		}
		msg := fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy)
		p.finishMPJob(a, job, msg)
		return
	}
	// 还没 accept 生效：补一次 accept 再等下一拍。
	if task.AcceptStatus == "not_accepted" || task.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, job.Code) {
			p.rescheduleMPJob(job, "accept 未登记生效")
			return
		}
		p.rescheduleMPJob(job, "")
		return
	}
	conv := fmt.Sprintf("wb2api-mp-%d-%d", time.Now().UnixMilli(), job.Sent)
	var ev map[string]any
	if job.WithActivityID {
		ev = upstream.SchoolSeasonChatEvent(conv)
	} else {
		ev = upstream.SchoolChatTimesEvents(conv)
	}
	if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
		p.rescheduleMPJob(job, "上报失败: "+err.Error())
		return
	}
	job.Sent++
	job.LastErr = ""
	p.invalidateTaskSnap(a.UID)
	log.Printf("panel: mp 事件链 %s %s 已发 %d 条（进度 %d/%d）",
		logfmt.Label(a.UID, a.NicknameValue()), job.Code, job.Sent, task.Current, target)
	p.rescheduleMPJob(job, "")
}

// rescheduleMPJob 把作业推迟到下一个人工节奏间隔（带 0~10s 抖动）。
func (p *Panel) rescheduleMPJob(job *mpPaceJob, note string) {
	st := p.mpPace()
	st.mu.Lock()
	job.LastErr = note
	job.Next = time.Now().Add(mpChatEventGap + time.Duration(rand.Int64N(int64(10*time.Second))))
	st.mu.Unlock()
}

// finishMPJob 记录活动历史并移除作业。
func (p *Panel) finishMPJob(a *auth.Auth, job *mpPaceJob, msg string) {
	p.dropMPJob(job.UID, job.Code, msg)
	p.recordManualActivity(a, activity.TaskGrowth, true, job.Code+"："+msg)
	log.Printf("panel: mp 事件链完成 %s %s: %s", logfmt.Label(a.UID, a.NicknameValue()), job.Code, msg)
}

// dropMPJob 移除作业；msg 非空时作为结束原因记入日志。
func (p *Panel) dropMPJob(uid, code, msg string) {
	st := p.mpPace()
	st.mu.Lock()
	delete(st.jobs, mpJobKey(uid, code))
	st.mu.Unlock()
	if msg != "" {
		log.Printf("panel: mp 事件链结束 uid=%s code=%s: %s", logfmt.UID8(uid), code, msg)
	}
}

// stopMPPace 停掉调度器（停机收尾用）。
func (p *Panel) stopMPPace() {
	st := p.mpPace()
	st.mu.Lock()
	if !st.running {
		st.mu.Unlock()
		return
	}
	st.running = false
	stop, done := st.stop, st.done
	st.mu.Unlock()
	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

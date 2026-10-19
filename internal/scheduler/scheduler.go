// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	BlackcatHours  []int // 默认 [23]：夜猫子（23:00–08:00 计数窗口）
	GrowthHours    []int // 默认 [1]：成长任务队列（Sequential 族每日零点解锁一环，
	// 01:00 自动扫描+执行；避开零点整防解锁竞态）

	// ExpiringSoonWindow 快过期积分窗口：签到/余额刷新查余额时，把到期时间
	// <= now+window 的套餐余额标记为"快过期"（pool 据此做最早到期优先，见
	// entry.creditsEarliestExpiry）。<=0 时不做路由门槛；展示仍使用完整逐包数据。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled 显式关闭夜猫子排程（schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled 显式关闭成长任务自动排程（schedule.growth_enabled=false）。
	GrowthDisabled bool

	// IncludeDisabledInTasks 让「保号类」四任务（签到 / 活跃上报 / token 保活 / 余额刷新）
	// 对已禁用（disabled）的账号也执行（schedule.include_disabled_in_tasks）。
	// 缺省 false = 保持「禁用的跳过」既有语义；打开后禁用号照常签到保号，但**仍不参与
	// 选号**——pool.pick 侧的 disabled 过滤与本开关无关。
	IncludeDisabledInTasks bool

	// GrowthHook 成长任务队列执行回调（panel.RunGrowthQueueOnce：扫描全部账号
	// 待办并执行，与面板「执行全部待办」按钮同管线）。调度器只管时点不管实现——
	// panel 在 scheduler 之后构造，用 SetGrowthHook 事后挂载；nil 时到点跳过。
	GrowthHook func()

	// ActivityPath 账号每日活动历史 JSONL 路径（可选）。为空且 Activity 为 nil 时
	// 埋点整体关闭，不影响任何任务行为。构造时注入，调度器内部只追加、不读。
	ActivityPath string
	// Activity 可选的高级覆盖（测试/外部已有 Store 时直接注入）。nil 且
	// ActivityPath 非空时 New 会按路径构造。写失败一律静默，不反噬主任务。
	Activity *activity.Store
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// schedMu 保护排程参数（时点/开关）；Reconfigure 可在运行期热改（面板保存配置时调用）。
	// rearmSchedule/rearmBalance 是「排程已变，立即重算」通知：Run 与余额刷新循环各自消费，
	// 分别用独立 channel（同 channel 被两个 select 消费会丢信号）。
	schedMu       sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance  chan struct{}

	// balanceInterval 余额刷新间隔（纳秒，0=暂停）。atomic 读写：执行循环每轮读当前值，
	// SetBalanceInterval 可任意时刻热改（面板保存配置）。
	balanceInterval atomic.Int64

	// batchMu/batchRunning 批量任务 per-kind 单飞闸门（唯一权威）：手动（面板
	// 「一键全部」）与定时（runBatch）共用同一把锁。到点时若同类批已在跑则跳过，
	// 否则两路会并发遍历同一批账号——本意是防风控，双跑既浪费上游请求也可能触发风控。
	// 键为稳定的 kind 字符串（checkin/travel/activity/keepalive/blackcat）。
	batchMu      sync.Mutex
	batchRunning map[string]bool
	batchWG      sync.WaitGroup
	// refreshWG 跟踪余额刷新 ticker（含进行中的一轮）。停机时先等它退出，
	// 再关闭下游活动历史/用量存储。
	refreshWG sync.WaitGroup
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.Activity == nil && strings.TrimSpace(cfg.ActivityPath) != "" {
		cfg.Activity = activity.New(activity.Config{Path: cfg.ActivityPath})
	}
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance:  make(chan struct{}, 1),
	}
}

// ExpiringSoonWindow 返回当前快过期路由窗口（读取时与热配置写在 schedMu 下同步）。
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow 热更新快过期路由窗口。窗口变化时清空池内旧快照，避免在下一轮
// 余额刷新覆盖前，继续用旧窗口得出的最早到期顺序选号。
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure 热更新排程参数（面板保存配置后调用）：改时点/开关并通知运行中的循环重算。
// 空 hours 视为「未配置」保留原值（与 config.normalize 的回落语义一致）。
// SetGrowthHook 挂载/替换成长任务队列回调（panel 构造晚于 scheduler，事后接线）。
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

// includeDisabledInTasks 返回保号类任务是否应覆盖禁用账号（配置快照，供循环开头取一次）。
func (s *Scheduler) includeDisabledInTasks() bool {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.IncludeDisabledInTasks
}

// SetIncludeDisabledInTasks 热更新「保号类任务是否覆盖禁用账号」
// （schedule.include_disabled_in_tasks）。
// 用独立 setter 而非并入 Reconfigure：后者已有 12 个位置参数，继续追加会让调用点难以
// 校对；本开关语义独立（只影响账号过滤，不影响时点），单独设值更清晰。
func (s *Scheduler) SetIncludeDisabledInTasks(v bool) {
	s.schedMu.Lock()
	s.cfg.IncludeDisabledInTasks = v
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke 非阻塞发一次唤醒信号（已有待处理信号则忽略，语义等价）。
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
// 排程参数在 schedMu 下快照，与 Reconfigure 的并发写隔离。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置。测试可缩短（与
// travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限
// 睡满，本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的三态结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotRearm                  // 排程已变（Reconfigure）：上层重算下一次唤醒
	slotCancel                 // ctx 取消：上层优雅退出
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑）。
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段
// 末尾必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
//
// ctx 取消 / rearmSchedule（在线改配置重排）在每段的 select 里随时返回，段长
// 不影响两者响应性。返回三态见 slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
// Reconfigure 触发 rearmSchedule 时提前唤醒重算（新时点/开关立即生效）。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 四类任务全部禁用：不空转，等重排通知（在线改配置重新启用）或退出信号。
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // 排程已变：重算下一次唤醒
		case slotFired:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 多号 × 间隔 ≈ 数分钟睡眠）不再阻塞同槽其他任务族；返回前等全部
			// 任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠的风险与
			// 串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// ctx 取消时由各任务内部的可取消等待快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			switch k {
			case taskCheckin:
				s.tryRunBatchKind("checkin", func() { s.runCheckin(activity.TriggerSchedule) })
			case taskTravel:
				s.tryRunBatchKind("travel", func() { s.runTravel(activity.TriggerSchedule) })
			case taskActivity:
				s.tryRunBatchKind("activity", func() { s.runActivity(ctx, activity.TriggerSchedule) })
			case taskKeepalive:
				s.tryRunBatchKind("keepalive", func() { s.runKeepalive(activity.TriggerSchedule) })
			case taskBlackcat:
				s.tryRunBatchKind("blackcat", s.RunBlackcatNow)
			case taskGrowth:
				// 成长任务队列：回调在 panel 侧异步启动（返回不等执行完），nil 未挂载则跳过。
				s.schedMu.Lock()
				hook := s.cfg.GrowthHook
				s.schedMu.Unlock()
				if hook != nil {
					hook()
				}
			}
		}(k)
	}
	wg.Wait()
}

// TryRunBatch 申请执行一次 per-kind 批量任务（面板「一键全部」入口）：
// 同一 kind 已在跑（手动或定时）时返回 false，调用方据此回 409；否则置位、
// 后台 goroutine 执行 fn，结束清位。闸门在 Scheduler 侧统一持有，确保手动与
// 定时两路不会并发遍历同一批账号。
func (s *Scheduler) TryRunBatch(kind string, fn func()) bool {
	_, ok := s.TryRunBatchDone(kind, fn)
	return ok
}

// TryRunBatchDone 与 TryRunBatch 相同，但额外返回一个在 fn 返回后关闭的
// done 通道。调用方（面板批量结果）据此只等待自己启动的这一轮，而不是
// 全局 WaitBatches——后者会在并发 Add/Wait 时产生竞态，也会被后启动的
// 其它 kind 拖住。
func (s *Scheduler) TryRunBatchDone(kind string, fn func()) (<-chan struct{}, bool) {
	if !s.beginBatchKind(kind) {
		return nil, false
	}
	s.batchWG.Add(1)
	done := make(chan struct{})
	go func() {
		// LIFO：先清 kind 标记、再减 WaitGroup、最后关闭 done。done 关闭时
		// 本轮闸门已经释放，面板可以立即轮询批次状态。
		defer close(done)
		defer s.batchWG.Done()
		defer s.endBatchKind(kind)
		fn()
	}()
	return done, true
}

// WaitBatches 等待所有通过 TryRunBatch 启动的后台批量任务结束（含签到/旅行/
// 活跃/保活/余额）。仅供停机排空使用；定时 runBatch 本身是同步等待，不在其内。
func (s *Scheduler) WaitBatches() {
	s.batchWG.Wait()
}

// tryRunBatchKind 定时路径的同步变体：在**当前 goroutine** 内申请并执行，
// 已在跑则跳过（runBatch 需等待本批收尾，不能像 TryRunBatch 那样另起 goroutine）。
func (s *Scheduler) tryRunBatchKind(kind string, fn func()) {
	if !s.beginBatchKind(kind) {
		return
	}
	defer s.endBatchKind(kind)
	fn()
}

// beginBatchKind 尝试置位某类批量任务的运行标记；已在跑返回 false。
func (s *Scheduler) beginBatchKind(kind string) bool {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	if s.batchRunning == nil {
		s.batchRunning = make(map[string]bool)
	}
	if s.batchRunning[kind] {
		return false
	}
	s.batchRunning[kind] = true
	return true
}

// endBatchKind 清除某类批量任务的运行标记。
func (s *Scheduler) endBatchKind(kind string) {
	s.batchMu.Lock()
	delete(s.batchRunning, kind)
	s.batchMu.Unlock()
}

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等限速睡醒），
// 等满返回 true。d<=0 立即放行。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// recordActivity 写一条账号活动历史。它是纯观测旁路：nil Store、写失败都静默，
// 绝不改变签到/活跃/旅行/保活任一任务的返回值、日志或上游调用次序。
func (s *Scheduler) recordActivity(a *auth.Auth, task activity.Task, trigger activity.Trigger, ok bool, message string) {
	if s == nil || s.cfg.Activity == nil || a == nil {
		return
	}
	_ = s.cfg.Activity.Append(activity.Record{
		UID:      a.UID,
		Nickname: a.Nickname,
		Realm:    a.Realm(),
		Task:     task,
		Trigger:  trigger,
		OK:       ok,
		Message:  message,
	})
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用账号默认跳过——
// 若 schedule.include_disabled_in_tasks 打开则一并执行（禁用只关选号，不停保号）。
// 旅行已从签到剥离为独立排程（travel_hours），不再搭签到便车。
// 末尾追加连登管家（streak.go）：可兑换档位自动兑换 + 抽奖次数自动抽完——
// 连登兑换按天数解锁，挂在每日签到后即「到天数那天自动完成兑换→抽奖闭环」。
func (s *Scheduler) RunCheckinNow() {
	s.runCheckin(activity.TriggerManual)
}

func (s *Scheduler) runCheckin(trigger activity.Trigger) {
	expiringSoon := s.ExpiringSoonWindow()
	includeDisabled := s.includeDisabledInTasks()
	// TaskAccounts 在一次读锁内取 UID/凭证，不再为每个账号构建完整 Status
	//（成本台账、模型限流行、冷却剩余秒等）。调度任务只需要凭证与少量判定字段，
	// 470 账号时 List+AuthByUID 的整表投影与 N 次加锁都是固定浪费。
	for _, acct := range s.cfg.Pool.TaskAccounts(includeDisabled) {
		uid, a := acct.UID, acct.Auth
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 门控：realm=global 账号无签到体系，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			continue
		}
		// 企业版无签到体系：上游对 POST /v2/billing/meter/daily-checkin 直接
		// 400 code 10001「企业账号不支持该操作」。跳过签到，但**不 continue**——
		// 下方余额查询照常跑（企业额度走 get-enterprise-user-usage 口径，且是
		// credit_floor 判定与面板额度展示的唯一数据源）。
		if !a.IsEnterprise() {
			if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
				// "今天已签到"是幂等成功（上游对重复签到返回 code!=0），不再当失败打 error 行。
				if upstream.IsAlreadyCheckin(err) {
					s.cfg.Pool.NoteCheckinDone(uid)
					log.Printf("checkin %s: 今天已签到（幂等）", logfmt.Label(uid, a.Nickname))
					s.recordActivity(a, activity.TaskCheckin, trigger, true, "今天已签到（幂等）")
				} else {
					log.Printf("checkin %s: %v", logfmt.Label(uid, a.Nickname), err)
					s.recordActivity(a, activity.TaskCheckin, trigger, false, err.Error())
				}
				// 其余业务错误也继续走余额查询
			} else {
				// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
				// 重复触发时出现），成功也落一行。
				s.cfg.Pool.NoteCheckinDone(uid)
				log.Printf("checkin %s: 签到成功", logfmt.Label(uid, a.Nickname))
				s.recordActivity(a, activity.TaskCheckin, trigger, true, "签到成功")
			}
		}
		// 分桶查余额：配置窗口内的积分单独标记，同时记录最早未来到期批次。
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(uid, a.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(uid, remain, total)
		s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	}
	s.RunStreakBonusNow()
}

// RunActivityNow 立即对池内所有可用账号执行一次对话活跃上报。
// 禁用账号默认跳过（schedule.include_disabled_in_tasks 打开时一并上报）；
// 无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上
// code=0 OK，点亮连登）；一条上报同时点亮 growth 连登 + 解锁 first_buddy 任务。
// 上报成功后续跑 streak 自检（checkActivityStreak）：回读连登天数，发现
// 「上报 200 但 streak 没涨」的静默丢弃（只读 oracle，不做重试）。
// RunActivityNow 是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走
// runActivity（ctx 取消时立即放弃剩余账号，不等限速睡满）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background(), activity.TriggerManual)
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context, trigger activity.Trigger) {
	first := true
	includeDisabled := s.includeDisabledInTasks()
	for _, acct := range s.cfg.Pool.TaskAccounts(includeDisabled) {
		a := acct.Auth
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// 企业版跳过活跃上报：/v2/report 本身返回 200（不报错），但它唯一的作用是
		// 点亮 growth 连登天数 / 解锁 first_buddy 领养——这两者对企业号都是 403
		// （「growth system is only available for personal users」），即**零收益**。
		// 该类"静默无效"最易被忽略（日志无异常），故显式跳过，不给上游多发一次请求。
		if a.IsEnterprise() {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切，
		// 无需改动 upstream。此处曾按「D4 门控：global 无活跃体系」跳过 global，实测该
		// 判断不成立——国际版 /v2/report 可用，跳过即国际版账号永远点不亮连登（上游
		// a190252 同口径修复）。checkin/travel 的 global 门控不受影响，仍跳过。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			s.recordActivity(a, activity.TaskActivity, trigger, false, err.Error())
			continue
		}
		s.checkActivityStreak(a) // 上报成功 → 回读 streak 自检
		s.recordActivity(a, activity.TaskActivity, trigger, true, "上报成功")
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 禁用账号默认跳过；若 schedule.include_disabled_in_tasks 打开，禁用号也会续期 token
// （这是轮换用法下把闲置号保持可用的关键），但**不再对已禁用的号重复计数 12153**——
// 它已经是终态，再计一次只会打出一行「— 禁用」的误导日志。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	s.runKeepalive(activity.TriggerManual)
}

func (s *Scheduler) runKeepalive(trigger activity.Trigger) {
	includeDisabled := s.includeDisabledInTasks()
	for _, acct := range s.cfg.Pool.TaskAccounts(includeDisabled) {
		uid, a := acct.UID, acct.Auth
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(uid, a.Nickname), err)
			s.recordActivity(a, activity.TaskKeepalive, trigger, false, err.Error())
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(uid) {
					log.Printf("keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(uid, a.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(uid) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(uid, a.Nickname), err)
			s.recordActivity(a, activity.TaskKeepalive, trigger, false, "token 刷新成功，写回失败: "+err.Error())
			continue
		}
		s.recordActivity(a, activity.TaskKeepalive, trigger, true, "token 刷新成功")
	}
}

// RunBalanceRefreshNow 并发查询余额并更新池内 credits。
// 账号范围：默认跳过禁用账号；schedule.include_disabled_in_tasks 打开时一并刷新
// （轮换用法下据此判断下一个该启用谁）。注意 ReenableIfCredits 对 disabled 是
// no-op，所以本开关**不会**导致禁用号被自动解冻——它只让已有账号的积分保持新鲜。
// 解冻语义与签到一致（ReenableIfCredits：余额 > 0 的冷却账号自动解冻），
// 但不做签到、不刷新 token——只让"积分"这个观测量保持新鲜。
// 供两类入口复用：后台周期任务（StartBalanceRefresh）与面板手动全量刷新。
//
// 单飞：手动（面板「刷新余额」）与定时（balance ticker）两路共用同一把 kind="balance"
// 闸门，互斥执行——否则同一批账号会被两路并发扫一遍上游（余额是分钟级观测量，重复
// 扫描纯浪费并放大风控面）。已在跑时后到者直接返回 false（本次跳过），不报错——
// 余额刷新是幂等观测，错过一轮下一轮自然补上。返回 true 表示本次确实执行了刷新。
func (s *Scheduler) RunBalanceRefreshNow() bool {
	if !s.beginBatchKind("balance") {
		return false
	}
	defer s.endBatchKind("balance")
	expiringSoon := s.ExpiringSoonWindow()
	accts := s.cfg.Pool.TaskAccounts(s.includeDisabledInTasks())
	// 固定 3 个 worker（与 panel 任务中心各批量扫描口径一致）：旧实现为每个账号
	// 起一个 goroutine 再争抢信号量，470 号时会瞬时堆积同阶 goroutine；这里只保留
	// 与并发上限相同的 worker，吞吐不变、堆峰值与调度抖动更低。
	workers := 3
	if len(accts) < workers {
		workers = len(accts)
	}
	jobs := make(chan pool.TaskAccount)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for acct := range jobs {
				remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(acct.Auth, expiringSoon)
				if err != nil {
					log.Printf("balance %s: %v", logfmt.Label(acct.UID, acct.Auth.Nickname), err)
					continue
				}
				s.cfg.Pool.ReenableIfCredits(acct.UID, remain, total)
				s.cfg.Pool.SetCreditsDetailed(acct.UID, remain, total, expiring, earliestAt, earliestRemaining)
			}
		}()
	}
	for _, acct := range accts {
		jobs <- acct
	}
	close(jobs)
	wg.Wait()
	return true
}

// StartBalanceRefresh 后台周期性余额刷新（独立 ticker goroutine，ctx 取消即停）。
// interval<=0 不启动（schedule.balance_refresh_enabled=false 时 main 不调用即可）。
// 独立于 Run 的小时制排程：余额是分钟级观测量，不值得为它扩展 nextFire 的粒度。
// 运行期可用 SetBalanceInterval 热改间隔（下一轮生效）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	// 先登记再启动：否则极小间隔下 ticker 恰好先于 Add 完成一轮 RunBalanceRefreshNow，
	// WaitBalanceRefresh 可能在 Add 前就返回，停机时仍有刷新在写活动历史。
	s.refreshWG.Add(1)
	go func() {
		defer s.refreshWG.Done()
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: 余额后台刷新每 %s（暂停中显示 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// 被热改暂停：等重排通知（重新启用时唤醒）或退出。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // 间隔已变：立刻按新值重算
			case <-timer.C:
				s.RunBalanceRefreshNow() // 返回值忽略：定时路径本身就期望"跳过即跳过"
			}
		}
	}()
}

// WaitBalanceRefresh 等待余额刷新循环退出（ctx 取消后）。仅停机使用。
func (s *Scheduler) WaitBalanceRefresh() { s.refreshWG.Wait() }

// SetBalanceInterval 热改余额刷新间隔；<=0 表示暂停循环（面板关闭该开关时）。
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}

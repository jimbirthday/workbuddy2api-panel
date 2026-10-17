// taskcenter.go 面板「任务中心」：全账号任务扫描 + 执行队列（可配并发）+
// 成长任务队列。
//
// 语义：
//   - 扫描（scan_all）：并发拉取每账号的成长任务列表（默认+小程序口径），
//     汇总出"未完成且可自动化"的待办清单（只读，不执行）。
//   - 执行队列（run_queue + queue）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁，与单任务/一键完成互斥），账号间并发
//     （concurrency 信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 扫描（只读）
// ---------------------------------------------------------------------------

// scanAccountItem 单账号扫描结果。
type scanAccountItem struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname"`
	Growth    []upstream.Task `json:"growth,omitempty"`
	GrowthErr string          `json:"growth_error,omitempty"`

	// hasErr 仅供汇总 error_count 使用（未导出，不参与 JSON 序列化）：
	// 该账号本扫描内成长任务列表拉取失败或 mp 列表拉取失败。
	hasErr bool
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// 上游锁定的任务不出待办：Sequential 族每日零点解锁一环，刚做完上一环时
	// 下一环以下发但 locked 形态出现在列表里——扫进队列只会 accept 不落账报
	// 失败（每日锁定窗口），零点解锁后自然回到待办。其余 locked（上游未开放）
	// 同语义：不该被自动化尝试。
	if t.Locked {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		// 达标未领：也入队（队列执行后会自动领）——但仅限有自动化动作的任务，
		// 否则队列执行时会因 autoActionFor 为 nil 直接报错。
		return autoActionFor(t.TaskCode) != nil
	}
	return autoActionFor(t.TaskCode) != nil
}

// scanState 全账号扫描的后台任务状态。
//
// 为什么改成异步：账号多时（实测 470 号）扫描要 2 分钟左右——470/3×2 次上游
// 请求，同步 HTTP 在这段时间里没有任何反馈，前端只能显示「扫描中…」，用户会
// 误以为卡死。改成「启动即返回 + 轮询进度」后，面板能看到已扫/总数与当前账号。
//
// 单飞：同一时刻只允许一个扫描。两次点击（或两个人同时点）不应把上游请求
// 翻倍——实测日志里两次扫描相隔 87 秒同时段执行过。
type scanState struct {
	mu      sync.Mutex
	running bool
	started time.Time
	done    int
	total   int
	seq     int
	items   []scanAccountItem
	pending int
	errors  int
	err     string
}

func (p *Panel) scan() *scanState {
	p.scanOnce.Do(func() { p.sc = &scanState{} })
	return p.sc
}

// tasksScanAll 启动全账号扫描（后台执行，立即返回）。
// 重复调用不会启动第二轮：running=true 时返回当前进度（started=false）；
// 空闲时再次调用则开始新一轮（用于用户主动重扫）。
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	st := p.scan()
	st.mu.Lock()
	if st.running {
		// 已在扫描：返回当前状态，前端据 started=false 继续轮询而不重复发起。
		snap := scanSnapshotLocked(st)
		st.mu.Unlock()
		writeJSON(w, http.StatusOK, snap)
		return
	}
	st.running = true
	st.started = time.Now()
	st.done = 0
	st.total = 0
	st.items = nil
	st.pending = 0
	st.errors = 0
	st.err = ""
	st.seq++
	seq := st.seq
	st.mu.Unlock()

	go p.runScan(seq)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "started": true, "running": true, "seq": seq,
	})
}

// tasksScanStatus 返回扫描进度与（完成后的）结果。前端每 500ms 轮询。
func (p *Panel) tasksScanStatus(w http.ResponseWriter, r *http.Request) {
	st := p.scan()
	st.mu.Lock()
	snap := scanSnapshotLocked(st)
	st.mu.Unlock()
	writeJSON(w, http.StatusOK, snap)
}

// scanSnapshotLocked 生成可序列化的进度快照。调用方必须持有 st.mu。
// accounts 只在完成后返回，避免把半成品当结果渲染。
func scanSnapshotLocked(st *scanState) map[string]any {
	out := map[string]any{
		"ok":            true,
		"started_at":    st.started,
		"running":       st.running,
		"done":          st.done,
		"total":         st.total,
		"pending_count": st.pending,
		"error_count":   st.errors,
		"seq":           st.seq,
	}
	if st.err != "" {
		out["error"] = st.err
	}
	if !st.running {
		out["accounts"] = st.items
	}
	return out
}

// runScan 后台扫描全部账号：成长任务（未完成+可自动化，含 mp 口径合并）。
// 只读操作；进度按账号完成数累加，供前端轮询展示。
func (p *Panel) runScan(seq int) {
	// 扫描跑在后台 goroutine：任何 panic 都必须收尾 running，否则单飞标志永久为
	// true，按钮会一直显示扫描中且之后再也无法启动新一轮。
	defer func() {
		if v := recover(); v != nil {
			st := p.scan()
			st.mu.Lock()
			if st.seq == seq {
				st.running = false
				st.err = fmt.Sprintf("扫描异常：%v", v)
			}
			st.mu.Unlock()
			log.Printf("panel: 队列扫描 panic seq=%d: %v", seq, v)
		}
	}()
	states := p.cfg.Pool.List()
	// 先算分母再起 goroutine：否则 3 个并发槽位可能先完成，前端会短暂看到
	// done > total（total 还没写），进度条出现回跳/负值。禁用账号不计入。
	run := 0
	for _, st := range states {
		if !st.Disabled {
			run++
		}
	}
	st := p.scan()
	st.mu.Lock()
	if st.seq != seq {
		st.mu.Unlock()
		return
	}
	st.total = run
	st.mu.Unlock()

	items := make([]scanAccountItem, len(states))
	// sem 限制扫描并发（口径与面板其它读接口一致，concurrency=3）：账号多时
	// 若每账号无上限地开 goroutine，会瞬间打满上游触发风控。
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		fixedI, fixedUID := i, st.UID
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a := p.cfg.Pool.AuthByUID(fixedUID)
			if a == nil {
				p.scanTick(seq, 1)
				return
			}
			it := &items[fixedI]
			it.UID, it.Nickname = fixedUID, a.Nickname
			// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				p.scanTick(seq, 1)
				return
			}
			// 企业版门控：同上——企业版无个人成长体系（GET /v2/activity/growth/tasks
			// 上游 403「growth system is only available for personal users」）。
			if a.IsEnterprise() {
				p.scanTick(seq, 1)
				return
			}
			if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
				it.hasErr = true
			} else {
				for _, t := range tasks {
					if growthPending(t) {
						it.Growth = append(it.Growth, t)
					}
				}
			}
			// 小程序口径任务（school_season 校园日 / Sequential_Tasks_1 小程序首对话）
			// 仅在 mp 头列表下发，与默认口径不重叠——合并进待办列表；mp 列表失败
			// 静默（无 mp 任务的部署/活动结束时零影响）。
			if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
				seen := map[string]bool{}
				for _, t := range it.Growth {
					seen[t.TaskCode] = true
				}
				for _, t := range mpTasks {
					if growthPending(t) && !seen[t.TaskCode] {
						it.Growth = append(it.Growth, t)
					}
				}
			} else {
				// mp 列表失败此前静默；这里只标记该账号本次扫描有查询失败，
				// 计入 error_count，不改变既有字段与前端可见的待办结构。
				it.hasErr = true
			}
			p.scanTick(seq, 1)
		}()
	}

	wg.Wait()

	pending := 0
	errorCount := 0
	for _, it := range items {
		pending += len(it.Growth)
		if it.hasErr {
			errorCount++
		}
	}
	st.mu.Lock()
	if st.seq != seq {
		// 期间被新一轮接管（理论上不会发生：单飞）；放弃旧结果，不覆盖新状态。
		st.mu.Unlock()
		return
	}
	st.items = items
	st.pending = pending
	st.errors = errorCount
	st.running = false
	st.done = st.total
	st.mu.Unlock()
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项，查询失败 %d 个", pending, errorCount)
}

// scanTick 累加一个账号的完成数；seq 不匹配（旧扫描）时忽略。
func (p *Panel) scanTick(seq, n int) {
	st := p.scan()
	st.mu.Lock()
	if st.seq == seq {
		st.done += n
	}
	st.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 执行队列
// ---------------------------------------------------------------------------

// queueItem 队列执行单元。
type queueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind     string `json:"kind"` // growth
	Code     string `json:"code"`
	Progress string `json:"progress,omitempty"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// queueState 队列运行状态。Seq 每次启动 +1——前端只渲染"自己启动的那一轮"，
// 执行结束后的残留 items 不会覆盖后续的扫描结果视图。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

// Panel 队列字段在 Panel 结构体上（panel.go）由 initQueue 惰性初始化；
// 这里集中访问器，避免改动 New 构造链。
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue 启动执行队列：{concurrency:1-4, growth:bool, school:bool}。
// 先做一次扫描，把全部待办项排队（growth 按账号内 autoActions 顺序执行，
// school 逐账号跑闭环），账号内串行、账号间受并发信号量约束。
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	// 限读 1 MiB（同 config.go 口径）：请求体是小结构体，加闸防超大 JSON 内存放大。
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为无可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "队列正在执行中（可在任务中心查看进度）"
	}
	// 先占位：扫描（数秒级网络耗时）期间若并发再次触发，直接命中上面的 running
	// 判拒，避免两个 goroutine 同时启动互相覆盖 q.items/q.seq。无待办时回滚。
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	// 扫描待办（复用扫描逻辑的拉取部分）。
	states := p.cfg.Pool.List()
	// 预扫同样是后台 goroutine：Panic 必须回滚 running，否则队列单飞永久卡住。
	defer func() {
		if v := recover(); v != nil {
			q.mu.Lock()
			q.running = false
			q.startedAt = time.Time{}
			q.mu.Unlock()
			log.Printf("panel: 队列预扫 panic: %v", v)
		}
	}()

	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	// sem 限制预扫并发（口径同上，concurrency=3）：与 tasksScanAll 一致，
	// 避免账号多时无界并发打满上游触发风控。
	sem := make(chan struct{}, 3)
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			one := queueAccount{a: a}
			// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
			if a.IsGlobal() {
				return
			}
			// 企业版门控：同上（企业版无个人成长体系，任务端点上游一律 403）。
			if a.IsEnterprise() {
				return
			}
			if growth {
				if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
					for _, t := range tasks {
						if growthPending(t) {
							one.grow = append(one.grow, t)
						}
					}
					// 合并小程序口径待办（与 tasksScanAll 同口径：mp 列表是默认口径
					// 超集，按 code 去重；失败静默）。此前此处漏合并——扫描显示
					// mp 待办而队列报"无可执行待办"。
					if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
						seen := map[string]bool{}
						for _, t := range one.grow {
							seen[t.TaskCode] = true
						}
						for _, t := range mpTasks {
							if growthPending(t) && !seen[t.TaskCode] {
								one.grow = append(one.grow, t)
							}
						}
					}
					sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
						return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if len(one.grow) > 0 {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()

	// 组装队列（账号分组，保持顺序）。
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{
				UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode,
				Progress: taskProgressText(&t), Status: "pending",
			})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "全部账号没有待办任务"
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, ""
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _ := p.startGrowthQueue(1, true)
	if started {
		log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
	}
}

// runQueueItems 队列执行主体：按账号分组，账号内串行（per-account 锁），
// 账号间并发（信号量）。每项结果写回队列状态。
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		if v := recover(); v != nil {
			// 任一账号 worker panic 都不能把进程带崩，也不能让队列永久停在
			// running=true：标出失败项并释放单飞闸门，面板可继续重试。
			q.mu.Lock()
			for i := range q.items {
				if q.items[i].Status == "pending" || q.items[i].Status == "running" {
					q.items[i].Status = "error"
					q.items[i].Message = fmt.Sprintf("执行异常：%v", v)
				}
			}
			q.running = false
			q.mu.Unlock()
			log.Printf("panel: 队列执行 panic: %v", v)
			return
		}
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					log.Printf("panel: 队列账号 worker panic uid=%s: %v", one.a.UID, v)
					p.queueSet(q, one.a.UID, func(it *queueItem) {
						if it.Status == "pending" || it.Status == "running" {
							it.Status, it.Message = "error", fmt.Sprintf("执行异常：%v", v)
						}
					})
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account 互斥：与单任务/一键完成共用一把锁。
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// 前置：批量接受尚未接受的任务。上游对 not_accepted 的任务不计数——
			// 面板「一键完成」一直有这步，队列路径此前漏了（表现为上报 200 但进度
			// 一直 not_accepted、无法领奖）。失败不阻塞（行为事件才是进度判据）。
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // 给上游状态流转留时间
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // 项间节流
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目三元组（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt 按索引更新队列条目状态（条目数组固定不再增删）。
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet 按 uid 批量改状态。
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks 批量接受该账号未接受的任务，返回接受的个数（失败返回 0 不阻塞）。
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", a.UID, err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖；与
// accountTaskAuto 同语义，结果以文字返回）。
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":    q.running,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
	})
}

// schoolVouchers 我的券码：逐 CN 账号查开学季 /vouchers（3 并发，与 packages
// 同款限流），失败只在对应账号标 error。global 账号无开学季，不发上游调用。
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err      string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（无开学季活动）"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}

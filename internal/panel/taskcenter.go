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
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
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
	current string
	items   []scanAccountItem
	// covered 是本轮扫描实际纳入的全部 UID（含 global/企业版等被门控跳过的账号）。
	// 队列复用必须以此为准判断账号集合，而不是从 items 反推（items 只含有任务的号）。
	covered map[string]bool
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
	st.covered = nil
	st.pending = 0
	st.errors = 0
	st.err = ""
	st.current = ""
	st.seq++
	seq := st.seq
	st.mu.Unlock()

	if !p.GoBackground(func() { p.runScan(seq) }) {
		st.mu.Lock()
		st.running = false
		st.err = "server shutting down"
		st.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	}
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
		"elapsed_sec":   int(time.Since(st.started).Seconds()),
		"current_uid":   st.current,
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
	// 扫描只需要 UID/凭证/禁用位；完整 List 会为 470 个账号各构建一次 Status
	// （冷却、限流台账、成本账本、token 用量），而这些字段扫描一个都不用。
	accounts := p.cfg.Pool.TaskAccounts(true)
	// 先算分母再起 goroutine：否则 3 个并发槽位可能先完成，前端会短暂看到
	// done > total（total 还没写），进度条出现回跳/负值。禁用账号不计入。
	run := 0
	for _, acct := range accounts {
		if !acct.Disabled {
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

	// 保持原 List 形状（含禁用账号空槽位）：扫描结果 JSON 与既有前端/测试的
	// 账号顺序、槽位语义不变，同时避免为禁用号发起上游请求。
	items := make([]scanAccountItem, len(accounts))
	// 覆盖集合直接来自本轮实际纳入的非禁用账号，且即使随后 AuthByUID
	// 返回 nil 也保留该 UID：复用队列必须知道「这个号本轮被考虑过」，
	// 不能把空 UID 或「没有待办」误当成账号集合一致。
	covered := make(map[string]bool, run)
	idx := make([]int, 0, run)
	for i, s := range accounts {
		// 显式保留非禁用账号的 UID：复用队列必须知道「这个号本轮被考虑过」，
		// 不能把没有待办的号误当成账号集合不一致。
		if !s.Disabled {
			items[i].UID = s.UID
			covered[s.UID] = true
			idx = append(idx, i)
		}
	}
	// 固定 3 个 worker：账号多时不再为每个账号先起一个 goroutine 再阻塞等
	// 信号量（470 账号会瞬间多出 470 个等待者）。并发上限与原先完全一致。
	if !p.runBackgroundBounded(3, len(idx), func(k int) {
		i := idx[k]
		fixedUID := items[i].UID
		p.scanCurrent(seq, fixedUID)
		a := accounts[i].Auth
		if a == nil {
			p.scanTick(seq, 1)
			return
		}
		it := &items[i]
		it.Nickname = a.Nickname
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
	}) {
		st.mu.Lock()
		if st.seq == seq {
			st.running = false
			st.err = "server shutting down"
		}
		st.mu.Unlock()
		return
	}

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
	st.covered = covered
	st.pending = pending
	st.errors = errorCount
	st.running = false
	st.done = st.total
	st.mu.Unlock()
	p.scanMu.Lock()
	p.scanCacheAt = time.Now()
	p.scanMu.Unlock()
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

// scanCurrent 记录本轮正在处理的账号，供前端显示「正在读取 xxx」。
// 只写进度展示字段，不影响扫描结果或单飞语义。
func (p *Panel) scanCurrent(seq int, uid string) {
	st := p.scan()
	st.mu.Lock()
	if st.seq == seq {
		st.current = uid
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
	mu      sync.Mutex
	running bool
	// phase 区分启动阶段：scan（预扫账号待办）/ exec（执行队列）/ ""（未启动或已结束）。
	// 预扫数分钟没有条目可渲染，前端必须靠它显示「预扫 x/y」，否则只能看到按钮
	// 停在「启动中…」，误以为队列卡死。
	phase     string
	preDone   int
	preTotal  int
	startedAt time.Time
	items     []queueItem
	byUID     map[string][]int
	conc      int
	seq       int
	message   string
	// preWarn 是本轮预扫的非阻塞警告（部分账号任务查询失败，但已有其他待办
	// 可执行）。与 message 区分：message 用于终态失败/无待办，preWarn 在
	// 执行期间持续展示，避免用户把「队列已启动」误读成「全部账号都扫到了」。
	preWarn string
}

// setItemsLocked 替换队列条目并重建 UID 索引。队列条目在启动后不再增删，
// 索引让每个账号 worker 的 queueSet 从「遍历全部条目」降为只碰自己的几项。
// 调用方必须持有 q.mu。
func (q *queueState) setItemsLocked(items []queueItem) {
	q.items = items
	q.byUID = make(map[string][]int, len(items))
	for i := range items {
		q.byUID[items[i].UID] = append(q.byUID[items[i].UID], i)
	}
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
	started, total, seq, msg := p.startGrowthQueueAsync(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		if msg == "server shutting down" {
			writeErr(w, http.StatusServiceUnavailable, msg)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		// 预扫在后台继续：HTTP 立即返回，前端随即轮询 tasks/queue 的
		// phase/pre_done/pre_total。不能在这里等 470 个账号扫完再响应——
		// 反向代理/浏览器连接会先超时，用户只能看到「启动中…」。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "started": true, "total": total, "seq": seq, "phase": "scan",
		})
	}
}

// scanReuseTTL 是「扫描完成 → 立即执行队列」复用扫描结果的时间窗。用户点
// 「扫描待办」看清清单后马上点「执行全部待办」是最常见的操作；470 账号一次
// 扫描本身约 2 分钟，用户看完清单再确认很容易超过 90 秒，因此窗口必须覆盖
// 「扫描耗时 + 短暂查看」，而不是只覆盖几秒。任务状态若期间变化，执行前
// taskByCode 会回读并给出真实结果；复用只是省一轮列表请求，不会跳过执行。
const scanReuseTTL = 10 * time.Minute

// reuseFreshScan 尝试复用最近一次完整扫描的待办。返回账号单元与队列项；
// ok=false 时必须回落到正常预扫。判据刻意保守：
//   - 扫描未出错（任一账号列表失败都可能漏待办）；
//   - 账号集合与当前池完全一致（期间增删账号会改变分母/清单）；
//   - TTL 内且状态仍未 running（避免与新一轮扫描竞争读到半成品）。
func (p *Panel) reuseFreshScan(accounts []pool.TaskAccount) ([]queueAccount, []queueItem, bool) {
	target := make(map[string]bool, len(accounts))
	byUID := make(map[string]*auth.Auth, len(accounts))
	for _, acct := range accounts {
		if !acct.Disabled {
			target[acct.UID] = true
			byUID[acct.UID] = acct.Auth
		}
	}
	p.scanMu.Lock()
	fresh := !p.scanCacheAt.IsZero() && time.Since(p.scanCacheAt) <= scanReuseTTL
	p.scanMu.Unlock()
	if !fresh {
		return nil, nil, false
	}
	st := p.scan()
	st.mu.Lock()
	if st.running || st.err != "" || st.errors != 0 || len(st.covered) == 0 {
		st.mu.Unlock()
		return nil, nil, false
	}
	covered := make(map[string]bool, len(st.covered))
	for uid := range st.covered {
		covered[uid] = true
	}
	itemsSnap := make([]scanAccountItem, len(st.items))
	copy(itemsSnap, st.items)
	// 复制后立刻释放 scanState 锁：下面的 Pool.AuthByUID 会取池读锁，不能与
	// 扫描状态锁形成嵌套；同时快照已不可变，后续账号删除只会让校验失败回落预扫。
	st.mu.Unlock()

	if len(covered) != len(target) {
		return nil, nil, false
	}
	for uid := range covered {
		if !target[uid] {
			return nil, nil, false
		}
	}
	accts := make([]queueAccount, 0, len(itemsSnap))
	var items []queueItem
	for _, it := range itemsSnap {
		// runScan 为禁用账号保留了空 UID 槽位；这些槽位不在 covered 中，也
		// 没有可执行任务。跳过它们，否则 AuthByUID("") == nil 会让整个复用
		// 永远失败，只要池里有一个禁用号就退回全量预扫。
		if it.UID == "" {
			continue
		}
		a := byUID[it.UID]
		if a == nil {
			return nil, nil, false
		}
		grow := append([]upstream.Task(nil), it.Growth...)
		sort.SliceStable(grow, func(i, j int) bool { // 与预扫同一顺序（依赖前置）
			return autoActionIndex(grow[i].TaskCode) < autoActionIndex(grow[j].TaskCode)
		})
		if len(grow) == 0 {
			continue
		}
		accts = append(accts, queueAccount{a: a, grow: grow})
		for _, t := range grow {
			items = append(items, queueItem{
				UID: a.UID, Nickname: a.Nickname, Kind: "growth", Code: t.TaskCode,
				Progress: taskProgressText(&t), Status: "pending",
			})
		}
	}
	return accts, items, true
}

// invalidateScanCache 队列成功启动后使扫描快照失效：队列执行会改变任务状态，
// 下一次启动必须重新预扫，不能把刚做完的待办又排一遍。
func (p *Panel) invalidateScanCache() {
	p.scanMu.Lock()
	p.scanCacheAt = time.Time{}
	p.scanMu.Unlock()
}

// startGrowthQueue 扫描全部账号待办并启动队列（HTTP「执行全部待办」与调度器
// growth 时点共用核心）。返回 (started, total, seq, msg)：seq==-1 表示队列
// 已在执行（冲突）；started=false 时 msg 为无可执行待办的说明。并发夹取
// [1,4]；growth 开关同 HTTP 入参语义。
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	return p.startGrowthQueueMode(concurrency, growth, false)
}

// startGrowthQueueAsync 与 startGrowthQueue 共用同一套单飞/预扫/执行逻辑，
// 但只负责占位并返回，预扫放到 GoBackground 中执行。HTTP 入口使用它，
// 调度器仍可使用同步版本以便在测试和停机路径里等待确定结果。
func (p *Panel) startGrowthQueueAsync(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	return p.startGrowthQueueMode(concurrency, growth, true)
}

func (p *Panel) startGrowthQueueMode(concurrency int, growth bool, async bool) (started bool, total int, seq int, msg string) {
	// 停机闸已关闭：不要再占队列 running 或扫描上游，直接如实返回。放在最前
	// 是为了让「无待办」与「服务正在退出」可区分，且不会留下 running 残留。
	p.bgMu.Lock()
	stopping := p.bgStop
	p.bgMu.Unlock()
	if stopping {
		return false, 0, 0, "server shutting down"
	}
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
	q.phase = "scan"
	q.preDone = 0
	q.preTotal = 0
	q.startedAt = time.Now()
	q.message = ""
	q.preWarn = ""
	// 预扫是异步的：seq 必须在 HTTP 返回前就分配，前端才能用同一个代次
	// 轮询；否则启动响应 seq=0、随后状态 seq=1，页面会误判为「别的队列接管」。
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	if async {
		if !p.GoBackground(func() { p.runGrowthQueueStart(concurrency, growth) }) {
			q.mu.Lock()
			q.running = false
			q.phase = ""
			q.startedAt = time.Time{}
			q.mu.Unlock()
			return false, 0, 0, "server shutting down"
		}
		return true, 0, seq, ""
	}
	return p.runGrowthQueueStart(concurrency, growth)
}

// runGrowthQueueStart 执行预扫并启动队列。调用方必须已经持有 queue.running
// 占位；本函数负责在所有退出路径（无待办、查询失败、停机）释放占位。
func (p *Panel) runGrowthQueueStart(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	q := p.queue()
	// 扫描待办（复用扫描逻辑的拉取部分）。这里只需要非禁用账号的 UID 与凭证：
	// List() 会为 470 个账号逐个构建 Status（冷却/限额/成本/token），预扫期间
	// 完全没有消费这些字段。TaskAccounts 在一次读锁内拍轻量快照。
	accounts := p.cfg.Pool.TaskAccounts(true)
	covered := 0
	for _, acct := range accounts {
		if !acct.Disabled {
			covered++
		}
	}
	q.mu.Lock()
	q.preTotal = covered
	q.mu.Unlock()
	// scanDone 由每个账号 goroutine 在结束时调用（包括门控跳过），让预扫阶段
	// 的进度真实反映「已处理账号数」，而不是只统计有任务的账号。
	scanDone := func() {
		q.mu.Lock()
		q.preDone++
		q.mu.Unlock()
	}
	// 刚完成的全量扫描已经拿到每号的成长+小程序待办；在这几秒内启动队列时
	// 直接复用，避免同一批 470 个账号再把两个列表端点各打一遍。只有「扫描成功
	// 完成、账号集合完全一致、TTL 内」才复用，否则回落到重新预扫。
	if growth {
		if accts, items, ok := p.reuseFreshScan(accounts); ok {
			if len(items) == 0 {
				// 空结果同样代表一次「扫描后状态」；立即失效，避免后续 90 秒内
				// 反复命中旧空快照而看不到刚解锁/刚下发的任务。
				p.invalidateScanCache()
				log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
				q.mu.Lock()
				q.running = false
				q.phase = ""
				// 保留 startedAt：让前端轮询读到本轮 message（无待办/预扫失败），
				// 而不是把已启动的一轮误判成从未启动。
				q.message = "全部账号没有待办任务"
				q.mu.Unlock()
				return false, 0, 0, "全部账号没有待办任务"
			}
			q.mu.Lock()
			q.setItemsLocked(items)
			q.conc = concurrency
			q.phase = "exec"
			q.preDone = 0
			q.preTotal = 0
			q.message = ""
			q.preWarn = ""
			seq = q.seq
			q.mu.Unlock()
			p.invalidateScanCache()
			if !p.GoBackground(func() { p.runQueueItems(accts, items, concurrency) }) {
				q.mu.Lock()
				q.running = false
				q.phase = ""
				q.startedAt = time.Time{}
				q.mu.Unlock()
				return false, 0, 0, "server shutting down"
			}
			log.Printf("panel: 队列启动：%d 项（并发 %d，复用刚完成的扫描）", len(items), concurrency)
			return true, len(items), seq, ""
		}
	}
	// 走到这里说明将在本函数内重新预扫上游。旧 scan_all 快照相对这次预扫已经
	// 过期；先失效，避免本轮无待办时下一次启动又复用更早的非空快照。
	p.invalidateScanCache()
	// 预扫同样是后台 goroutine：Panic 必须回滚 running，否则队列单飞永久卡住。
	defer func() {
		if v := recover(); v != nil {
			q.mu.Lock()
			q.running = false
			q.phase = ""
			q.message = fmt.Sprintf("预扫异常：%v", v)
			q.mu.Unlock()
			log.Printf("panel: 队列预扫 panic: %v", v)
		}
	}()

	var accts []queueAccount
	var mu sync.Mutex
	preErrors := 0
	// 预扫只保留固定 3 个 worker；账号顺序仍按 TaskAccounts 原序继续放入 accts。
	type preJob struct {
		a *auth.Auth
	}
	jobs := make([]preJob, 0, len(accounts))
	for _, acct := range accounts {
		if acct.Disabled {
			continue
		}
		if acct.Auth == nil {
			continue
		}
		jobs = append(jobs, preJob{a: acct.Auth})
	}
	if !p.runBackgroundBounded(3, len(jobs), func(k int) {
		a := jobs[k].a
		defer scanDone()
		one := queueAccount{a: a}
		// D4 门控：global 账号无 CN 成长任务体系，不发起任何上游调用。
		if a.IsGlobal() {
			return
		}
		// 企业版门控：同上（企业版无个人成长体系，任务端点上游一律 403）。
		if a.IsEnterprise() {
			return
		}
		failed := 0
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
				} else {
					failed++
				}
				sort.Slice(one.grow, func(i, j int) bool { // 按 autoActions 顺序（依赖前置）
					return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
				})
			} else {
				failed++
			}
		}
		if failed > 0 {
			mu.Lock()
			preErrors++
			mu.Unlock()
		}
		if len(one.grow) > 0 {
			mu.Lock()
			accts = append(accts, one)
			mu.Unlock()
		}
	}) {
		q.mu.Lock()
		q.running = false
		q.phase = ""
		q.message = "server shutting down"
		q.mu.Unlock()
		return false, 0, 0, "server shutting down"
	}

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
		if preErrors > 0 {
			log.Printf("panel: 队列预扫不完整：%d 个账号查询失败，未启动队列", preErrors)
			q.mu.Lock()
			q.running = false
			q.phase = ""
			q.message = fmt.Sprintf("预扫不完整：%d 个账号任务查询失败，未启动队列（可重试）", preErrors)
			q.mu.Unlock()
			return false, 0, 0, q.message
		}
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		q.mu.Lock()
		q.running = false
		q.phase = ""
		q.message = "全部账号没有待办任务"
		q.mu.Unlock()
		return false, 0, 0, "全部账号没有待办任务"
	}

	q.mu.Lock()
	q.setItemsLocked(items)
	q.conc = concurrency
	q.phase = "exec"
	q.preDone = 0
	q.preTotal = 0
	q.preWarn = ""
	if preErrors > 0 {
		q.preWarn = fmt.Sprintf("%d 个账号任务查询失败，本轮可能漏扫；可稍后重试", preErrors)
	}
	q.message = ""
	seq = q.seq
	q.mu.Unlock()

	p.invalidateScanCache()
	if !p.GoBackground(func() { p.runQueueItems(accts, items, concurrency) }) {
		q.mu.Lock()
		q.running = false
		q.phase = ""
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "server shutting down"
	}
	log.Printf("panel: 队列启动：%d 项（并发 %d，成长 %v）", len(items), concurrency, growth)
	return true, len(items), seq, ""
}

// RunGrowthQueueOnce 调度器 growth 时点回调（sch.SetGrowthHook 挂载）：与
// 「执行全部待办」按钮完全同管线（成长，串行并发 1）。Sequential 族
// 每日零点解锁一环，此前只能手动扫描推进；此回调让链条每天自动走一环。
// 异步执行（startGrowthQueue 启动 goroutine 即返），已在跑/无待办安全跳过。
func (p *Panel) RunGrowthQueueOnce() {
	// 停机入口先关面板后台闸；调度回调若恰好在此后到达，不再启动新队列。
	p.bgMu.Lock()
	stopped := p.bgStop
	p.bgMu.Unlock()
	if stopped {
		return
	}
	started, total, _, _ := p.startGrowthQueueAsync(1, true)
	if started {
		// 异步入口在预扫完成前返回 total=0；真正的条目数会写入队列状态。
		if total > 0 {
			log.Printf("panel: 定时成长任务队列已启动（%d 项）", total)
		} else {
			log.Printf("panel: 定时成长任务队列已启动（预扫中）")
		}
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
			q.phase = ""
			q.mu.Unlock()
			log.Printf("panel: 队列执行 panic: %v", v)
			return
		}
		q.mu.Lock()
		q.running = false
		q.phase = ""
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	// 固定 worker 池：账号间并发上限不变，但不再为每个账号先起一个 goroutine
	// 再阻塞等信号量。panic 在本任务内兜住，标记失败后继续执行其他账号。
	runBounded(concurrency, len(accts), func(k int) {
		one := accts[k]
		func() {
			defer func() {
				if v := recover(); v != nil {
					log.Printf("panel: 队列账号 worker panic uid=%s: %v", logfmt.UID8(one.a.UID), v)
					p.queueSet(q, one.a.UID, func(it *queueItem) {
						if it.Status == "pending" || it.Status == "running" {
							it.Status, it.Message = "error", fmt.Sprintf("执行异常：%v", v)
						}
					})
				}
			}()
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
			// 只遍历本账号的条目：队列启动后条目固定，q.byUID 是启动时建好的
			// 索引；旧实现每个账号都遍历全部条目，账号/待办多时是 O(n×m)。
			q.mu.Lock()
			indexes := append([]int(nil), q.byUID[one.a.UID]...)
			q.mu.Unlock()
			for _, i := range indexes {
				kind, code := q.snapshotAt(i)
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
		}()
	})
}

// queueAccount 队列执行的账号单元（runQueueItems 参数）。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// snapshotAt 锁内读条目的类型/任务码（避免锁外持有指针）。
func (q *queueState) snapshotAt(i int) (kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].Kind, q.items[i].Code
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
	for _, i := range q.byUID[uid] {
		fn(&q.items[i])
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
		log.Printf("panel: 队列 accept uid=%s: %v（不阻塞）", logfmt.UID8(a.UID), err)
		return 0
	}
	log.Printf("panel: 队列 accept uid=%s: 已接受 %d 个任务", logfmt.UID8(a.UID), len(codes))
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
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", logfmt.UID8(a.UID), code, msg)
	return msg, nil
}

// tasksQueueStatus 队列状态（轮询用）。
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	snap := map[string]any{
		"running":    q.running,
		"phase":      q.phase,
		"pre_done":   q.preDone,
		"pre_total":  q.preTotal,
		"total":      len(items),
		"conc":       q.conc,
		"started":    !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq":        q.seq,
		"items":      items,
		"message":    q.message,
		"pre_warn":   q.preWarn,
	}
	// 不在队列锁内序列化和写响应：客户端慢或 items 很多时，写网络会一直占着
	// q.mu，使 queueMarkAt/queueSet 排队，前端看到的是「队列卡住」。快照复制
	// 是 O(items) 的短临界区，序列化与网络写放到锁外。
	q.mu.Unlock()
	writeJSON(w, http.StatusOK, snap)
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
	idx := make([]int, 0, len(accts))
	for i, st := range accts {
		if st.Disabled {
			continue // 未占位，行末统一压掉
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			idx = append(idx, i)
		}
	}
	if !p.runBackgroundBounded(3, len(idx), func(k int) {
		i := idx[k]
		a := p.cfg.Pool.AuthByUID(accts[i].UID)
		if a == nil {
			return
		}
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
	}) {
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	}
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}

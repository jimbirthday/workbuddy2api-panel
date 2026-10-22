package panel

// tasksnapshot.go 账号成长任务列表的短 TTL 快照。
//
// 为什么需要它：一次「一键做任务」会遍历 25 个动作，每个动作执行前都要读一遍
// 任务列表（mp 专属码还要再读一遍 mp 口径），执行后再回读 1~4 次——单账号
// 实测 50~65 次列表请求，其中约 30 次是同一次执行内的重复快照。470 个账号的
// 队列放大后是两万次纯冗余的上游 GET，是「做任务慢」的第二大来源（第一是
// mp 事件的真人节奏等待）。
//
// 语义约束：
//   - 只读快照。任何写操作（accept / 事件上报 / 领奖）之后，调用方必须
//     invalidateTaskSnap，否则后续 before 判定会用旧进度。
//   - TTL 很短（3 秒）：即使漏了失效，异步计分场景下最多多读 3 秒的旧值，
//     而回读路径（taskByCodeWaiting）强制绕过缓存。
//   - 同一账号并发请求合并（single-flight）：扫描与队列同时打开一个账号时
//     不会打出两份列表请求。

import (
	"context"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// taskSnapTTL 快照有效期。取 3 秒：足以覆盖一个动作前后的多次读取（同一动作
// 内 1~3 次），又短于上游异步计分的落账时间（实测 5~8 秒），不会把「刚上报的
// 进度」永久挡在缓存外。
var taskSnapTTL = 3 * time.Second

// taskSnapTimeout 单次快照拉取的总上限（两路口径并行共享）。
var taskSnapTimeout = 20 * time.Second

type taskSnapEntry struct {
	at    time.Time
	tasks []upstream.Task
	err   error
}

// taskSnapState 按 uid 缓存。条目数有界（账号数），不做 LRU。
type taskSnapState struct {
	mu     sync.Mutex
	items  map[string]taskSnapEntry
	flight map[string]chan struct{}
}

func (p *Panel) taskSnap() *taskSnapState {
	p.taskSnapOnce.Do(func() {
		p.ts = &taskSnapState{items: map[string]taskSnapEntry{}, flight: map[string]chan struct{}{}}
	})
	return p.ts
}

// taskList 返回该账号的合并任务列表（默认口径 + 小程序口径，按 task_code 去重）。
// force=true 绕过缓存（回读路径必须看到最新进度）。
//
// 默认口径失败才算错误；mp 口径失败只降级（活动结束/未开通的部署不受影响）。
func (p *Panel) taskList(a *auth.Auth, force bool) ([]upstream.Task, error) {
	if a == nil || p.cfg.Upstream == nil {
		return nil, errTaskSnapNoUpstream
	}
	st := p.taskSnap()
	uid := a.UID
	for {
		st.mu.Lock()
		if !force {
			if e, ok := st.items[uid]; ok && time.Since(e.at) < taskSnapTTL {
				st.mu.Unlock()
				return e.tasks, e.err
			}
		}
		if ch, ok := st.flight[uid]; ok {
			st.mu.Unlock()
			<-ch // 等待在途拉取完成后重读缓存
			force = false
			continue
		}
		ch := make(chan struct{})
		st.flight[uid] = ch
		st.mu.Unlock()

		tasks, err := p.fetchTaskList(a)

		st.mu.Lock()
		if err == nil {
			st.items[uid] = taskSnapEntry{at: time.Now(), tasks: tasks}
		}
		// 失败不写缓存（也不刷新旧条目的时间戳）：下一次调用会重新拉取，
		// 瞬时故障不会被固化 3 秒。
		delete(st.flight, uid)
		close(ch)
		st.mu.Unlock()
		return tasks, err
	}
}

// fetchTaskList 一次并行拉取默认与小程序两路口径并合并。首次命中即用。
func (p *Panel) fetchTaskList(a *auth.Auth) ([]upstream.Task, error) {
	ctx, cancel := context.WithTimeout(context.Background(), taskSnapTimeout)
	defer cancel()
	var (
		wg      sync.WaitGroup
		tasks   []upstream.Task
		mpTasks []upstream.Task
		err     error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		tasks, err = p.cfg.Upstream.ListTasksContext(ctx, a)
	}()
	go func() {
		defer wg.Done()
		mpTasks, _ = p.cfg.Upstream.ListTasksMPContext(ctx, a) // 失败只降级
	}()
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if len(mpTasks) == 0 {
		return tasks, nil
	}
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
	return tasks, nil
}

// invalidateTaskSnap 丢弃某账号的任务快照。写操作之后必须调用。
func (p *Panel) invalidateTaskSnap(uid string) {
	if uid == "" {
		return
	}
	st := p.taskSnap()
	st.mu.Lock()
	delete(st.items, uid)
	st.mu.Unlock()
}

// taskFromList 在列表中定位任务码（未找到返回 nil，不视为错误）。
func taskFromList(tasks []upstream.Task, code string) *upstream.Task {
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i]
		}
	}
	return nil
}

package panel

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"time"
)

// sleepCtx 是面板侧的可取消等待：ctx 已取消时立即返回 false（停机不必等
// 剩余节流时间）。面板任务里大量「给上游留处理时间」的等待都必须走它，
// 否则 StopWaiting / docker stop 会被最长的 sleep 拖住数分钟。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx == nil || ctx.Err() == nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// wait 是面板后台作业的可取消等待（绑定 p.bgCtx）。返回 false 表示停机信号
// 已到，调用方应立即收尾（不要再打后续上游请求）。
//
// 与 sleepCtx(context.Background(), …) 的区别：Background 版本只能被显式的
// 任务级 ctx 取消，面板停机时不会生效。队列/接受任务这类长链路必须走这里，
// 否则 docker stop 要等「账号数 × 项间节流」全部睡完。
func (p *Panel) wait(d time.Duration) bool {
	return sleepCtx(p.bgCtx, d)
}

// runBounded 在固定数量的 worker 上处理 n 个下标，避免「每个任务先起一个
// goroutine、再在 goroutine 里等信号量」造成的瞬时 goroutine 放大。并发上限
// 与调用方原有信号量一致；任务按到达顺序取号，不承诺完成顺序。
//
// 任何任务 panic 都会被收集并在全部 worker 退出后重新抛出，交给调用方的
// recover/停机收尾逻辑处理；不会因为一个 worker 退出而让任务分发永久阻塞。
func runBounded(workers, n int, fn func(int)) {
	if n <= 0 || fn == nil {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	var panicOnce sync.Once
	var firstPanic any
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				func() {
					defer func() {
						if v := recover(); v != nil {
							panicOnce.Do(func() { firstPanic = v })
						}
					}()
					fn(i)
				}()
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	if firstPanic != nil {
		panic(firstPanic)
	}
}

// runBackgroundBounded 用一个受停机闸管理后台任务跑固定并发作业，并等待完成。
// 返回 false 表示停机闸已关闭、作业未启动；调用方应立即回滚单飞/锁状态。
func (p *Panel) runBackgroundBounded(workers, n int, fn func(int)) bool {
	if n <= 0 {
		return true
	}
	done := make(chan struct{})
	if !p.GoBackground(func() {
		defer close(done)
		// 这是后台边界：runBounded 会把 worker panic 重新抛出，若没有同步
		// 调用方的 recover，整个进程会退出。批量运维任务失败应只影响本轮，
		// 因此在这里隔离并记录，done 仍然关闭，等待者不会悬挂。
		defer func() {
			if v := recover(); v != nil {
				log.Printf("panel: 后台批量任务 panic（workers=%d n=%d）: %v\n%s", workers, n, v, debug.Stack())
			}
		}()
		runBounded(workers, n, fn)
	}) {
		return false
	}
	<-done
	return true
}

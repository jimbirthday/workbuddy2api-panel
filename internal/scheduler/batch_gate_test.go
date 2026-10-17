package scheduler

import (
	"testing"
	"time"
)

// TestTryRunBatchSingleFlight 同一 kind 的单飞闸门：运行中拒绝第二次，
// 结束后可再次申请。这是「手动 + 定时两路不并发同批账号」的核心保障。
func TestTryRunBatchSingleFlight(t *testing.T) {
	s := New(Config{})
	release := make(chan struct{})
	started := make(chan struct{})

	if !s.TryRunBatch("checkin", func() {
		close(started)
		<-release
	}) {
		t.Fatal("首次申请应成功")
	}
	<-started

	if s.TryRunBatch("checkin", func() {}) {
		t.Fatal("同一 kind 运行中第二次申请应失败")
	}
	// 不同 kind 互不影响：travel 独立放行并在函数返回前跑完。
	ranOther := false
	if !s.TryRunBatch("travel", func() { ranOther = true }) {
		t.Fatal("不同 kind 不应互斥")
	}

	close(release)
	// 等 checkin 收尾清位：轮询到可再次申请为止。
	ok := false
	for i := 0; i < 2000 && !ok; i++ {
		done := make(chan struct{})
		if s.TryRunBatch("checkin", func() { close(done) }) {
			<-done
			ok = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !ok {
		t.Fatal("结束后应可再次申请（清位失败）")
	}
	if !ranOther {
		t.Fatal("travel 批次应已执行")
	}
}

// TestTryRunBatchKindSyncSkipsWhenRunning tryRunBatchKind 为定时路径的同步变体：
// 同类批已在跑时直接跳过（不执行、不阻塞），确保 cron 不会与手动批同跑。
func TestTryRunBatchKindSyncSkipsWhenRunning(t *testing.T) {
	s := New(Config{})
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		s.TryRunBatch("activity", func() {
			close(started)
			<-release
		})
	}()
	<-started

	ran := false
	s.tryRunBatchKind("activity", func() { ran = true })
	if ran {
		t.Fatal("定时路径在手动批运行中应被闸门跳过")
	}
	close(release)
}

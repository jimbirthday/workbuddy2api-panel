package usage

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDailyTokenUsageSinceLocalMidnight(t *testing.T) {
	r := New("")
	day := time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local)
	add := func(at time.Time, uid, model string, tokens int64) {
		r.Add(at, "cn", uid, model, Delta{TotalTokens: tokens, HasTotal: true}, true)
	}

	add(day.Add(-2*time.Hour), "u1", "glm-5.3", 700) // 昨天：不得计入
	add(day.Add(time.Hour), "u1", "glm-5.3", 100)
	add(day.Add(2*time.Hour), "u1", "cn:glm-5.3", 25) // 归一化后与裸名合并
	add(day.Add(3*time.Hour), "u2", "kimi-k3", 50)
	r.Add(day.Add(4*time.Hour), "cn", "u3", "failed", Delta{}, false) // 失败且无 usage：无 token

	got, ok := r.DailyTokenUsageAt(day.Add(5 * time.Hour))
	if !ok {
		t.Fatal("DailyTokenUsageAt ok=false for live recorder")
	}
	if u := got["u1"]; u.Tokens != 125 || u.Models["glm-5.3"] != 125 {
		t.Fatalf("u1 today = %+v, want tokens=125 model glm-5.3=125", u)
	}
	if u := got["u2"]; u.Tokens != 50 || u.Models["kimi-k3"] != 50 {
		t.Fatalf("u2 today = %+v, want tokens=50 model kimi-k3=50", u)
	}
	if _, exists := got["u3"]; exists {
		t.Fatalf("failed attempt without usage must not create a daily entry: %+v", got["u3"])
	}

	add(day.AddDate(0, 0, 1).Add(time.Hour), "u1", "glm-5.3", 900) // 明天：不得提前计入今天
	got, ok = r.DailyTokenUsageAt(day.Add(5 * time.Hour))
	if !ok || got["u1"].Tokens != 125 {
		t.Fatalf("future bucket leaked into today = %+v ok=%v, want 125", got["u1"], ok)
	}

	next, ok := r.DailyTokenUsageAt(day.Add(24 * time.Hour))
	if !ok {
		t.Fatal("DailyTokenUsageAt(next day) ok=false")
	}
	if next["u1"].Tokens != 900 {
		t.Fatalf("next day should reset at local midnight, got %+v", next)
	}
}

// TestDailyTokenUsageCacheRefreshesWithinTTL 缓存不能把新增用量藏住：
// 请求完成（Add）后，下一次读取必须在 TTL 内立即看到新增 token。
func TestDailyTokenUsageCacheRefreshesWithinTTL(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 5, 0, 0, 0, time.Local)
	r.Add(now.Add(-time.Minute), "cn", "u1", "glm-5.3", Delta{TotalTokens: 100, HasTotal: true}, true)
	first, ok := r.DailyTokenUsageAt(now)
	if !ok || first["u1"].Tokens != 100 {
		t.Fatalf("first snapshot = %+v ok=%v", first, ok)
	}
	r.Add(now.Add(-30*time.Second), "cn", "u1", "glm-5.3", Delta{TotalTokens: 50, HasTotal: true}, true)
	later, ok := r.DailyTokenUsageAt(now.Add(time.Millisecond))
	if !ok || later["u1"].Tokens != 150 {
		t.Fatalf("snapshot after Add = %+v ok=%v, want 150 (cache must be invalidated)", later, ok)
	}
}

func TestDailyTokenUsageNilRecorder(t *testing.T) {
	var r *Recorder
	if got, ok := r.DailyTokenUsageAt(time.Now()); ok || got != nil {
		t.Fatalf("nil recorder = %v/%v, want nil/false", got, ok)
	}
}

// 读侧拿到的快照必须不可变：并发 Add 只能发布新表，不能原地改旧表，
// 否则 pick 遍历到一半会看到已更新的 Models，甚至触发 map 竞态。
func TestDailyTokenUsageSnapshotIsImmutable(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 5, 0, 0, 0, time.Local)
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 100, HasTotal: true}, true)
	first, ok := r.DailyTokenUsageAt(now)
	if !ok || first["u1"].Tokens != 100 || first["u1"].Models["m1"] != 100 {
		t.Fatalf("first snapshot = %+v ok=%v", first, ok)
	}
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 50, HasTotal: true}, true)
	r.Add(now, "cn", "u1", "m2", Delta{TotalTokens: 25, HasTotal: true}, true)
	if first["u1"].Tokens != 100 || first["u1"].Models["m1"] != 100 || first["u1"].Models["m2"] != 0 {
		t.Fatalf("older snapshot mutated in place: %+v", first["u1"])
	}
	second, _ := r.DailyTokenUsageAt(now)
	if second["u1"].Tokens != 175 || second["u1"].Models["m1"] != 150 || second["u1"].Models["m2"] != 25 {
		t.Fatalf("new snapshot = %+v", second["u1"])
	}
}

// 增量快照不能把未变化的账号整表复制：热写必须与账号数无关。这里用预算式
// 断言锁住回归，不依赖 wall-clock（CI 抖动下不作为性能门槛）。
func TestDailyAddWarmDoesNotCopyAccountTable(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 5, 0, 0, 0, time.Local)
	for i := 0; i < 470; i++ {
		r.Add(now, "cn", "u"+strconv.Itoa(i), "m1", Delta{TotalTokens: 1, HasTotal: true}, true)
	}
	if _, ok := r.DailyTokenUsageAt(now); !ok {
		t.Fatal("snapshot not initialized")
	}
	later := now.Add(time.Second)
	allocs := testing.AllocsPerRun(100, func() {
		r.Add(later, "cn", "hot", "m1", Delta{TotalTokens: 1, HasTotal: true}, true)
	})
	// Add 自身固定写一个桶 + 一个 delta 条目；若重新复制 470 账号的 map，
	// allocs 会明显超出该常数。阈值只防结构性回退，不做微基准比较。
	if allocs > 8 {
		t.Fatalf("warm Add allocs=%.1f, want small constant (no per-account copy)", allocs)
	}
}

// 发布过增量后再次读取，不能每次都把 470 账号的基础表与覆盖层合并一遍。
// 合并视图只在增量版本变化时重建，稳态读取应只做一次 map 返回。
func TestDailyOverridesViewDoesNotCopyPerRead(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 5, 0, 0, 0, time.Local)
	for i := 0; i < 470; i++ {
		r.Add(now, "cn", "u"+strconv.Itoa(i), "m1", Delta{TotalTokens: 1, HasTotal: true}, true)
	}
	if _, ok := r.DailyTokenUsageAt(now); !ok {
		t.Fatal("snapshot not initialized")
	}
	r.Add(now, "cn", "hot", "m1", Delta{TotalTokens: 2, HasTotal: true}, true)
	first, _ := r.DailyTokenUsageAt(now)
	if first["hot"].Tokens != 2 {
		t.Fatalf("merged view hot=%+v want 2", first["hot"])
	}
	allocs := testing.AllocsPerRun(100, func() {
		got, ok := r.DailyTokenUsageAt(now)
		if !ok || got["u469"].Tokens != 1 {
			t.Fatal("bad daily view")
		}
	})
	if allocs > 2 {
		t.Fatalf("steady read allocs=%.1f, want no full-table copy per read", allocs)
	}
}

// Add 发生在快照建立前时，首次读取必须把历史桶一次算进去；此后的 Add 走增量，
// 不能丢掉历史部分。
func TestDailyTokenUsageIncrementalAfterColdStart(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 5, 0, 0, 0, time.Local)
	r.Add(now.Add(-time.Hour), "cn", "u1", "m1", Delta{TotalTokens: 40, HasTotal: true}, true)
	got, ok := r.DailyTokenUsageAt(now)
	if !ok || got["u1"].Tokens != 40 {
		t.Fatalf("cold snapshot=%+v ok=%v", got, ok)
	}
	r.Add(now.Add(-time.Minute), "cn", "u1", "m2", Delta{PromptTokens: 7, CompletionTokens: 3, HasPromptTokens: true, HasCompletion: true}, true)
	got, _ = r.DailyTokenUsageAt(now)
	if got["u1"].Tokens != 50 || got["u1"].Models["m2"] != 10 {
		t.Fatalf("incremental snapshot=%+v want tokens=50 m2=10", got["u1"])
	}
}

func TestDailyTokenUsageZeroValueRecorder(t *testing.T) {
	var r Recorder
	r.Add(time.Now(), "cn", "u", "m", Delta{TotalTokens: 3, HasTotal: true}, true)
	got, ok := r.DailyTokenUsageAt(time.Now())
	if !ok || got["u"].Tokens != 3 {
		t.Fatalf("zero recorder daily usage = %+v/%v, want u=3/true", got, ok)
	}
}

// 冷启动的全表聚合与并发 Add 交错时，不能发布「少了聚合窗口内新增 token」的
// 快照；同一天的多个并发读者也只能有一个真正做全表聚合。
func TestDailySnapshotConcurrentColdBuild(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)
	r.Add(now.Add(-time.Hour), "cn", "u1", "m1", Delta{TotalTokens: 100, HasTotal: true}, true)

	const readers = 32
	var wg sync.WaitGroup
	results := make(chan map[string]DailyTokenUsage, readers)
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, ok := r.DailyTokenUsageAt(now)
			if !ok {
				t.Error("DailyTokenUsageAt ok=false")
				return
			}
			results <- got
		}()
	}
	close(start)
	// 与构建窗口竞争：若旧实现发布不完整快照，最终结果会少这 25。
	r.Add(now.Add(-time.Minute), "cn", "u1", "m1", Delta{TotalTokens: 25, HasTotal: true}, true)
	wg.Wait()
	close(results)

	final, ok := r.DailyTokenUsageAt(now)
	if !ok || final["u1"].Tokens != 125 {
		t.Fatalf("final snapshot=%+v ok=%v, want u1=125 after concurrent cold build", final["u1"], ok)
	}
	// 已发布快照之后再 Add，读取必须立即看到，不能等下一次全表构建。
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 5, HasTotal: true}, true)
	if got, _ := r.DailyTokenUsageAt(now); got["u1"].Tokens != 130 {
		t.Fatalf("incremental add after publish=%+v want 130", got["u1"])
	}
}

// 持续 Add 不能饿死冷启动聚合：构建期间的新增会被合并进首次发布，读者不能
// 无限等待，也不能在聚合之后逐步重算。
func TestDailySnapshotBuildCompletesUnderContinuousAdd(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)
	// 制造足够大的聚合工作量，让构建窗口覆盖并发写入。
	for i := 0; i < 20000; i++ {
		r.Add(now.Add(-time.Hour), "cn", "u"+strconv.Itoa(i%100), "m"+strconv.Itoa(i%7), Delta{TotalTokens: 1, HasTotal: true}, true)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Add(now, "cn", "hot", "m1", Delta{TotalTokens: 1, HasTotal: true}, true)
				}
			}
		}(w)
	}
	done := make(chan map[string]DailyTokenUsage, 1)
	go func() {
		got, ok := r.DailyTokenUsageAt(now)
		if ok {
			done <- got
		}
	}()
	var first map[string]DailyTokenUsage
	select {
	case first = <-done:
	case <-time.After(2 * time.Second):
		close(stop)
		writers.Wait()
		t.Fatal("DailyTokenUsageAt did not publish within 2s under continuous Add")
	}
	close(stop)
	writers.Wait()
	if first["u0"].Tokens == 0 {
		t.Fatalf("cold snapshot missing historical buckets: %+v", first["u0"])
	}

	// 发布后的最终值必须包含所有成功写入的 token；这里只校验热账号最终非零，
	// 精确值由 TestDailySnapshotConcurrentColdBuild 的受控场景覆盖。
	final, _ := r.DailyTokenUsageAt(now)
	if final["hot"].Tokens == 0 || final["hot"].Models["m1"] == 0 {
		t.Fatalf("continuous Adds missing from published snapshot: %+v", final["hot"])
	}
}

// 同一账号/模型在一轮构建中先后写入两次时，必须只补上「扫描之后」的那次。
// 早期实现把 pending 合并成一条并只记住最后版本，会把扫描前已包含的 5 重复
// 叠加，得到 base+16；正确结果应为 base+11。
func TestDailySnapshotKeepsInterleavedPendingAtShardBoundary(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)
	r.Add(now, "cn", "interleaved", "m1", Delta{TotalTokens: 100, HasTotal: true}, true)

	key := "h:" + now.Format(hourLayout) + "|cn|interleaved|m1|"
	target := shardIndex(key)
	dailyShardScanHook = func(shard int, after bool) {
		if shard != target {
			return
		}
		if !after {
			r.Add(now, "cn", "interleaved", "m1", Delta{TotalTokens: 5, HasTotal: true}, true)
			return
		}
		r.Add(now, "cn", "interleaved", "m1", Delta{TotalTokens: 6, HasTotal: true}, true)
	}
	t.Cleanup(func() { dailyShardScanHook = nil })

	got, ok := r.DailyTokenUsageAt(now)
	if !ok {
		t.Fatal("DailyTokenUsageAt ok=false")
	}
	if u := got["interleaved"]; u.Tokens != 111 || u.Models["m1"] != 111 {
		t.Fatalf("interleaved snapshot = %+v, want tokens=111 model m1=111 (base 100 + 5 + 6)", u)
	}
}

// resetDailyUsage 清空 building 后，新构建者可以立刻接手；旧构建者扫描完
// 返回时只能放弃自己的结果，不能清掉新构建者的所有权标记。否则后续读者会
// 认为无人构建并再起一个，形成并行构建和少计快照。
func TestDailyBuildOwnershipSurvivesReset(t *testing.T) {
	r := New("")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)
	r.Add(now, "cn", "u1", "m1", Delta{TotalTokens: 100, HasTotal: true}, true)

	firstBlocked := make(chan struct{})
	secondBlocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	rejected := make(chan struct{})
	var scans atomic.Int32
	dailyShardScanHook = func(shard int, after bool) {
		if shard != 0 || after {
			return
		}
		switch scans.Add(1) {
		case 1:
			close(firstBlocked)
			<-releaseFirst
		case 2:
			close(secondBlocked)
			<-releaseSecond
		}
	}
	dailyBuildRejectHook = func() { close(rejected) }
	t.Cleanup(func() {
		dailyShardScanHook = nil
		dailyBuildRejectHook = nil
	})

	firstDone := make(chan map[string]DailyTokenUsage, 1)
	go func() {
		got, _ := r.DailyTokenUsageAt(now)
		firstDone <- got
	}()
	<-firstBlocked

	r.resetDailyUsage()
	secondDone := make(chan map[string]DailyTokenUsage, 1)
	go func() {
		got, _ := r.DailyTokenUsageAt(now)
		secondDone <- got
	}()
	<-secondBlocked

	close(releaseFirst)
	<-rejected
	r.daily.mu.Lock()
	ownerStillSet := !r.daily.building.IsZero() && r.daily.buildToken != 0
	r.daily.mu.Unlock()
	if !ownerStillSet {
		t.Fatal("old builder cleared the new builder's ownership")
	}

	close(releaseSecond)
	if got := <-secondDone; got["u1"].Tokens != 100 {
		t.Fatalf("new builder snapshot = %+v, want u1=100", got["u1"])
	}
	if got := <-firstDone; got["u1"].Tokens != 100 {
		t.Fatalf("old builder snapshot = %+v, want u1=100", got["u1"])
	}
}

// BenchmarkDailyTokenUsageCold 模拟冷启动/跨零点后的首次全表聚合。生产形态
// 为 470 账号 × 4 模型 × 24 小时桶；稳态请求走增量快照，不再进入这里。
func BenchmarkDailyTokenUsageCold(b *testing.B) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	for a := 0; a < 470; a++ {
		uid := "u" + strconv.Itoa(a)
		for m := 0; m < 4; m++ {
			model := "m" + strconv.Itoa(m)
			for h := 0; h < 24; h++ {
				r.Add(base.Add(time.Duration(h)*time.Hour), "cn", uid, model,
					Delta{TotalTokens: 12, HasTotal: true}, true)
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Add(time.Now(), "cn", "hot", "m0", Delta{TotalTokens: 1, HasTotal: true}, true)
		r.resetDailyUsage()
		if got, ok := r.DailyTokenUsageAt(time.Now()); !ok || got["u0"].Tokens == 0 {
			b.Fatal("empty daily snapshot")
		}
	}
}

// BenchmarkDailyTokenUsageWarm 稳态下每个请求都 read 一次日额度：必须只读
// 增量快照，不能随历史桶数增长。这是 pick 热路径的真实形态。
func BenchmarkDailyTokenUsageWarm(b *testing.B) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	for a := 0; a < 470; a++ {
		uid := "u" + strconv.Itoa(a)
		for m := 0; m < 4; m++ {
			model := "m" + strconv.Itoa(m)
			for h := 0; h < 24; h++ {
				r.Add(base.Add(time.Duration(h)*time.Hour), "cn", uid, model,
					Delta{TotalTokens: 12, HasTotal: true}, true)
			}
		}
	}
	now := time.Now()
	if _, ok := r.DailyTokenUsageAt(now); !ok {
		b.Fatal("snapshot not initialized")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Add(now, "cn", "hot", "m0", Delta{TotalTokens: 1, HasTotal: true}, true)
	}
}

// BenchmarkDailySnapshotPublishWarm 请求完成时发布新的日额度快照。旧实现每次
// 深拷贝整张账号表，470 账号时约 18µs/41KiB；稳态发布应只与账号数成正比，
// 且不需要复制未命中账号的 Models。
func BenchmarkDailySnapshotPublishWarm(b *testing.B) {
	r := New("")
	now := time.Now()
	for a := 0; a < 470; a++ {
		uid := "u" + strconv.Itoa(a)
		for m := 0; m < 4; m++ {
			r.Add(now, "cn", uid, "m"+strconv.Itoa(m),
				Delta{TotalTokens: 12, HasTotal: true}, true)
		}
	}
	if _, ok := r.DailyTokenUsageAt(now); !ok {
		b.Fatal("snapshot not initialized")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Add(now, "cn", "hot", "m0", Delta{TotalTokens: 1, HasTotal: true}, true)
	}
}

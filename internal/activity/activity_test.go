package activity

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testStore(t *testing.T, path string) *Store {
	t.Helper()
	return New(Config{Path: path, MaxBytes: 1 << 20, RetentionDays: 90})
}

func mustAppend(t *testing.T, s *Store, rec Record) {
	t.Helper()
	if err := s.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

// 异步模式必须：Append 不阻塞在 fsync；Close 排空队列后所有记录可见。
func TestAsyncAppendDrainsOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	s := New(Config{Path: path, MaxBytes: 1 << 20, RetentionDays: 90, Async: true})
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		if err := s.Append(Record{Time: base.Add(time.Duration(i) * time.Second), UID: "u" + strconv.Itoa(i%7), Task: TaskCheckin, Trigger: TriggerSchedule, OK: true}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := testStore(t, path).Query(Filter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 500 || len(got.Records) != 500 {
		t.Fatalf("after close total=%d len=%d want 500", got.Total, len(got.Records))
	}
	if got.Records[0].UID != "u2" { // 第 500 条 i=499, 499%7=2
		t.Fatalf("newest record uid=%q want u2", got.Records[0].UID)
	}
}

// 正常关闭后再 Append 必须立即失败，不能把记录塞进无人消费的队列。
func TestAsyncAppendAfterCloseFails(t *testing.T) {
	s := New(Config{Path: filepath.Join(t.TempDir(), FileName()), Async: true})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Record{UID: "u1", Task: TaskCheckin}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("append after close err=%v want ErrQueueFull", err)
	}
}

// 队列满时 Append 返回 ErrQueueFull 而不是阻塞；调用方（调度器）会静默忽略。
func TestAsyncAppendQueueFullDoesNotBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	s := New(Config{Path: path, Async: true})
	// 先占满队列（不启动消费者，保证测试期间队列保持满态）。
	for i := 0; i < cap(s.appQueue); i++ {
		s.appQueue <- []byte("{}\n")
	}
	done := make(chan error, 1)
	go func() { done <- s.Append(Record{UID: "u", Task: TaskCheckin}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueFull) {
			t.Fatalf("append err=%v want ErrQueueFull", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Append blocked on full queue")
	}
	if s.Dropped() == 0 {
		t.Fatal("dropped counter not incremented")
	}
	// 没有启动消费者时 Close 也会启动并排空占位行。
	_ = s.Close()
}

// Close 与 Append 并发时，每一条返回 nil 的记录都必须最终落盘；不能出现
// 「入队成功但消费者已退出」导致静默丢失。
func TestAsyncCloseConcurrentAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	s := New(Config{Path: path, Async: true})
	const writers, each = 8, 100
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				err := s.Append(Record{UID: "w" + strconv.Itoa(w), Task: TaskCheckin, Trigger: TriggerManual, OK: true})
				if err == nil {
					accepted.Add(1)
				} else if !errors.Is(err, ErrQueueFull) {
					t.Errorf("append error: %v", err)
				}
			}
		}(w)
	}
	time.Sleep(5 * time.Millisecond)
	_ = s.Close()
	wg.Wait()

	got, err := testStore(t, path).Query(Filter{Limit: maxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if int64(got.Total) != accepted.Load() {
		t.Fatalf("durable=%d accepted=%d (nil-returning appends must persist)", got.Total, accepted.Load())
	}
}

// TestAppendRestartAndNewestFirst 验证写入后可重启读取，且读取顺序为最新在前。
func TestAppendRestartAndNewestFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	mustAppend(t, store, Record{Time: base, UID: "u1", Nickname: "甲", Realm: "cn", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: "签到成功"})
	mustAppend(t, store, Record{Time: base.Add(time.Minute), UID: "u2", Nickname: "乙", Realm: "global", Task: TaskActivity, Trigger: TriggerManual, OK: false, Message: "boom"})
	mustAppend(t, store, Record{Time: base.Add(2 * time.Minute), UID: "u1", Nickname: "甲", Realm: "cn", Task: TaskTravel, Trigger: TriggerSchedule, OK: true, Message: "depart ok"})

	reopened := testStore(t, path)
	got, err := reopened.Query(Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Total != 3 || len(got.Records) != 3 {
		t.Fatalf("total=%d len=%d want 3/3", got.Total, len(got.Records))
	}
	want := []string{"u1", "u2", "u1"}
	for i, uid := range want {
		if got.Records[i].UID != uid {
			t.Errorf("record[%d].uid=%q want %q", i, got.Records[i].UID, uid)
		}
	}
	if got.Records[0].Task != TaskTravel || got.Records[2].Task != TaskCheckin {
		t.Errorf("newest-first order wrong: %#v", got.Records)
	}
}

// TestQueryFilters 覆盖 uid / task / result / 时间区间 / limit 的组合过滤。
func TestQueryFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	records := []Record{
		{Time: base, UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true},
		{Time: base.Add(time.Hour), UID: "u2", Task: TaskActivity, Trigger: TriggerManual, OK: false},
		{Time: base.Add(2 * time.Hour), UID: "u1", Task: TaskTravel, Trigger: TriggerSchedule, OK: true},
		{Time: base.Add(3 * time.Hour), UID: "u2", Task: TaskKeepalive, Trigger: TriggerSchedule, OK: true},
	}
	for _, rec := range records {
		mustAppend(t, store, rec)
	}

	cases := []struct {
		name string
		in   Filter
		want int
	}{
		{"uid", Filter{UID: "u1"}, 2},
		{"task", Filter{Task: TaskActivity}, 1},
		{"failed", Filter{Result: "failed"}, 1},
		{"ok alias", Filter{Result: "success"}, 3},
		{"time range", Filter{Since: base.Add(30 * time.Minute), Until: base.Add(2*time.Hour + 30*time.Minute)}, 2},
		{"limit", Filter{Limit: 2}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.Query(tc.in)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if tc.name == "limit" {
				if got.Total != 4 || len(got.Records) != 2 {
					t.Fatalf("limit total=%d len=%d want 4/2", got.Total, len(got.Records))
				}
				return
			}
			if got.Total != tc.want || len(got.Records) != tc.want {
				t.Fatalf("total=%d len=%d want %d", got.Total, len(got.Records), tc.want)
			}
		})
	}

	if _, err := store.Query(Filter{Task: "nope"}); err == nil {
		t.Fatal("invalid task must be rejected")
	}
	if _, err := store.Query(Filter{Result: "nope"}); err == nil {
		t.Fatal("invalid result must be rejected")
	}
}

// queryWindowLegacy 是优化前的滑动窗口实现，只用于差分测试。它必须与新
// Query 在任意 Filter/limit 下逐条一致。
func queryWindowLegacy(t *testing.T, path string, f Filter) QueryResult {
	t.Helper()
	limit := normalizeLimit(f.Limit)
	resultMode, err := normalizeResult(f.Result)
	if err != nil {
		t.Fatal(err)
	}
	store := New(Config{Path: path, MaxBytes: 1 << 20, RetentionDays: 90})
	uid := strings.TrimSpace(f.UID)
	matched := make([]Record, 0, limit)
	total := 0
	err = store.scanLocked(func(rec Record) {
		if uid != "" && rec.UID != uid {
			return
		}
		if f.Task != "" && rec.Task != f.Task {
			return
		}
		if resultMode != "" && rec.OK != (resultMode == "ok") {
			return
		}
		if !inTimeRange(rec.Time, f.Since, f.Until) {
			return
		}
		total++
		if len(matched) < limit {
			matched = append(matched, rec)
			return
		}
		copy(matched, matched[1:])
		matched[len(matched)-1] = rec
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	return QueryResult{Records: matched, Total: total, Limit: limit}
}

// TestQueryRingWindowDiffAgainstLegacy 环形窗口必须与旧实现完全一致：
// 覆盖命中数小于/等于/大于 limit、uid/task/result/时间过滤与连续多次查询。
func TestQueryRingWindowDiffAgainstLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 257; i++ {
		rec := Record{
			Time:    base.Add(time.Duration(i%61) * time.Minute),
			UID:     "u" + strconv.Itoa(i%4),
			Task:    []Task{TaskCheckin, TaskActivity, TaskTravel, TaskKeepalive}[i%4],
			Trigger: TriggerSchedule,
			OK:      i%3 != 0,
			Message: "m" + strconv.Itoa(i),
		}
		mustAppend(t, store, rec)
	}
	from := base.Add(5 * time.Minute)
	to := base.Add(55 * time.Minute)
	filters := []Filter{
		{}, {Limit: 1}, {Limit: 2}, {Limit: 7}, {Limit: 100}, {Limit: 256}, {Limit: 257}, {Limit: 1000},
		{UID: "u1", Limit: 3}, {Task: TaskTravel, Limit: 5}, {Result: "failed", Limit: 9},
		{Result: "ok", Limit: 11}, {Since: from, Until: to, Limit: 13},
		{UID: "u2", Task: TaskCheckin, Result: "failed", Since: from, Limit: 4},
	}
	for _, f := range filters {
		got, err := store.Query(f)
		if err != nil {
			t.Fatalf("filter=%+v: %v", f, err)
		}
		want := queryWindowLegacy(t, path, f)
		if got.Total != want.Total || got.Limit != want.Limit || len(got.Records) != len(want.Records) {
			t.Fatalf("filter=%+v: got total/limit/len=%d/%d/%d want %d/%d/%d", f, got.Total, got.Limit, len(got.Records), want.Total, want.Limit, len(want.Records))
		}
		for i := range got.Records {
			if got.Records[i].Message != want.Records[i].Message || !got.Records[i].Time.Equal(want.Records[i].Time) {
				t.Fatalf("filter=%+v record[%d]=%+v want %+v", f, i, got.Records[i], want.Records[i])
			}
		}
	}
}

// TestQueryEmptyWindow 空历史仍返回合法 limit 与空记录（不返回 nil 契约差异）。
func TestQueryEmptyWindow(t *testing.T) {
	store := testStore(t, filepath.Join(t.TempDir(), FileName()))
	got, err := store.Query(Filter{Limit: maxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 0 || len(got.Records) != 0 || got.Limit != maxLimit {
		t.Fatalf("got %+v", got)
	}
}

// 活动页会随筛选条件产生不同缓存 key；缓存必须有界，不能随切换次数增长。
func TestQueryCacheBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for i := 0; i < queryCacheMax*3; i++ {
		mustAppend(t, store, Record{Time: base.Add(time.Duration(i) * time.Second), UID: "u" + strconv.Itoa(i), Task: TaskCheckin, Trigger: TriggerSchedule, OK: true})
	}
	for i := 0; i < queryCacheMax*3; i++ {
		if _, err := store.Query(Filter{UID: "u" + strconv.Itoa(i), Limit: 10}); err != nil {
			t.Fatal(err)
		}
	}
	store.queryCacheMu.Lock()
	n := len(store.queryCache)
	store.queryCacheMu.Unlock()
	if n > queryCacheMax {
		t.Fatalf("query cache entries=%d want <= %d", n, queryCacheMax)
	}
}

// BenchmarkQueryLargeRingWindow 验证 1 万条命中、limit=100 时查询成本不再随
// 「命中数 × limit」增长；优化前这里的滑动 copy 约占主要 CPU 时间。
func BenchmarkQueryLargeRingWindow(b *testing.B) {
	path := filepath.Join(b.TempDir(), FileName())
	store := New(Config{Path: path, MaxBytes: 64 << 20, RetentionDays: 90})
	base := time.Now().Add(-time.Hour)
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	for i := 0; i < 10000; i++ {
		rec := Record{Time: base.Add(time.Duration(i) * time.Millisecond), UID: "u" + strconv.Itoa(i%50), Task: TaskCheckin, Trigger: TriggerSchedule, OK: i%2 == 0, Message: "m" + strconv.Itoa(i)}
		line, _ := json.Marshal(rec)
		if _, err := bw.Write(append(line, '\n')); err != nil {
			b.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := store.Query(Filter{Limit: 100})
		if err != nil || got.Total != 10000 || len(got.Records) != 100 {
			b.Fatalf("total=%d len=%d err=%v", got.Total, len(got.Records), err)
		}
	}
}

// BenchmarkCompactLargeFile 验证整理大文件时峰值分配不随「历史条数」线性保存
// 整份 []Record；10k 条记录应只保留窗口偏移和文件缓冲。
func BenchmarkCompactLargeFile(b *testing.B) {
	dir := b.TempDir()
	base := time.Now().Add(-24 * time.Hour)
	for n := 0; n < b.N; n++ {
		path := filepath.Join(dir, "activity-"+strconv.Itoa(n)+".jsonl")
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		bw := bufio.NewWriter(f)
		for i := 0; i < 10000; i++ {
			rec := Record{Time: base.Add(time.Duration(i) * time.Second), UID: "u" + strconv.Itoa(i%50), Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: strings.Repeat("x", 80)}
			line, _ := json.Marshal(rec)
			if _, err := bw.Write(append(line, '\n')); err != nil {
				b.Fatal(err)
			}
		}
		if err := bw.Flush(); err != nil {
			b.Fatal(err)
		}
		_ = f.Close()
		store := New(Config{Path: path, MaxBytes: 8 << 20, RetentionDays: 90})
		b.ReportAllocs()
		b.StartTimer()
		if _, _, err := store.Compact(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
	}
}

// TestBadLinesSkipped 坏 JSON、空行、未知任务与没有 uid 的行都不能让读取失败。
func TestBadLinesSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	content := strings.Join([]string{
		`{"ts":"2026-10-18T00:00:00Z","uid":"u1","task":"checkin","trigger":"schedule","ok":true}`,
		`{not json`,
		``,
		`{"ts":"2026-10-18T00:01:00Z","uid":"u2","task":"unknown","trigger":"schedule","ok":true}`,
		`{"ts":"2026-10-18T00:02:00Z","task":"activity","trigger":"schedule","ok":true}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := testStore(t, path).Query(Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.Total != 1 || len(got.Records) != 1 || got.Records[0].UID != "u1" {
		t.Fatalf("got %#v want only u1", got.Records)
	}
}

// TestMessageTruncatedSanitized 说明字段必须截断、脱敏并保持合法 UTF-8。
func TestMessageTruncatedSanitized(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	msg := "Authorization: Bearer abcdefghijklmnop access_token=supersecret " + strings.Repeat("错误", 300)
	mustAppend(t, store, Record{UID: "u1", Task: TaskCheckin, Trigger: TriggerManual, OK: false, Message: msg})

	got, err := store.Query(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records=%d", len(got.Records))
	}
	message := got.Records[0].Message
	if len(message) > maxMessageBytes {
		t.Errorf("message bytes=%d want <= %d", len(message), maxMessageBytes)
	}
	if !utf8.ValidString(message) {
		t.Error("message must remain valid UTF-8")
	}
	if strings.Contains(message, "abcdefghijklmnop") || strings.Contains(message, "supersecret") {
		t.Errorf("credential leaked: %q", message)
	}
	if !strings.Contains(message, "[redacted]") {
		t.Errorf("expected redaction marker: %q", message)
	}
}

// TestAppendFailureDoesNotPanic 路径不可写时返回错误即可，不能 panic。
func TestAppendFailureDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, dir) // 把目录当文件路径，OpenFile 必然失败
	if err := store.Append(Record{UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true}); err == nil {
		t.Fatal("expected append error for directory path")
	}
	if _, err := store.Query(Filter{}); err != nil {
		t.Fatalf("query after failed append: %v", err)
	}
}

// TestConcurrentAppendQuery 写入与读取并发时无死锁、无数据竞争（配合 -race）。
func TestConcurrentAppendQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.Append(Record{UID: "u" + string(rune('a'+i%5)), Task: TaskCheckin, Trigger: TriggerManual, OK: i%2 == 0})
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = store.Query(Filter{Limit: 20})
		}()
	}
	wg.Wait()
	got, err := store.Query(Filter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 100 {
		t.Fatalf("total=%d want 100", got.Total)
	}
}

// TestQueryDoesNotWaitForSlowAppend 查询只与文件替换的短窗口互斥，不应被写入的
// fsync/整理长时间阻塞（防止重新引入「读写共用一把大锁」）。
func TestQueryDoesNotWaitForSlowAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	mustAppend(t, store, Record{UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true})

	// 人为持有写锁，模拟一次慢 Append；查询必须仍然立即可用。
	store.writeMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.Query(Filter{Limit: 10})
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		store.writeMu.Unlock()
		t.Fatal("Query blocked on write lock")
	}
	store.writeMu.Unlock()
}

// 查询缓存必须：同条件同文件命中、返回副本不污染缓存、追加后立即失效。
func TestQueryCacheInvalidatesOnAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	mustAppend(t, store, Record{UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true})

	first, err := store.Query(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	first.Records[0].UID = "mutated"
	second, err := store.Query(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if second.Records[0].UID != "u1" {
		t.Fatalf("cached result shared slice: %+v", second.Records)
	}

	mustAppend(t, store, Record{UID: "u2", Task: TaskActivity, Trigger: TriggerManual, OK: true})
	after, err := store.Query(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if after.Total != 2 || after.Records[0].UID != "u2" {
		t.Fatalf("append did not invalidate cache: total=%d rows=%+v", after.Total, after.Records)
	}
}

// 并发同条件查询只能扫描一次：第二个请求等待首个扫描结果，而不是重复整文件读。
func TestQueryCacheSingleFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	for i := 0; i < 20; i++ {
		mustAppend(t, store, Record{UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true})
	}
	store.queryMu.Lock() // 模拟首个扫描正在进行
	done := make(chan error, 2)
	go func() {
		_, err := store.Query(Filter{Limit: 10})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		_, err := store.Query(Filter{Limit: 10})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	store.queryMu.Unlock()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// TestCompactionByAgeAndBytes 保留策略同时受时间窗口和字节上限约束。
func TestCompactionByAgeAndBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	store := New(Config{Path: path, MaxBytes: 600, RetentionDays: 2, Now: func() time.Time { return now }})
	for i := 0; i < 20; i++ {
		mustAppend(t, store, Record{Time: now.Add(time.Duration(i) * time.Minute), UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: strings.Repeat("x", 90)})
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 600 {
		got, qerr := store.Query(Filter{Limit: 100})
		if qerr != nil {
			t.Fatal(qerr)
		}
		t.Fatalf("file size=%d want <=600 (records=%d)", fi.Size(), got.Total)
	}
	// 字节上限必须从最旧端裁剪，最新记录不能因搬移前缀而丢失。
	latest, err := store.Query(Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Records) != 1 || latest.Records[0].UID != "u1" {
		t.Fatalf("latest record lost after byte-limit compaction: %+v", latest.Records)
	}
	marker := latest.Records[0].Message
	if marker != strings.Repeat("x", 90) {
		t.Fatalf("latest record corrupted after compact: %q", marker)
	}

	// 时间窗口：推进时钟后，旧记录必须在整理时被丢弃。
	now = now.Add(48 * time.Hour)
	kept, dropped, err := store.Compact()
	if err != nil {
		t.Fatal(err)
	}
	if kept != 0 || dropped == 0 {
		t.Fatalf("kept=%d dropped=%d want all expired", kept, dropped)
	}
}

// TestCompactionRewritesOversizedBadLines 按时间/条数都清不掉的坏行文件，仍必须
// 受字节上限约束（否则坏行会让 JSONL 永久膨胀）。
func TestCompactionRewritesOversizedBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	bad := strings.Repeat(`{"broken":`, 100) + "\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	store := New(Config{Path: path, MaxBytes: 50})
	if _, _, err := store.Compact(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("size=%d want 0 after dropping unreadable oversized file", fi.Size())
	}
}

// TestCompactStreamingPreservesOrderAndDropsBadLines 流式整理必须保持原始追加
// 顺序（最旧→最新），清理坏行/过期行，并让字节上限裁剪发生。
func TestCompactStreamingPreservesOrderAndDropsBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	store := New(Config{Path: path, MaxBytes: 1 << 20, RetentionDays: 2, Now: func() time.Time { return now }})
	mustAppend(t, store, Record{Time: now.Add(-72 * time.Hour), UID: "old", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true})
	mustAppend(t, store, Record{Time: now.Add(-2 * time.Hour), UID: "a", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: "a"})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{bad json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, store, Record{Time: now.Add(-time.Hour), UID: "b", Task: TaskTravel, Trigger: TriggerManual, OK: false, Message: "b"})
	mustAppend(t, store, Record{Time: now, UID: "c", Task: TaskKeepalive, Trigger: TriggerSchedule, OK: true, Message: "c"})

	kept, dropped, err := store.Compact()
	if err != nil {
		t.Fatal(err)
	}
	// 坏行不进入 records 计数，因此 dropped 只统计「解码成功但被保留策略丢弃」
	// 的记录；这里过期记录 1 条、坏行 1 条，只有前者计入。
	if kept != 3 || dropped != 0 {
		t.Fatalf("kept/dropped=%d/%d want 3/0", kept, dropped)
	}
	got, err := store.Query(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 3 {
		t.Fatalf("total=%d want 3", got.Total)
	}
	want := []string{"c", "b", "a"}
	for i, msg := range want {
		if got.Records[i].Message != msg {
			t.Fatalf("record[%d]=%q want %q (records=%+v)", i, got.Records[i].Message, msg, got.Records)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "bad json") {
		t.Fatalf("bad line survived compaction: %q", raw)
	}
}

// 活动历史含账号标识：正式文件/整理临时文件必须 0600，目录 0700，且落盘只保留
// UID 前 8 位，绝不写入完整手机号或账号 UID。
func TestActivityHistoryPermissionsAndUIDMasking(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity-state")
	path := filepath.Join(dir, FileName())
	store := New(Config{Path: path})
	const fullUID = "13800138000"
	if err := store.Append(Record{UID: fullUID, Task: TaskCheckin, Trigger: TriggerManual, OK: true}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("history file mode=%04o want 0600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("history dir mode=%04o want 0700", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fullUID) {
		t.Fatalf("history leaked full uid: %s", raw)
	}
	if !strings.Contains(string(raw), fullUID[:8]) {
		t.Fatalf("history should keep uid8: %s", raw)
	}
}

// 消息正文可能夹带完整 UID（上游错误/请求回显会复制账号标识）。规范化时
// 必须同时清理字段与正文，否则磁盘和面板接口仍会泄露手机号。
func TestActivityMessageMasksFullUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	const fullUID = "13800138000"
	if err := store.Append(Record{
		UID: fullUID, Task: TaskCheckin, Trigger: TriggerManual, OK: false,
		Message: "upstream rejected uid=" + fullUID + " token=abc123",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), fullUID) {
		t.Fatalf("activity file leaked full uid in message: %s", raw)
	}
	got, err := store.Query(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 1 || strings.Contains(got.Records[0].Message, fullUID) {
		t.Fatalf("query leaked full uid: %+v", got.Records)
	}
	if !strings.Contains(got.Records[0].Message, fullUID[:8]) {
		t.Fatalf("message should retain uid8 for diagnosis: %q", got.Records[0].Message)
	}
}

// 同一筛选条件下 limit 200→400→600 应复用一份已扫描的大窗口，而不是每次
// 重扫整份 JSONL；返回内容仍必须与请求 limit 精确一致。
func TestQueryCacheReusesLargerWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 600; i++ {
		mustAppend(t, store, Record{Time: base.Add(time.Duration(i) * time.Second), UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: "m" + strconv.Itoa(i)})
	}
	small, err := store.Query(Filter{UID: "u1", Limit: 200})
	if err != nil || len(small.Records) != 200 || small.Limit != 200 {
		t.Fatalf("small=%+v err=%v", small, err)
	}
	// 大窗口首次扫描后，小窗口与大窗口都应命中同一缓存；大窗口返回 600 条。
	large, err := store.Query(Filter{UID: "u1", Limit: 600})
	if err != nil || len(large.Records) != 600 || large.Limit != 600 {
		t.Fatalf("large=%+v err=%v", large, err)
	}
	again, err := store.Query(Filter{UID: "u1", Limit: 200})
	if err != nil || len(again.Records) != 200 || again.Limit != 200 {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	store.queryCacheMu.Lock()
	n := len(store.queryCache)
	store.queryCacheMu.Unlock()
	if n != 1 {
		t.Fatalf("same filter should share one cache entry, got %d", n)
	}
}

// 命中总数小于请求 limit 时，小窗口缓存已包含全部结果；更大的 limit 不能
// 再整文件重扫，且返回的 Total/Limit 必须与请求一致。
func TestQueryCacheReusesCompleteShortWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := testStore(t, path)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 7; i++ {
		mustAppend(t, store, Record{Time: base.Add(time.Duration(i) * time.Second), UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: "m" + strconv.Itoa(i)})
	}
	// 首次请求 limit=100 只命中 7 条，缓存窗口已包含全部匹配记录。
	first, err := store.Query(Filter{UID: "u1", Limit: 100})
	if err != nil || len(first.Records) != 7 || first.Total != 7 || first.Limit != 100 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// 持查询读锁：若第二次仍去扫文件会阻塞在 queryMu.RLock 上；命中缓存则
	// 直接返回，验证「命中数不足 limit」的更大 limit 不再重复扫描。
	store.queryMu.Lock()
	done := make(chan struct{})
	var all QueryResult
	var qerr error
	go func() {
		defer close(done)
		all, qerr = store.Query(Filter{UID: "u1", Limit: 200})
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		store.queryMu.Unlock()
		t.Fatal("complete short window should be served from cache without rescanning")
	}
	store.queryMu.Unlock()
	if qerr != nil || len(all.Records) != 7 || all.Total != 7 || all.Limit != 200 {
		t.Fatalf("all=%+v err=%v", all, qerr)
	}
}

func TestAsyncFlushMakesAllAcceptedRowsVisibleWithoutClosing(t *testing.T) {
	s := New(Config{Path: filepath.Join(t.TempDir(), FileName()), Async: true})
	t.Cleanup(func() { _ = s.Close() })
	for _, total := range []int{600, 1} {
		for i := 0; i < total; i++ {
			mustAppend(t, s, Record{UID: "u1", Task: TaskGrowth, OK: true})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.Flush(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Query(Filter{Limit: 1000})
		want := 600
		if total == 1 {
			want = 601
		}
		if err != nil || got.Total != want {
			t.Fatalf("after flush total=%d want=%d err=%v", got.Total, want, err)
		}
	}
}

package reqlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRecorderMetricsAndRecent(t *testing.T) {
	r := New(Config{})
	r.Begin()
	r.Record(Event{RequestID: "ok", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 10})
	r.Begin()
	r.Record(Event{RequestID: "bad", Status: 429, OK: false, Outcome: OutcomeHTTPError, DurationMs: 30})

	s := r.Snapshot()
	if s.Completed != 2 || s.InFlight != 0 || s.Succeeded != 1 || s.Failed != 1 {
		t.Fatalf("counts = %+v", s)
	}
	if s.SuccessRate != 50 || s.HTTPSuccessRate != 50 || s.AvgDurationMs != 20 {
		t.Fatalf("rates = success:%v http:%v avg:%v", s.SuccessRate, s.HTTPSuccessRate, s.AvgDurationMs)
	}
	if len(s.Recent) != 2 || s.Recent[0].RequestID != "bad" || s.Recent[1].RequestID != "ok" {
		t.Fatalf("recent = %+v, want newest first", s.Recent)
	}
}

// TestRecorderRecentRingOverwritesOldest recent 必须只保留最近 100 条且最新在前；
// 满环后第 101 条应覆盖最旧一条，不能丢新事件或倒序错位。
func TestRecorderRecentRingOverwritesOldest(t *testing.T) {
	r := New(Config{})
	for i := 0; i < recentLimit+25; i++ {
		r.Record(Event{RequestID: "r" + strconv.Itoa(i), Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	got := r.Snapshot().Recent
	if len(got) != recentLimit {
		t.Fatalf("recent len=%d want %d", len(got), recentLimit)
	}
	if got[0].RequestID != "r"+strconv.Itoa(recentLimit+24) {
		t.Fatalf("newest=%s want r%d", got[0].RequestID, recentLimit+24)
	}
	if got[recentLimit-1].RequestID != "r25" {
		t.Fatalf("oldest=%s want r25", got[recentLimit-1].RequestID)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].RequestID == got[i].RequestID {
			t.Fatalf("duplicate recent entry at %d: %s", i, got[i].RequestID)
		}
	}
}

// TestRecorderZeroValueRecord 零值 Recorder 曾靠 append 自动扩容；改成环形缓冲后
// 必须保持同样的可用性，不能在空切片上写入 panic。
func TestRecorderZeroValueRecord(t *testing.T) {
	var r Recorder
	r.Record(Event{RequestID: "zero", Status: 200, OK: true, Outcome: OutcomeSuccess})
	got := r.Snapshot().Recent
	if len(got) != 1 || got[0].RequestID != "zero" {
		t.Fatalf("zero-value recorder recent=%+v want one event", got)
	}
}

func BenchmarkRecorderRecord(b *testing.B) {
	r := New(Config{})
	e := Event{Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 12}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Record(e)
	}
}

func TestArchiveRotationReadAndFilter(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 120, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 12; i++ {
		r.Record(Event{
			Time:       base.Add(time.Duration(i) * time.Second),
			RequestID:  "multi-" + string(rune('a'+i)),
			Model:      "glm-5.3",
			Account:    "账号(uid8)",
			Status:     200,
			OK:         true,
			Outcome:    OutcomeSuccess,
			DurationMs: int64(i + 1),
		})
	}
	r.Record(Event{
		Time:       base.Add(20 * time.Second),
		RequestID:  "other",
		Model:      "other-model",
		Status:     500,
		Outcome:    OutcomeHTTPError,
		DurationMs: 99,
	})
	r.Close()

	stats := r.Snapshot().Archive
	if !stats.Enabled || stats.Files < 2 || stats.Bytes == 0 || stats.DroppedWrites != 0 {
		t.Fatalf("archive stats = %+v", stats)
	}
	rows, err := r.ReadArchive(5, Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].RequestID != "multi-l" || rows[4].RequestID != "multi-h" {
		t.Fatalf("filtered rows = %+v", rows)
	}
}

// ReadArchive 的契约是「按事件时间倒序返回最近记录」，与归档文件的 mtime 无关。
// 快速轮转（FileMaxBytes 很小）时同一秒内会写出多个文件，Linux 下这些文件的 mtime
// 可能完全相同；把归档目录整体拷贝 / 恢复备份也会打乱 mtime。此用例把所有归档文件的
// mtime 改成与事件时间相反的顺序，钉住「只能按事件时间排序」。
func TestReadArchiveOrdersByEventTimeNotFileMtime(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, FileMaxBytes: 1, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Hour)
	const n = 5
	for i := 0; i < n; i++ {
		r.Record(Event{
			Time:      base.Add(time.Duration(i) * time.Second),
			RequestID: "ev-" + string(rune('a'+i)),
			Model:     "glm-5.3",
			Status:    200,
			OK:        true,
			Outcome:   OutcomeSuccess,
		})
	}
	r.Close()

	// FileMaxBytes=1 让每个事件独占一个归档文件：基准文件 requests-<day>.jsonl 装的是
	// 最早的事件，字典序却排在 requests-<day>.N.jsonl 之后。按 os.ReadDir 顺序递增地
	// 设置 mtime，最早事件所在文件就拿到最大 mtime、被放在最后读取。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("归档文件数 = %d, want %d", len(entries), n)
	}
	stamp := time.Unix(1700000000, 0)
	for i, e := range entries {
		ts := stamp.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(filepath.Join(dir, e.Name()), ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := r.ReadArchive(n, Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(rows))
	for _, e := range rows {
		got = append(got, e.RequestID)
	}
	if len(rows) != n {
		t.Fatalf("rows = %d, want %d", len(rows), n)
	}
	for i := range rows {
		want := "ev-" + string(rune('a'+n-1-i))
		if rows[i].RequestID != want {
			t.Fatalf("rows[%d] = %s, want %s（须按事件时间倒序，与文件 mtime 无关）\n got %v", i, rows[i].RequestID, want, got)
		}
	}
}

// 来源字段随事件落盘并可被 client_ip / user_agent 过滤（包含匹配、大小写不敏感）。
func TestArchiveFilterByClientInfo(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	rows := []Event{
		{RequestID: "a", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "203.0.113.7", UserAgent: "python-requests/2.31.0"},
		{RequestID: "b", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "198.51.100.4", UserAgent: "Mozilla/5.0 Chrome/120.0.0.0"},
		{RequestID: "c", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess},
	}
	for i, e := range rows {
		e.Time = base.Add(time.Duration(i) * time.Second)
		r.Record(e)
	}
	r.Close()

	got, err := r.ReadArchive(10, Filter{ClientIP: "203.0.113."})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "a" {
		t.Fatalf("client ip filter = %+v", got)
	}
	if got[0].UserAgent != "python-requests/2.31.0" {
		t.Fatalf("user agent not persisted: %+v", got[0])
	}

	// UA 片段过滤大小写不敏感（面板搜索框直接传用户输入）。
	got, err = r.ReadArchive(10, Filter{UserAgent: "chrome/120"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("ua filter = %+v", got)
	}

	// 来源为空的旧事件不该被非空来源条件误命中。
	got, err = r.ReadArchive(10, Filter{ClientIP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unmatched ip filter = %+v", got)
	}
}

// 时间区间过滤（「今天」/「自定义」）：闭区间，任一侧为零值即该侧不设界。
func TestArchiveFilterByTimeRange(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 30})
	base := time.Now().Add(-72 * time.Hour).Truncate(time.Minute)
	ids := []string{"d3", "d2", "d1", "now"}
	for i, id := range ids {
		r.Record(Event{
			Time: base.Add(time.Duration(i) * 24 * time.Hour), RequestID: id,
			Status: 200, OK: true, Outcome: OutcomeSuccess,
		})
	}
	r.Close()

	rows, err := r.ReadArchive(10, Filter{From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(rows))
	for _, e := range rows {
		got = append(got, e.RequestID)
	}
	// 倒序返回，闭区间命中 d2、d1。
	if len(got) != 2 || got[0] != "d1" || got[1] != "d2" {
		t.Fatalf("时间区间过滤 = %v, want [d1 d2]", got)
	}

	onlyFrom, err := r.ReadArchive(10, Filter{From: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyFrom) != 2 || onlyFrom[0].RequestID != "now" {
		t.Fatalf("仅 From 过滤 = %+v, want 2 条（d1、now）", onlyFrom)
	}

	onlyTo, err := r.ReadArchive(10, Filter{To: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyTo) != 1 || onlyTo[0].RequestID != "d3" {
		t.Fatalf("仅 To 过滤 = %+v, want 仅 d3", onlyTo)
	}

	// 时间区间与其它条件是与关系。
	combined, err := r.ReadArchive(10, Filter{
		From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour), Outcome: OutcomeHTTPError,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != 0 {
		t.Fatalf("区间+结果组合应为空（这几条都是成功）: %+v", combined)
	}
}

func TestArchiveQueueDropCounter(t *testing.T) {
	w := &archiveWriter{
		cfg:  Config{Enabled: true, Dir: t.TempDir()},
		ch:   make(chan Event, 1),
		done: make(chan struct{}),
	}
	w.ch <- Event{RequestID: "occupied"}
	w.enqueue(Event{RequestID: "dropped"})
	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("dropped=%d want 1", got)
	}
}

func TestArchivePruneHonorsSize(t *testing.T) {
	dir := t.TempDir()
	for i, name := range []string{"requests-2026-09-20.jsonl", "requests-2026-09-21.jsonl", "requests-2026-09-22.jsonl"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().AddDate(0, 0, -10+i)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	w := &archiveWriter{cfg: Config{Enabled: true, Dir: dir, RetentionDays: 30, MaxBytes: 50}, done: make(chan struct{})}
	w.prune()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "requests-2026-09-22.jsonl" {
		t.Fatalf("remaining = %+v, want newest file only", entries)
	}
}

// 统计缓存必须随 prune 失效：若在删除前刚好统计过目录，TTL 内继续返回旧
// Files/Bytes 会让面板显示已清理的归档仍在占用空间。
func TestArchiveStatsCacheInvalidatedAfterPrune(t *testing.T) {
	dir := t.TempDir()
	ages := []time.Duration{-10 * 24 * time.Hour, -24 * time.Hour}
	for i, name := range []string{"requests-2026-09-20.jsonl", "requests-2026-09-21.jsonl"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(ages[i])
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	w := &archiveWriter{cfg: Config{Enabled: true, Dir: dir, RetentionDays: 5, MaxBytes: 1 << 20}, done: make(chan struct{})}
	if got := w.stats(); got.Files != 2 || got.Bytes != 20 {
		t.Fatalf("initial stats = %+v", got)
	}
	w.prune()
	if _, err := os.Stat(filepath.Join(dir, "requests-2026-09-20.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("prune should remove the oldest file, stat err=%v", err)
	}
	if got := w.stats(); got.Files != 1 || got.Bytes != 10 {
		t.Fatalf("stats after prune = %+v, want one 10-byte file", got)
	}
}

// TestReadArchiveLimitKeepsBoundedPerFile 单文件远大于 limit 时，读取窗口必须
// 有界，且事件时间与落盘顺序不一致时仍返回全文件最新 limit 条。
func TestReadArchiveLimitKeepsBoundedPerFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	base := time.Now().Add(-24 * time.Hour)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 400 条乱序写入：第 i 条的时间为 (i*37)%400 秒，确保不能按行号直接截断。
	for i := 0; i < 400; i++ {
		e := Event{Time: base.Add(time.Duration((i*37)%400) * time.Second), RequestID: "r" + strconv.Itoa(i), Outcome: OutcomeSuccess, Status: 200, OK: true}
		raw, _ := json.Marshal(e)
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := readFileLimit(path, Filter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("rows=%d want 10", len(rows))
	}
	newest := base.Add(399 * time.Second)
	if !rows[0].Time.Equal(newest) {
		t.Fatalf("newest=%v want %v", rows[0].Time, newest)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Time.After(rows[i-1].Time) {
			t.Fatalf("not descending at %d: %v > %v", i, rows[i].Time, rows[i-1].Time)
		}
	}
}

// 跨归档文件读取时，每个文件只能领用「limit 减去已收集条数」的窗口。否则每个
// 旧文件都各读 limit 条再拼接，内存会变成 文件数×limit，虽然最终截断正确，
// 但轮询期间的分配会随归档数量增长。
func TestReadArchiveLimitShrinksAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	files := []string{"requests-2026-10-17.jsonl", "requests-2026-10-18.jsonl"}
	for fi, name := range files {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 50; i++ {
			e := Event{Time: base.Add(time.Duration(fi*3600+i) * time.Second), RequestID: name + "-" + strconv.Itoa(i)}
			raw, _ := json.Marshal(e)
			if _, err := f.Write(append(raw, '\n')); err != nil {
				t.Fatal(err)
			}
		}
		_ = f.Close()
	}
	w := &archiveWriter{cfg: Config{Enabled: true, Dir: dir}, done: make(chan struct{})}
	rows, err := w.read(10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("rows=%d want 10", len(rows))
	}
	if rows[0].RequestID != files[1]+"-49" {
		t.Fatalf("newest=%s want %s-49", rows[0].RequestID, files[1])
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Time.After(rows[i-1].Time) {
			t.Fatalf("not descending at %d", i)
		}
	}
}

// 归档文件名日期不保证与事件时间一致（恢复备份、补写历史、手工归档）。即使
// 新文件名里的记录较旧，也不能读到 limit 条就停止，否则会漏掉旧文件名里更新
// 的事件。这里 limit=1：先读的文件只有 stale 事件，后面文件才有真正最新事件。
func TestReadArchiveDoesNotStopBeforeNewerEventInOlderFile(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	write := func(name string, events []Event) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, e := range events {
			raw, _ := json.Marshal(e)
			if _, err := f.Write(append(raw, '\n')); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("requests-2026-10-18.jsonl", []Event{{Time: base, RequestID: "stale-filename-new"}})
	write("requests-2026-10-17.jsonl", []Event{{Time: base.Add(time.Minute), RequestID: "recent-filename-old"}})

	w := &archiveWriter{cfg: Config{Enabled: true, Dir: dir}, done: make(chan struct{})}
	rows, err := w.read(1, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RequestID != "recent-filename-old" {
		t.Fatalf("rows=%+v want the newest event from the older filename", rows)
	}
}

// TestReadFileLimitStableEqualTimes 同一时刻的事件超过窗口时，必须保留落盘顺序
// 中最新的 limit 条；批量裁剪不能把同刻事件顺序打乱。readFileLimit 的结果会
// 再经全局 Stable 排序，因此这里直接验证局部窗口的稳定契约。
func TestReadFileLimitStableEqualTimes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Truncate(time.Second)
	for i := 0; i < 120; i++ {
		raw, _ := json.Marshal(Event{Time: ts, RequestID: "same-" + strconv.Itoa(i)})
		_, _ = f.Write(append(raw, '\n'))
	}
	f.Close()
	rows, err := readFileLimit(path, Filter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("rows=%d want 10", len(rows))
	}
	// 全量 Stable 排序在同一时刻保留落盘正序；截断到最近 10 条时，同刻集合
	// 的语义由 readFile 契约决定（最早落盘的同刻事件优先保留）。
	for i, row := range rows {
		if row.RequestID != "same-"+strconv.Itoa(i) {
			t.Fatalf("row[%d]=%s want same-%d", i, row.RequestID, i)
		}
	}
}

// BenchmarkReadFileLimitOneMiB 10k 行文件、只取最近 200 条：验证窗口读法
// 不会把整个文件都留在内存里。旧实现分配随行数增长，新实现与 limit 同阶。
func BenchmarkReadFileLimitOneMiB(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	base := time.Now().Add(-10 * time.Hour)
	bw := bufio.NewWriter(f)
	for i := 0; i < 10000; i++ {
		e := Event{Time: base.Add(time.Duration(i) * time.Millisecond), RequestID: "r" + strconv.Itoa(i), Model: "glm-5.3", Outcome: OutcomeSuccess, Status: 200, OK: true}
		raw, _ := json.Marshal(e)
		_, _ = bw.Write(append(raw, '\n'))
	}
	_ = bw.Flush()
	_ = f.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows, err := readFileLimit(path, Filter{}, 200); err != nil || len(rows) != 200 {
			b.Fatalf("rows=%d err=%v", len(rows), err)
		}
	}
}

// TestReadFileLimitDiffAgainstFullRead 窗口读法必须与「全量读取 + Stable 倒序 +
// 截断」逐条一致，覆盖多批同刻事件（旧测试只覆盖单批，漏掉了裁剪后同刻继续
// 到达的乱序场景）。
func TestReadFileLimitDiffAgainstFullRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	rnd := rand.New(rand.NewSource(0))
	for i := 0; i < 500; i++ {
		second := rnd.Intn(40) // 大量同刻事件
		e := Event{Time: base.Add(time.Duration(second) * time.Second), RequestID: "r" + strconv.Itoa(i)}
		raw, _ := json.Marshal(e)
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	full, err := readFile(path, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	full = append([]Event(nil), full...)
	if len(full) > 1 {
		sortArchiveNewestFirst(full)
	}
	for _, keep := range []int{1, 2, 3, 7, 16, 50, 200} {
		got, err := readFileLimit(path, Filter{}, keep)
		if err != nil {
			t.Fatal(err)
		}
		want := full
		if len(want) > keep {
			want = want[:keep]
		}
		if len(got) != len(want) {
			t.Fatalf("keep=%d len=%d want %d", keep, len(got), len(want))
		}
		for i := range want {
			if got[i].RequestID != want[i].RequestID || !got[i].Time.Equal(want[i].Time) {
				t.Fatalf("keep=%d row[%d]=%s@%v want %s@%v", keep, i, got[i].RequestID, got[i].Time, want[i].RequestID, want[i].Time)
			}
		}
	}
}

// TestReadFileLimitFastPathMatchesSlowPath 钉住快速预筛与旧完整解码路径的逐条
// 等价性：转义字符串、坏行、未知字段、同刻乱序、时间窗口与所有过滤维度都必须
// 完全一致。快速路径只负责缩小候选窗口，不能改变最终返回集合与顺序。
func TestReadFileLimitFastPathMatchesSlowPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	lines := []string{
		`{"time":"2026-10-18T00:00:00Z","request_id":"a","model":"glm-5.3","account":"Alice(12345678)","outcome":"success","status":200,"ok":true}`,
		`not-json`,
		`{"time":"2026-10-18T00:00:01Z","request_id":"escaped","model":"glm-5.3","account":"A\u006cice(12345678)","outcome":"success"}`,
		`{"time":"2026-10-18T00:00:02Z","request_id":"extra","model":"glm-5.3","meta":{"a":[1,2,{"b":"x,y"}]},"client_ip":"203.0.113.7","user_agent":"Python-Requests/2.31","outcome":"http_error","key_id":"Team-A"}`,
		`{"time":"2026-10-18T00:00:03Z","request_id":"dup","model":"glm-5.3","model":"kimi-k3","outcome":"success"}`,
		`{"time":"2026-10-18T00:00:04.123456789+08:00","request_id":"offset","model":"KIMI-K3","account":"bob(87654321)","outcome":"success"}`,
		`{"time":"2026-10-17T23:59:59Z","request_id":"outside","model":"glm-5.3","outcome":"success"}`,
		`{"time":"2026-10-18T00:00:05Z","request_id":"missing-outcome","model":"glm-5.3"}`,
		`{"time":"2026-10-18T00:00:06Z","request_id":"null-model","model":null,"outcome":"success"}`,
		`{"time":"2026-10-18T00:00:07Z","request_id":"bad-time","model":"glm-5.3","outcome":"success"} trailing`,
		`{"time":"2026-10-18T00:00:00Z","request_id":"same-1","model":"glm-5.3","outcome":"success"}`,
		`{"time":"2026-10-18T00:00:00Z","request_id":"same-2","model":"glm-5.3","outcome":"success"}`,
		`{"time":"2026-10-18T00:00:00Z","request_id":"same-3","model":"glm-5.3","outcome":"success"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	filters := []Filter{
		{},
		{Model: "GLM", From: base, To: base.Add(10 * time.Second)},
		{Account: "alice", Outcome: OutcomeSuccess},
		{ClientIP: "203.0.113", UserAgent: "python-requests"},
		{KeyID: "team-a"},
		{Model: "KIMI", From: base.Add(time.Second)},
	}
	for _, filter := range filters {
		for _, keep := range []int{0, 1, 2, 3, 7, 200} {
			got, gotErr := readFileLimit(path, filter, keep)
			want, wantErr := readFileLimitSlow(f, filter.normalized(), keep)
			describe := func(err error) string {
				if err == nil {
					return "<nil>"
				}
				return err.Error()
			}
			if describe(gotErr) != describe(wantErr) {
				t.Fatalf("filter=%+v keep=%d err=%v want %v", filter, keep, gotErr, wantErr)
			}
			if len(got) != len(want) {
				t.Fatalf("filter=%+v keep=%d len=%d want %d (got=%+v want=%+v)", filter, keep, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i].RequestID != want[i].RequestID || !got[i].Time.Equal(want[i].Time) {
					t.Fatalf("filter=%+v keep=%d row[%d]=%s@%v want %s@%v", filter, keep, i, got[i].RequestID, got[i].Time, want[i].RequestID, want[i].Time)
				}
			}
		}
	}
}

// TestReadFileLimitFastPathRandomized 用随机字段组合和窗口边界批量比对两条
// 路径，防止手写预筛在空字段、同刻、乱序、超窗等组合上出现细微偏差。
func TestReadFileLimitFastPathRandomized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	rnd := rand.New(rand.NewSource(42))
	models := []string{"", "glm-5.3", "GLM-5.3", "kimi-k3", "deepseek-v4-pro", "含中文"}
	outcomes := []string{"", OutcomeSuccess, OutcomeHTTPError, OutcomeInterrupted}
	ips := []string{"", "203.0.113.7", "198.51.100.4"}
	uas := []string{"", "python-requests/2.31", "Mozilla/5.0 Chrome/120"}
	var lines []string
	for i := 0; i < 700; i++ {
		// 少量时间值制造大量同刻样本；再混入一条坏行与一条窗口外记录。
		at := base.Add(time.Duration(rnd.Intn(25)) * time.Second)
		e := Event{
			Time:      at,
			RequestID: "r" + strconv.Itoa(i),
			Model:     models[rnd.Intn(len(models))],
			Account:   []string{"", "alice(12345678)", "bob(87654321)"}[rnd.Intn(3)],
			Outcome:   outcomes[rnd.Intn(len(outcomes))],
			ClientIP:  ips[rnd.Intn(len(ips))],
			UserAgent: uas[rnd.Intn(len(uas))],
			Status:    200,
			OK:        true,
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
		if i%137 == 0 {
			lines = append(lines, "not-json")
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	filters := []Filter{
		{},
		{Model: "GLM"},
		{Account: "alice", Outcome: OutcomeSuccess},
		{ClientIP: "203.0.113", UserAgent: "python"},
		{From: base.Add(3 * time.Second)},
		{From: base.Add(2 * time.Second), To: base.Add(9 * time.Second), Model: "kimi"},
	}
	for fi, filter := range filters {
		for _, keep := range []int{1, 2, 5, 17, 64, 400} {
			got, gotErr := readFileLimit(path, filter, keep)
			want, wantErr := readFileLimitSlow(f, filter.normalized(), keep)
			if gotErr != nil || wantErr != nil {
				t.Fatalf("filter[%d]=%+v keep=%d errors: %v / %v", fi, filter, keep, gotErr, wantErr)
			}
			if len(got) != len(want) {
				t.Fatalf("filter[%d]=%+v keep=%d len=%d want %d", fi, filter, keep, len(got), len(want))
			}
			for i := range want {
				if got[i].RequestID != want[i].RequestID || !got[i].Time.Equal(want[i].Time) {
					t.Fatalf("filter[%d]=%+v keep=%d row[%d]=%s want %s", fi, filter, keep, i, got[i].RequestID, want[i].RequestID)
				}
			}
		}
	}
}

// TestReadArchiveFastPathFallsBackOnNonASCIIFilter 非 ASCII 过滤词不能走字节
// 小写匹配；结果仍须与完整解码 + strings.ToLower 的语义一致。
func TestReadArchiveFastPathFallsBackOnNonASCIIFilter(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	r.Record(Event{Time: base, RequestID: "zh", Account: "账号A(12345678)", Model: "glm-5.3", Outcome: OutcomeSuccess, Status: 200, OK: true})
	r.Record(Event{Time: base.Add(time.Second), RequestID: "en", Account: "alice(87654321)", Model: "glm-5.3", Outcome: OutcomeSuccess, Status: 200, OK: true})
	r.Close()

	got, err := r.ReadArchive(10, Filter{Account: "账号a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "zh" {
		t.Fatalf("non-ascii filter = %+v", got)
	}
}

// TestReadArchiveFastPathFallsBackOnNonASCIIField 事件字段含非 ASCII 时，即使
// 过滤词是 ASCII 也必须回退完整解码：strings.ToLower 可能把非 ASCII 字符映射
// 成 ASCII（例如 Kelvin 符号 → k），字节匹配会漏掉这类命中。
func TestReadArchiveFastPathFallsBackOnNonASCIIField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-18.jsonl")
	lines := []string{
		`{"time":"2026-10-18T00:00:00Z","request_id":"kelvin","model":"\u212Aimi-k3","outcome":"success"}`,
		`{"time":"2026-10-18T00:00:01Z","request_id":"plain","model":"kimi-k3","outcome":"success"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, err := readFileLimit(path, Filter{Model: "kimi"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want, err := readFileLimitSlow(f, Filter{Model: "kimi"}.normalized(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("fast path rows=%d want %d (%+v vs %+v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].RequestID != want[i].RequestID {
			t.Fatalf("row[%d]=%s want %s", i, got[i].RequestID, want[i].RequestID)
		}
	}
}

// TestEventErrorFieldsJSONRoundTrip 排障字段必须按 JSON tag 序列化且零值省略，
// 归档读取后原样恢复（ErrorMsg 在 Record 入队前已截断到 300 字节）。
func TestEventErrorFieldsJSONRoundTrip(t *testing.T) {
	full := Event{
		ErrorCode:      "rate_limit_exceeded",
		ErrorKind:      "soft_rate",
		UpstreamStatus: 429,
		ErrorMsg:       "upstream says slow down",
		ModelBlocked:   true,
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"error_code", "error_kind", "upstream_status", "error_msg", "model_blocked"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("field %s omitted: %s", key, raw)
		}
	}
	if got["error_code"] != "rate_limit_exceeded" || got["error_kind"] != "soft_rate" ||
		got["upstream_status"] != float64(429) || got["model_blocked"] != true {
		t.Fatalf("roundtrip values = %v", got)
	}

	// 零值字段全部省略。
	zero, err := json.Marshal(Event{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"error_code", "error_kind", "upstream_status", "error_msg", "model_blocked"} {
		if bytes.Contains(zero, []byte(`"`+key+`"`)) {
			t.Fatalf("zero-value %s should be omitted: %s", key, zero)
		}
	}

	// 归档往返：写入文件再读出，字段保持一致。
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-2026-10-24.jsonl")
	line, _ := json.Marshal(full)
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := readFileLimit(path, Filter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ErrorCode != full.ErrorCode || rows[0].ErrorKind != full.ErrorKind ||
		rows[0].UpstreamStatus != full.UpstreamStatus || rows[0].ErrorMsg != full.ErrorMsg || !rows[0].ModelBlocked {
		t.Fatalf("archive roundtrip = %+v", rows)
	}
}

// TestRecorderTruncatesErrorMsg 超长错误原文在 Record 入队/落盘前按 UTF-8 rune
// 边界截断到 300 字节，不能切出半截多字节序列。
func TestRecorderTruncatesErrorMsg(t *testing.T) {
	// 350 个 3 字节汉字 + 尾标：确保截断点落在多字节字符中间。
	long := strings.Repeat("错", 350)
	r := New(Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	r.Record(Event{RequestID: "long", Status: 500, Outcome: OutcomeHTTPError, ErrorCode: "upstream_parse", ErrorMsg: long})
	r.Close()

	rows, err := r.ReadArchive(10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d want 1", len(rows))
	}
	got := rows[0].ErrorMsg
	if len(got) > 300 {
		t.Fatalf("error_msg len=%d want <=300", len(got))
	}
	if !strings.HasPrefix(long, got) {
		t.Fatalf("error_msg must be a prefix of the original: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("error_msg is not valid UTF-8: %q", got)
	}
	// 内存指标同样存截断后的值。
	recent := r.Snapshot().Recent
	if len(recent) != 1 || recent[0].ErrorMsg != got {
		t.Fatalf("recent error_msg not truncated: %+v", recent)
	}
}

// TestArchiveFilterByErrorCode error_code 支持包含匹配（大小写不敏感）。
func TestArchiveFilterByErrorCode(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	r.Record(Event{Time: base, RequestID: "rate", Outcome: OutcomeHTTPError, Status: 429, ErrorCode: "rate_limit_exceeded"})
	r.Record(Event{Time: base.Add(time.Second), RequestID: "blocked", Outcome: OutcomeHTTPError, Status: 400, ErrorCode: "content_blocked"})
	r.Close()

	got, err := r.ReadArchive(10, Filter{ErrorCode: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "rate" {
		t.Fatalf("error_code filter = %+v", got)
	}
	// 大小写不敏感。
	got, err = r.ReadArchive(10, Filter{ErrorCode: "RATE_LIMIT"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "rate" {
		t.Fatalf("case-insensitive error_code filter = %+v", got)
	}
}

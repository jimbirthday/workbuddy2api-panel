package reqlog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadArchiveChangedReusesUnchangedWindow 目录签名不变时，第二次查询必须命中
// 缓存并返回 changed=false；追加一条记录后签名变化，结果必须刷新且 changed=true。
func TestReadArchiveChangedReusesUnchangedWindow(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	r.Record(Event{Time: base, RequestID: "r1", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 10})
	r.Close()

	first, changed, err := r.ReadArchiveChanged(10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(first) != 1 || first[0].RequestID != "r1" {
		t.Fatalf("first=%+v changed=%v", first, changed)
	}

	second, changed, err := r.ReadArchiveChanged(10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("unchanged directory must report changed=false")
	}
	if len(second) != 1 || second[0].RequestID != "r1" {
		t.Fatalf("cached window=%+v", second)
	}
	// 返回副本：调用方截断/追加不能污染缓存。
	second[0].RequestID = "mutated"
	third, _, _ := r.ReadArchiveChanged(10, Filter{})
	if len(third) != 1 || third[0].RequestID != "r1" {
		t.Fatalf("cache polluted by caller mutation: %+v", third)
	}

	// 面板默认轮询使用 IfChanged：未变化时不复制整窗。
	light, changed, err := r.ReadArchiveChangedIfChanged(10, Filter{})
	if err != nil || changed || light != nil {
		t.Fatalf("if-changed unchanged read=%+v changed=%v err=%v", light, changed, err)
	}

	// 追加一条新事件（直接写文件，模拟归档 writer 落盘）。
	path := filepath.Join(dir, "requests-"+base.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"time":"` + base.Add(time.Minute).Format(time.RFC3339Nano) + `","request_id":"r2","status":200,"ok":true,"outcome":"success","duration_ms":20}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	updated, changed, err := r.ReadArchiveChanged(10, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(updated) != 2 || updated[0].RequestID != "r2" {
		t.Fatalf("updated=%+v changed=%v", updated, changed)
	}
}

// TestReadArchiveChangedSeparatesFilterWindows 同一目录下不同筛选条件不能互相命中
// 对方的缓存；否则切换账号/模型筛选会显示上一组结果。
func TestReadArchiveChangedSeparatesFilterWindows(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	r.Record(Event{Time: base, RequestID: "a", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Record(Event{Time: base.Add(time.Second), RequestID: "b", Model: "kimi-k3", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Close()

	if rows, changed, _ := r.ReadArchiveChanged(10, Filter{Model: "glm"}); !changed || len(rows) != 1 || rows[0].RequestID != "a" {
		t.Fatalf("glm rows=%+v changed=%v", rows, changed)
	}
	if rows, changed, _ := r.ReadArchiveChanged(10, Filter{Model: "kimi"}); !changed || len(rows) != 1 || rows[0].RequestID != "b" {
		t.Fatalf("kimi rows=%+v changed=%v", rows, changed)
	}
}

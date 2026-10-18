package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

func TestScanLockedJoinsLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	long := strings.Repeat("x", 120<<10)
	rec := Record{Time: time.Now(), UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: long}
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	store := testStore(t, path)
	var got []Record
	if err := store.scanLocked(func(r Record) { got = append(got, r) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Message) != 200 {
		t.Fatalf("long line not decoded: len=%d msg=%d", len(got), len(got[0].Message))
	}
}

func TestScanLockedPreservesShortLinesAfterLong(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	bw := bufio.NewWriter(f)
	for _, n := range []int{10, 120 << 10, 20, 130 << 10, 30} {
		rec := Record{Time: time.Now(), UID: "u", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: strings.Repeat("y", n)}
		line, _ := json.Marshal(rec)
		if _, err := bw.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store := testStore(t, path)
	count := 0
	if err := store.scanLocked(func(r Record) { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("count=%d want 5", count)
	}
}

func TestScanLockedHalfLineAtEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	rec := Record{Time: time.Now(), UID: "u1", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true}
	line, _ := json.Marshal(rec)
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}
	store := testStore(t, path)
	var got []Record
	if err := store.scanLocked(func(r Record) { got = append(got, r) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("half line should still decode: %d", len(got))
	}
}

func TestConcurrentCompactAndQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName())
	store := New(Config{Path: path, MaxBytes: 1 << 20, RetentionDays: 90})
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 200; i++ {
		mustAppend(t, store, Record{Time: base.Add(time.Duration(i) * time.Second), UID: "u", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true})
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, _, err := store.Compact(); err != nil {
					t.Errorf("compact: %v", err)
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := store.Query(Filter{Limit: 100}); err != nil {
					t.Errorf("query: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

func legacySanitizeMessage(msg string) string {
	msg = strings.Join(strings.Fields(strings.TrimSpace(msg)), " ")
	msg = bearerRE.ReplaceAllString(msg, "$1 [redacted]")
	msg = secretKVRE.ReplaceAllString(msg, "$1=[redacted]")
	return logfmt.Truncate(msg, maxMessageBytes)
}

func TestSanitizeMessageMatchesLegacy(t *testing.T) {
	cases := []string{
		"",
		"  plain message  ",
		"multiple   spaces\tand\nnewlines",
		"中文　全角空格 与\t制表",
		"Authorization: Bearer abcdefghijklmnop",
		"authorization: bearer abcdefghijklmnop",
		"Bearer abcdefghijklmnop",
		"Basic dXNlcjpwYXNz",
		"Access_Token=supersecret",
		"access-token: supersecret",
		"refresh_token = abc123",
		"API_KEY=xyz",
		"api-key: xyz",
		"apikey=xyz",
		"password: hunter2",
		"secret=top",
		"cookie: sid=1",
		"quoted \"access_token=abc\" tail",
		"普通错误，不含敏感词",
		strings.Repeat("长消息 ", 100) + " access_token=tailsecret",
	}
	for _, in := range cases {
		want := legacySanitizeMessage(in)
		got := sanitizeMessage(in)
		if got != want {
			t.Fatalf("sanitize mismatch for %q:\n got=%q\nwant=%q", in, got, want)
		}
	}
}

func TestCompactTempFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName())
	now := time.Now()
	store := New(Config{Path: path, MaxBytes: 1 << 20, RetentionDays: 90, Now: func() time.Time { return now }})
	for i := 0; i < 20; i++ {
		mustAppend(t, store, Record{Time: now, UID: "u", Task: TaskCheckin, Trigger: TriggerSchedule, OK: true, Message: "m"})
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 让同目录临时文件无法创建：整理必须返回错误，且原 JSONL 保持不变。
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, _, err := store.Compact(); err == nil {
		t.Skip("directory permissions not enforced on this platform")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed compaction must not modify the original file")
	}
}

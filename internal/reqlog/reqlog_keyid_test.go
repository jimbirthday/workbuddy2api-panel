package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// key_id 是调用方身份 id，不是密钥：归档/筛选/导出都必须保留 id，且不得写入
// Authorization 或任何 key 原文。这里用显式密钥形状验证“只写 id”。
func TestArchiveKeyIDAndNoSecret(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Enabled: true, Dir: dir, RetentionDays: 7, MaxBytes: 1 << 20})
	secret := "sk-live-secret-value"
	r.Record(Event{Time: time.Now(), KeyID: "team-a", Outcome: OutcomeSuccess, Status: 200, OK: true})
	r.Record(Event{Time: time.Now(), KeyID: "team-b", Outcome: OutcomeHTTPError, Status: 502})
	r.Close()

	rows, err := r.ReadArchive(10, Filter{KeyID: "TEAM-A"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].KeyID != "team-a" {
		t.Fatalf("filtered rows=%+v", rows)
	}
	all, _ := r.ReadArchive(10, Filter{})
	if len(all) != 2 {
		t.Fatalf("all rows=%d", len(all))
	}

	files, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if len(files) == 0 {
		t.Fatal("archive file missing")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"key_id":"team-a"`) {
		t.Fatalf("archive missing key_id: %s", raw)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "Authorization") {
		t.Fatalf("archive leaked secret: %s", raw)
	}

	enc, _ := json.Marshal(all[0])
	if strings.Contains(string(enc), secret) {
		t.Fatalf("event leaked secret: %s", enc)
	}
}

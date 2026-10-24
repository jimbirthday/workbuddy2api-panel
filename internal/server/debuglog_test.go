package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"Authorization": "Bearer abc.def-ghi"}`, `{"Authorization": "Bearer REDACTED"}`},
		{`Authorization: Bearer secret_token_123`, `Authorization: Bearer REDACTED`},
		{`{"accessToken":"tok1","refresh_token":"tok2"}`, `{"accessToken":"REDACTED","refresh_token":"REDACTED"}`},
		{`{"Authorization": "Bearer keep", "accessToken": "x"}`, `{"Authorization": "Bearer REDACTED", "accessToken": "REDACTED"}`},
		{`no sensitive data`, `no sensitive data`},
	}
	for _, c := range cases {
		got := string(redact([]byte(c.in)))
		if got != c.want {
			t.Errorf("redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeRID(t *testing.T) {
	cases := map[string]string{
		"abc123": "abc123",
		"a/b\\c": "a_b_c",
		"..":     "__",
		"a..b":   "a__b",
		"rid:1":  "rid_1",
		"":       "",
		"中文":     "__",
	}
	for in, want := range cases {
		if got := sanitizeRID(in); got != want {
			t.Errorf("sanitizeRID(%q) = %q, want %q", in, got, want)
		}
	}
	// 路径穿越类必须被压掉：经过过滤后的文件名不能包含路径分隔符。
	for _, in := range []string{"../etc/passwd", `..\..\windows`, "a/b/../../c"} {
		got := sanitizeRID(in)
		if strings.ContainsAny(got, "/\\") {
			t.Errorf("sanitizeRID(%q) leaked path separator: %q", in, got)
		}
	}
}

// TestCaptureDisabledNoFile 关闭时零开销：不创建目录、不写文件。
func TestCaptureDisabledNoFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	d := NewDebugLog(dir, "", false)
	d.Capture("rid", "req_in", []byte(`{"Authorization": "Bearer secret"}`))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("disabled capture created dir: %v", err)
	}
	// 空 payload 也不写。
	d2 := NewDebugLog(dir, "", true)
	d2.Capture("rid", "req_in", nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("empty payload created dir: %v", err)
	}
}

// TestCaptureWritesRedacted 开启后写入脱敏内容 + 分段。
func TestCaptureWritesRedacted(t *testing.T) {
	dir := t.TempDir()
	d := NewDebugLog(dir, "", true)
	d.Capture("r123", "req_in", []byte(`{"Authorization": "Bearer tok", "accessToken":"x"}`))
	d.Capture("r123", "upstream_error", []byte(`{"error":"boom"}`))

	raw, err := os.ReadFile(filepath.Join(dir, "r123.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(raw)
	if strings.Contains(got, "tok") && strings.Contains(got, "Bearer tok") {
		t.Errorf("token leaked:\n%s", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("missing redaction:\n%s", got)
	}
	for _, want := range []string{"--- req_in ---", "--- upstream_error ---", `{"error":"boom"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// TestCaptureDirDefault 空 dir 落到 logs/debug（相对当前工作目录，测试只验证默认值非空）。
func TestCaptureDirDefault(t *testing.T) {
	d := NewDebugLog("", "", true)
	if d.dir == "" {
		t.Fatal("default dir empty")
	}
}

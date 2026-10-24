package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// readRotatedFiles 按时间顺序（最旧 → 最新）拼接活跃文件与全部轮转文件：
// path.maxFiles-1 → … → path.1 → path。轮转时旧内容不断被推向更高编号，
// 因此编号越大内容越旧，活跃文件永远是最新一段。
func readRotatedFiles(t *testing.T, path string, maxFiles int) string {
	t.Helper()
	var sb strings.Builder
	for i := maxFiles - 1; i >= 1; i-- {
		b, err := os.ReadFile(path + "." + itoa(i))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		sb.Write(b)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sb.Write(b)
	return sb.String()
}

// New 必须拒绝空路径；maxBytes/maxFiles 非法时回落到默认值（16 MiB / 3 份）。
func TestNewValidation(t *testing.T) {
	if _, err := New("", 256, 3); err == nil {
		t.Fatal("New(\"\") must return error")
	}
	w, err := New(filepath.Join(t.TempDir(), "x.log"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if w.maxBytes != 16<<20 {
		t.Fatalf("maxBytes fallback = %d, want %d", w.maxBytes, int64(16<<20))
	}
	if w.maxFiles != 3 {
		t.Fatalf("maxFiles fallback = %d, want 3", w.maxFiles)
	}

	neg, err := New(filepath.Join(t.TempDir(), "x.log"), -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if neg.maxBytes != 16<<20 || neg.maxFiles != 3 {
		t.Fatalf("negative fallback = (%d, %d)", neg.maxBytes, neg.maxFiles)
	}
}

// 小 maxBytes 下多次写入必须触发轮转：base 保留最新内容，path.1/path.2 保留
// 旧内容；保留窗口外的文件不能残留，Write 返回的 n 恒等于输入长度。
func TestWriterRotatesAndDeletesOldest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := New(path, 256, 3)
	if err != nil {
		t.Fatal(err)
	}

	line := strings.Repeat("a", 40) + "\n" // 41 字节，40 行必然触发多轮轮转
	for i := 0; i < 40; i++ {
		n, err := w.Write([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if n != len(line) {
			t.Fatalf("Write n=%d, want %d", n, len(line))
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("base file missing: %v", err)
	}
	for _, suffix := range []string{".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("%s missing: %v", path+suffix, err)
		}
	}
	// 最旧编号（maxFiles-1=2）之上的文件不应存在；保留窗口之外的最旧内容已删除。
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("path.3 should be removed, stat err=%v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("file count = %d, want 3 (base + .1 + .2)", len(entries))
	}
}

// 一次多行 Write 跨多个轮转边界时不能丢行、不能丢换行。maxFiles 开得足够大，
// 使所有轮转内容都保留，拼接后必须与输入完全一致。
func TestWriterMultiLineAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	const maxFiles = 30
	w, err := New(path, 64, maxFiles)
	if err != nil {
		t.Fatal(err)
	}

	var want strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&want, "l%03d-%s\n", i, strings.Repeat("x", 20))
	}
	payload := []byte(want.String())
	n, err := w.Write(payload)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(payload) {
		t.Fatalf("Write n=%d, want %d", n, len(payload))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if got := readRotatedFiles(t, path, maxFiles); got != want.String() {
		t.Fatalf("content mismatch:\n got=%q\nwant=%q", got, want.String())
	}
}

// 单行超过 maxBytes 也必须完整写入，不能静默丢内容。
func TestWriterOversizedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w, err := New(path, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("B", 200) + "\n"
	n, err := w.Write([]byte(big))
	if err != nil {
		t.Fatal(err)
	}
	if n != len(big) {
		t.Fatalf("Write n=%d, want %d", n, len(big))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readRotatedFiles(t, path, 3); got != big {
		t.Fatalf("oversized line lost: got %q", got)
	}
}

// 目录不存在等打开失败必须以 error 返回，不能 panic。
func TestWriterOpenError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no", "such", "dir", "x.log")
	w, err := New(path, 256, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\n")); err == nil {
		t.Fatal("Write to missing directory must return error")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// 多 goroutine 并发写入：sync.Mutex 保证每行完整、不互相穿插，总行数正确。
func TestWriterConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	const maxFiles = 64
	w, err := New(path, 256, maxFiles)
	if err != nil {
		t.Fatal(err)
	}

	const (
		goroutines = 8
		linesPer   = 25
	)
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < linesPer; i++ {
				line := fmt.Sprintf("g%d-%04d-%s\n", g, i, strings.Repeat("x", 16))
				n, err := w.Write([]byte(line))
				if err != nil {
					errCh <- err
					return
				}
				if n != len(line) {
					errCh <- fmt.Errorf("n=%d, want %d", n, len(line))
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	content := readRotatedFiles(t, path, maxFiles)
	if lines := strings.Count(content, "\n"); lines != goroutines*linesPer {
		t.Fatalf("total lines = %d, want %d", lines, goroutines*linesPer)
	}
}

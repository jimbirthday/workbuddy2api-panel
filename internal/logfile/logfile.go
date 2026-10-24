// Package logfile 提供按大小轮转的持久化日志文件 Writer。
//
// 与 reqlog 的事件归档不同，这里不解析日志内容，只负责把调用方写进来的字节
// 完整地落到盘上：单文件超过 maxBytes 时按 path → path.1 → path.2 → …
// 顺序级联轮转，最旧的编号文件在超过 maxFiles 时删除。所有追加都走
// O_APPEND，写入失败（例如目录不存在）直接返回 error，不 panic。
package logfile

import (
	"errors"
	"io"
	"os"
	"sync"
)

const (
	// defaultMaxBytes 是未显式指定或非法单文件上限时回落的默认值（16 MiB）。
	defaultMaxBytes = int64(16 << 20)
	// defaultMaxFiles 是未显式指定或非法保留份数时回落的默认值：base + 2 个轮转。
	defaultMaxFiles = 3
)

// ErrEmptyPath 表示构造时传入的日志文件路径为空。
var ErrEmptyPath = errors.New("logfile: path is empty")

// Writer 是一个并发安全的按大小轮转日志文件写入器。
type Writer struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	file     *os.File
	size     int64
}

// New 创建指向 path 的轮转 Writer。maxBytes<=0 回落 16 MiB，
// maxFiles<=0 回落 3（保留 base + 2 个轮转文件）。path 为空返回错误。
//
// 构造时只做参数规范化，不主动创建文件；第一次 Write 时才会打开（保持
// 与 main 装配的兼容，目录缺失等打开失败都会在 Write 阶段以 error 返回）。
func New(path string, maxBytes int64, maxFiles int) (*Writer, error) {
	if path == "" {
		return nil, ErrEmptyPath
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxFiles <= 0 {
		maxFiles = defaultMaxFiles
	}
	return &Writer{
		path:     path,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
	}, nil
}

// Write 实现 io.Writer。p 会先按 '\n' 切行（保留行首与换行），逐行检查
// 是否需要轮转；单个行本身就超过 maxBytes 时直接写，不丢弃任何内容。
// 返回的 n 始终等于 len(p)，与是否触发轮转无关。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 空写不打开文件、不触发轮转，直接返回成功。
	if len(p) == 0 {
		return 0, nil
	}

	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '\n' {
			// 当前行区间是 [start, i]（含换行），写入前检查是否需要轮转。
			if err := w.writeChunk(p[start : i+1]); err != nil {
				return i + 1 - start, err
			}
			start = i + 1
		}
	}
	// 最后一段没有换行结尾，也要写出去（“保留行首”指不把行内容拆丢）。
	if start < len(p) {
		if err := w.writeChunk(p[start:]); err != nil {
			return len(p) - start, err
		}
	}
	return len(p), nil
}

// writeChunk 写入一段已按行切好的字节。先判断当前文件大小 + 待写字节是否
// 超过上限，超过则先轮转；单个 chunk 超过 maxBytes 时也直接写（不丢内容），
// 由后续 Write 在下一次写前触发轮转。
func (w *Writer) writeChunk(p []byte) error {
	if w.file == nil {
		if err := w.open(); err != nil {
			return err
		}
	}
	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	n, err := w.file.Write(p)
	if n > 0 {
		w.size += int64(n)
	}
	return err
}

// open 打开（或重新打开）活跃文件并读取当前大小，保证 size 与磁盘一致。
func (w *Writer) open() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
		w.size = 0
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

// rotate 执行级联轮转：path.maxFiles-1 删除，path.2 → path.3、path.1 → path.2、
// path → path.1，随后重新打开空活跃文件。rename 目标已存在时先删除（Linux
// 上 os.Rename 会直接覆盖，但 Windows 上目标存在会失败，先 Remove 保证稳妥）。
func (w *Writer) rotate() error {
	_ = w.file.Close()
	w.file = nil
	w.size = 0

	// 最旧的一份超出保留窗口，先删掉。
	oldest := numberedPath(w.path, w.maxFiles-1)
	_ = os.Remove(oldest)

	// 从高编号向低编号级联，避免覆盖还没搬走的文件。
	for i := w.maxFiles - 1; i > 0; i-- {
		src := numberedPath(w.path, i-1)
		dst := numberedPath(w.path, i)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		// 目标存在先移除，Windows 上 rename 不会覆盖已存在文件。
		if _, err := os.Stat(dst); err == nil {
			if err := os.Remove(dst); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return w.open()
}

// numberedPath 返回带编号的轮转文件路径，编号 0 即活跃文件本身。
func numberedPath(path string, n int) string {
	if n <= 0 {
		return path
	}
	return path + "." + itoa(n)
}

// itoa 是避免引入 strconv 的最小正整数转字符串（编号由轮转逻辑保证非负）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Close 关闭底层文件。重复关闭安全，返回最后一次关闭错误。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	w.size = 0
	return err
}

// Dir 返回当前活跃文件路径（未打开过时与构造传入的 path 一致），
// 便于调用方在启动日志中打印实际落盘位置。
func (w *Writer) Dir() string {
	return w.path
}

// 编译期保证 Writer 实现 io.Writer 与 io.Closer。
var (
	_ io.Writer = (*Writer)(nil)
	_ io.Closer = (*Writer)(nil)
)

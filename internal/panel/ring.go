// ring.go 固定容量的结构化日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
//
// 每行入环时按前缀规则归类频道（chat=对话请求表格行 / task=任务动作 /
// sys=系统与其它），面板日志视图按频道筛选——对话流量大时任务结果不被冲掉。
package panel

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 日志频道。
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys  = "sys"
)

// LogEntry 单条日志（时间戳取写入时刻；log 包行的行首日期时间已被剥离）。
type LogEntry struct {
	TS   time.Time `json:"ts"`
	Ch   string    `json:"ch"`
	Text string    `json:"text"`
}

// taskPrefixes 任务动作日志的行首标识（scheduler 与 panel 的既有口径）。
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: 任务", "panel: 一键", "panel: checkin", "panel: 手动",
	"panel: 队列", "panel: 券码",
}

// tsPrefixRe log 包默认 flags（日期 时间）产生的行首时间戳。
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine 按行首特征归类频道。
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat 表格日志（server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring 日志环形缓冲。
type Ring struct {
	mu      sync.Mutex
	entries []LogEntry
	cap     int
	head    int // 下一写入位；写满后指向最旧一条
	len     int
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity, entries: make([]LogEntry, capacity)}
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cap <= 0 {
		r.cap = 500
	}
	if len(r.entries) < r.cap {
		// 零值 Ring 也允许写入（测试/旧调用方可能直接 var r Ring）。只追加缺失
		// 容量，不重置已有条目；正常 NewRing 路径不会进入这里。
		grown := make([]LogEntry, r.cap)
		if len(r.entries) > 0 && r.len > 0 {
			start := (r.head - r.len + len(r.entries)) % len(r.entries)
			for i := 0; i < r.len; i++ {
				grown[i] = r.entries[(start+i)%len(r.entries)]
			}
		}
		r.entries = grown
		r.head = r.len
	}
	// 直接在 []byte 上切行，避免把整个写入块先复制成 string；LogEntry.Text
	// 仍需要独立字符串，但只复制实际保留的行。
	raw := bytes.TrimRight(p, "\r\n")
	for len(raw) > 0 {
		var line []byte
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			line, raw = raw, nil
		}
		if len(line) == 0 {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(string(line), "")
		r.entries[r.head] = LogEntry{TS: now, Ch: classifyLine(text), Text: text}
		r.head = (r.head + 1) % r.cap
		if r.len < r.cap {
			r.len++
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部条目（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.len == 0 {
		return nil
	}
	out := make([]LogEntry, r.len)
	start := (r.head - r.len + r.cap) % r.cap
	for i := 0; i < r.len; i++ {
		out[i] = r.entries[(start+i)%r.cap]
	}
	return out
}

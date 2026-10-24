// debuglog.go 请求级脱敏抓包器：只按需落盘，捕获失败静默丢弃，绝不进入请求路径。
//
// 为什么需要：生产排障时「上游回的错误帧原文 / 修复层动过的正文片段」是唯一的
// 一手证据，但完整抓包会把 Authorization / accessToken 这类敏感字段连同正常正文
// 一起写进磁盘。这里做成请求级、脱敏、按 rid 分文件的最小实现：
//   - 默认关闭（enabled=false），零开销；
//   - 开启后只抓错误/修复相关字节，不抓正常出站正文（隐私边界）；
//   - payload 先脱敏再落盘，写失败只丢弃不 panic。
package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DebugLog 请求级脱敏抓包器。并发安全；enabled=false 时 Capture 零开销返回。
type DebugLog struct {
	dir       string
	uidFilter string
	enabled   bool
	mu        sync.Mutex
}

// NewDebugLog 构造抓包器。dir 为空时落到 logs/debug；uidFilter 非空时只抓匹配
// 账号的请求（暂时由调用方在挂载点决定是否传入；本实现保留字段但 Capture 不
// 按 uid 过滤——过滤粒度在 handler 挂载点层面更可控）。enabled=false 关闭。
func NewDebugLog(dir, uidFilter string, enabled bool) *DebugLog {
	if dir == "" {
		dir = filepath.Join("logs", "debug")
	}
	return &DebugLog{dir: dir, uidFilter: uidFilter, enabled: enabled}
}

// Enabled 报告抓包器是否开启。
func (d *DebugLog) Enabled() bool {
	return d != nil && d.enabled
}

// sanitizeRID 文件名安全过滤：只保留字母数字与 '-', '_', '.'，其余替换为 '_'。
// 顺带把路径分隔符 / \ 与连续点（..）也一并替换，杜绝目录穿越。
func sanitizeRID(rid string) string {
	rid = strings.TrimSpace(rid)
	var b strings.Builder
	b.Grow(len(rid))
	for _, r := range rid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	// 连续点（含 ".."）压成下划线：文件名里保留单点即可，双点除了目录穿越无他用。
	return strings.ReplaceAll(b.String(), "..", "__")
}

var (
	// bearerRe 匹配 JSON 里的 "Authorization": "Bearer xxx" 与非 JSON 头
	// Authorization: Bearer xxx 两种形态，token 统一替换为 REDACTED。
	bearerRe = regexp.MustCompile(`(?i)("Authorization"\s*:\s*"Bearer\s+)[A-Za-z0-9._~+/=-]+|(Authorization\s*:\s*Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	// tokenFieldRe 匹配 accessToken / refresh_token 字段值（JSON 字符串形态）。
	tokenFieldRe = regexp.MustCompile(`(?i)("(?:accessToken|refresh_token)"\s*:\s*")[^"]*(")`)
)

// redact 对 payload 做尽力脱敏：Bearer token 与 accessToken/refresh_token 字段值
// 替换为 REDACTED。实现是「简单替换即可」级别，不做 JSON 完整解析。
func redact(payload []byte) []byte {
	out := bearerRe.ReplaceAll(payload, []byte("${1}${2}REDACTED"))
	out = tokenFieldRe.ReplaceAll(out, []byte("${1}REDACTED${2}"))
	return out
}

// Capture 追加写 dir/<rid>.log。enabled=false 或 payload 为空时零开销直接返回。
// 文件内按 "--- phase ---" 分段并带时间戳；rid 先做文件名安全过滤；写盘失败静默丢弃。
func (d *DebugLog) Capture(rid, phase string, payload []byte) {
	if d == nil || !d.enabled || len(payload) == 0 {
		return
	}
	name := sanitizeRID(rid)
	if name == "" {
		name = "unknown"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = os.MkdirAll(d.dir, 0o755) // 失败会落在随后的 OpenFile 上，统一静默丢弃
	f, err := os.OpenFile(filepath.Join(d.dir, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("--- " + phase + " --- " + time.Now().Format(time.RFC3339) + "\n")
	_, _ = f.Write(redact(payload))
	_, _ = f.WriteString("\n")
}

// dsmlRepairFragment 从非流式聚合响应中提取包含 DSML 标记的 assistant content
// 片段（脱敏由 DebugLog.Capture 统一处理）。只返回命中修复标记的正文片段，
// 供「DSML 修复命中」抓包用；无标记返回 nil。
func dsmlRepairFragment(resp map[string]any) []byte {
	if resp == nil {
		return nil
	}
	choices, _ := resp["choices"].([]any)
	var out []byte
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := c["message"].(map[string]any)
		if !ok {
			continue
		}
		text, _ := msg["content"].(string)
		if text == "" || !strings.Contains(text, "\uFF5C\uFF5C") {
			continue
		}
		out = append(out, text...)
		out = append(out, '\n')
	}
	return out
}

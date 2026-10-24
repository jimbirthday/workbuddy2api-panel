// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey string // 顶层兼容密钥；空 = 该条目不参与鉴权
	// Keys 是完整 keyring（含 legacy 条目）。APIKey 保留是为了旧调用方与
	// 只读字段兼容；鉴权优先使用 Keys，Keys 为空时才回落到 APIKey。
	Keys                 []httpauth.Key
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
	// LogLevel 日志级别（debug/info/warn/error）：main 装配与面板保存配置时
	// 经快照热生效（logx.SetLevel），无需重启。
	LogLevel string
	// BackgroundModels 客户端后台任务模型重定向（空名单/空目标 = 关闭）。
	// 与鉴权同一次快照读取，保存配置后无需重启；重定向发生在 Key 的模型白名单
	// 校验之后，不会让受限 Key 通过别名访问白名单外的模型。
	BackgroundModelNames  string
	BackgroundModelTarget string
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

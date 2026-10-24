// Package logx 轻量级别日志：零第三方依赖，只包装标准库 log。
//
// 约定（与仓库「不引日志库」的现状一致）：
//   - 所有输出必须走标准库 log.Output，这样 cmd/server/main.go 里
//     log.SetOutput(io.MultiWriter(...)) 注入的面板环形缓冲镜像仍然生效；
//   - 前缀：Debug → "DEBUG: "、Warn → "WARN: "、Error → "ERR: "、Info → 无前缀
//     （与既有 log.Printf 风格兼容，正常流转不打级别字样）；
//   - 级别由环境变量 WB2A_LOG_LEVEL 启动时初始化一次（缺省 info，大小写不敏感，
//     非法值回落 info）。
package logx

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// 级别常量：与 SetLevel/Level 接受的字符串一一对应。
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// levelNames 是级别字符串的规范形态；normalize 会把输入折叠到其中。
var levelNames = []string{LevelDebug, LevelInfo, LevelWarn, LevelError}

// level 当前级别，用数值表示（0=debug … 3=error），越大越安静。
// 用 atomic 存级别，SetLevel 与各级别日志函数之间无锁。
var level atomic.Int32

// levelIndex 返回规范级别名对应的数值下标（未命中按 info=1 处理）。
func levelIndex(name string) int {
	for i, n := range levelNames {
		if n == name {
			return i
		}
	}
	return 1 // info
}

// normalize 把任意级别字符串折叠到规范级别：大小写不敏感，非法值回落 info。
func normalize(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case LevelDebug:
		return LevelDebug
	case LevelWarn:
		return LevelWarn
	case LevelError:
		return LevelError
	default:
		return LevelInfo
	}
}

func init() {
	initLevel()
}

// SetLevel 设置日志级别（debug|info|warn|error），大小写不敏感；非法值回落 info。
func SetLevel(s string) {
	level.Store(int32(levelIndex(normalize(s))))
}

// initLevel 从环境变量重新解析级别。init() 启动时读一次；测试用它恢复环境。
func initLevel() {
	SetLevel(os.Getenv("WB2A_LOG_LEVEL"))
}

// Level 返回当前级别（规范形态字符串）。
func Level() string {
	i := int(level.Load())
	if i < 0 || i >= len(levelNames) {
		return LevelInfo
	}
	return levelNames[i]
}

// enabled 报告该级别在当前阈值下是否应输出。
func enabled(name string) bool {
	return level.Load() <= int32(levelIndex(name))
}

// logAt 统一入口：级别过滤 + 前缀拼接 + log.Output 输出。
// calldepth=2 指向 logAt 的调用方（Debugf 等），保证日志行里的 file:line
// 是业务调用点而不是本包内部。
func logAt(name, prefix, format string, args ...any) {
	if !enabled(name) {
		return
	}
	msg := prefix
	if len(args) == 0 {
		msg += format
	} else {
		msg += fmt.Sprintf(format, args...)
	}
	_ = log.Output(2, msg)
}

// Debugf 输出 debug 级别日志（带 "DEBUG: " 前缀）。
func Debugf(format string, args ...any) { logAt(LevelDebug, "DEBUG: ", format, args...) }

// Infof 输出 info 级别日志（无前缀）。
func Infof(format string, args ...any) { logAt(LevelInfo, "", format, args...) }

// Warnf 输出 warn 级别日志（带 "WARN: " 前缀）。
func Warnf(format string, args ...any) { logAt(LevelWarn, "WARN: ", format, args...) }

// Errorf 输出 error 级别日志（带 "ERR: " 前缀）。
func Errorf(format string, args ...any) { logAt(LevelError, "ERR: ", format, args...) }

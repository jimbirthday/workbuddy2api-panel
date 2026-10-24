package logx

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// capture 在 fn 期间把标准库 log 输出重定向到 buffer，返回输出内容。
func capture(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	return buf.String()
}

func TestLevels(t *testing.T) {
	SetLevel(LevelDebug)
	if Level() != LevelDebug {
		t.Fatalf("level=%s want debug", Level())
	}
	out := capture(t, func() {
		Debugf("d")
		Infof("i")
		Warnf("w")
		Errorf("e")
	})
	for _, want := range []string{"DEBUG: d", "i", "WARN: w", "ERR: e"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug level missing %q:\n%s", want, out)
		}
	}
}

// TestLevelFilter 级别过滤：低级别不输出高级别更不输出；只发同级及更重。
func TestLevelFilter(t *testing.T) {
	// debug 级：info 也要发。
	SetLevel(LevelDebug)
	out := capture(t, func() { Infof("i@debug") })
	if !strings.Contains(out, "i@debug") {
		t.Errorf("debug level should emit info:\n%s", out)
	}
	// info 级：不发 debug，发 info/warn/error。
	SetLevel(LevelInfo)
	out = capture(t, func() {
		Debugf("d@info")
		Infof("i@info")
		Warnf("w@info")
		Errorf("e@info")
	})
	if strings.Contains(out, "d@info") {
		t.Errorf("info level must not emit debug:\n%s", out)
	}
	for _, want := range []string{"i@info", "WARN: w@info", "ERR: e@info"} {
		if !strings.Contains(out, want) {
			t.Errorf("info level missing %q:\n%s", want, out)
		}
	}
	// warn 级：发 warn/error，不发 info/debug。
	SetLevel(LevelWarn)
	out = capture(t, func() {
		Debugf("d@warn")
		Infof("i@warn")
		Warnf("w@warn")
		Errorf("e@warn")
	})
	if strings.Contains(out, "d@warn") || strings.Contains(out, "i@warn") {
		t.Errorf("warn level must not emit debug/info:\n%s", out)
	}
	for _, want := range []string{"WARN: w@warn", "ERR: e@warn"} {
		if !strings.Contains(out, want) {
			t.Errorf("warn level missing %q:\n%s", want, out)
		}
	}
	// error 级：只发 error。
	SetLevel(LevelError)
	out = capture(t, func() {
		Debugf("d@error")
		Infof("i@error")
		Warnf("w@error")
		Errorf("e@error")
	})
	if strings.Contains(out, "d@error") || strings.Contains(out, "i@error") || strings.Contains(out, "w@error") {
		t.Errorf("error level must not emit debug/info/warn:\n%s", out)
	}
	if !strings.Contains(out, "ERR: e@error") {
		t.Errorf("error level missing error line:\n%s", out)
	}
}

// TestSetLevelCaseInsensitive 大小写不敏感 + 非法值回落 info。
func TestSetLevelCaseInsensitive(t *testing.T) {
	SetLevel("DEBUG")
	if Level() != LevelDebug {
		t.Errorf("DEBUG -> %s want debug", Level())
	}
	SetLevel("Warn")
	if Level() != LevelWarn {
		t.Errorf("Warn -> %s want warn", Level())
	}
	SetLevel("")
	if Level() != LevelInfo {
		t.Errorf("empty -> %s want info", Level())
	}
	SetLevel("bogus")
	if Level() != LevelInfo {
		t.Errorf("bogus -> %s want info", Level())
	}
	// 非法级别下 info 输出、debug 不发。
	out := capture(t, func() {
		Debugf("d@bogus")
		Infof("i@bogus")
	})
	if strings.Contains(out, "d@bogus") {
		t.Errorf("bogus level must not emit debug:\n%s", out)
	}
	if !strings.Contains(out, "i@bogus") {
		t.Errorf("bogus level (info fallback) should emit info:\n%s", out)
	}
}

// TestInitFromEnv 启动时从 WB2A_LOG_LEVEL 读一次。
func TestInitFromEnv(t *testing.T) {
	old, had := os.LookupEnv("WB2A_LOG_LEVEL")
	os.Setenv("WB2A_LOG_LEVEL", "ERROR")
	initLevel() // 重新解析一次环境变量
	if Level() != LevelError {
		t.Errorf("WB2A_LOG_LEVEL=ERROR -> %s want error", Level())
	}
	os.Setenv("WB2A_LOG_LEVEL", "warn")
	initLevel()
	if Level() != LevelWarn {
		t.Errorf("WB2A_LOG_LEVEL=warn -> %s want warn", Level())
	}
	if had {
		os.Setenv("WB2A_LOG_LEVEL", old)
	} else {
		os.Unsetenv("WB2A_LOG_LEVEL")
	}
	initLevel() // 恢复测试环境
}

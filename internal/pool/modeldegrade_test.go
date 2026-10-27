package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNoteModelFailuresScoped 连败降权按模型隔离：A 模型连败达阈只让 A 临时出池，
// 账号级（degradeUntil/consecutiveFails）与 B 模型均不受影响。
func TestNoteModelFailuresScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(2, time.Hour, time.Hour)

	// 模型 A 连败 2 次 → A 降权，账号级与 B 不受影响。
	p.NoteFailures("u1", "kimi-k3-1")
	p.NoteFailures("u1", "kimi-k3-1")

	if p.HasRotatable(nil, "kimi-k3-1", "") {
		t.Fatal("kimi-k3-1 应处于模型级连败降权中，不可选")
	}
	if !p.HasRotatable(nil, "deepseek-v4.1-flash", "") {
		t.Fatal("其他模型不应被模型级连败降权影响")
	}
	if a := p.PickExcludingForModel(nil, "deepseek-v4.1-flash"); a == nil || a.UID != "u1" {
		t.Fatalf("账号级应仍可选其他模型: %+v", a)
	}

	p.mu.RLock()
	e := p.byUID["u1"]
	acctFails, acctUntil := e.consecutiveFails, e.degradeUntil
	p.mu.RUnlock()
	if acctFails != 0 || !acctUntil.IsZero() {
		t.Fatalf("账号级连败状态不得被模型级失败污染: fails=%d until=%v", acctFails, acctUntil)
	}

	// B 成功只清 B，不清 A 的模型级降权。
	p.NoteFailures("u1", "deepseek-v4.1-flash")
	p.NoteSuccess("u1")
	p.ClearModelDegrade("u1", "deepseek-v4.1-flash")
	if p.HasRotatable(nil, "kimi-k3-1", "") {
		t.Fatal("B 的成功/清除不应解除 A 的模型级降权")
	}
}

// TestNoteFailuresAccountFallback 无模型上下文时仍走账号级连败降权（向后兼容）。
func TestNoteFailuresAccountFallback(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(2, time.Hour, time.Hour)
	p.NoteFailures("u1", "")
	p.NoteFailures("u1", "")

	if p.HasRotatable(nil, "kimi-k3-1", "") {
		t.Fatal("账号级连败降权应让该号整体出池")
	}
	p.mu.RLock()
	e := p.byUID["u1"]
	acctUntil := e.degradeUntil
	p.mu.RUnlock()
	if acctUntil.IsZero() {
		t.Fatal("账号级降权截止应已写入")
	}
}

// TestClearModelDegrade 清除指定模型级连败，不影响其他模型与账号级。
func TestClearModelDegrade(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(1, time.Hour, time.Hour)
	p.NoteFailures("u1", "kimi-k3-1")
	if p.HasRotatable(nil, "kimi-k3-1", "") {
		t.Fatal("前置：kimi-k3-1 应已降权")
	}
	p.ClearModelDegrade("u1", "kimi-k3-1")
	if !p.HasRotatable(nil, "kimi-k3-1", "") {
		t.Fatal("ClearModelDegrade 应解除该模型的降权")
	}
}

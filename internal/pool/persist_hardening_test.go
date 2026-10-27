package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestPersistFailureStaysDirtyAndRetries 落盘失败后 dirty 必须被重新置位：
// 否则本次失败的变更在下一次内存变更之前再也不会重试，进程重启即永久丢失。
func TestPersistFailureStaysDirtyAndRetries(t *testing.T) {
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(block, "state.json") // 父路径是普通文件 → 必失败

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.mu.Lock()
	p.stateFp = bad
	p.mu.Unlock()

	p.Flush()
	if p.persistFails == 0 {
		t.Fatal("落盘失败应被记录")
	}
	if !p.dirty.Load() {
		t.Fatal("落盘失败后 dirty 必须重新置位（否则变更永不重试）")
	}

	// 换到可写路径：下一次 Flush 必须把先前失败的变更补上。
	good := filepath.Join(t.TempDir(), "state.json")
	p.mu.Lock()
	p.stateFp = good
	p.mu.Unlock()
	p.Flush()
	if p.persistFails != 0 {
		t.Fatalf("成功后 persistFails 应归零，得到 %d", p.persistFails)
	}
	if p.dirty.Load() {
		t.Fatal("成功后 dirty 应为 false")
	}
	raw, err := os.ReadFile(good)
	if err != nil || !strings.Contains(string(raw), `"credits":42`) {
		t.Fatalf("重试落盘内容不正确: %v %s", err, raw)
	}

	// 落盘成功后不应留下临时文件（唯一化 tmp 名也必须被清理）。
	if _, err := os.Stat(good + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("不应残留 .tmp: %v", err)
	}
}

// TestPersistLeavesNoTmpFile 成功落盘后目录里只应有 state.json 本身。
func TestPersistLeavesNoTmpFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 7, 0)
	p.Flush()

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != "state.json" {
			t.Fatalf("落盘目录出现多余文件: %s", e.Name())
		}
	}
}

// TestNoteFailuresKeepsLaterCooldownReason 连败降权达阈时，若账号级冷却（until）仍生效
// 且不早于本次降权截止，则不得用降权原因覆盖 e.reason——否则 statusOf 的 reason 会把
// 仍在生效的真实冷却原因（如 "429 rate limit"）掩盖成 "consecutive failures"。
func TestNoteFailuresKeepsLaterCooldownReason(t *testing.T) {
	// 子测 1：更晚的账号级冷却胜出 → 保留原 reason。
	t.Run("later_cooldown_keeps_reason", func(t *testing.T) {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		p.SetDegrade(1, time.Minute, time.Minute) // 降权 1 分钟
		p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
		p.NoteFailures("u1", "") // 达阈 → 降权截止（+1m）早于 until（+1h）

		p.mu.RLock()
		e := p.byUID["u1"]
		until, degradeUntil, reason := e.until, e.degradeUntil, e.reason
		p.mu.RUnlock()
		if degradeUntil.IsZero() {
			t.Fatal("降权应已触发")
		}
		if reason != "429 rate limit" {
			t.Fatalf("更晚的账号级冷却原因被降权覆盖: %q", reason)
		}
		if !degradeUntil.Before(until) {
			t.Fatalf("前置条件：降权截止应早于 until（%v vs %v）", degradeUntil, until)
		}
		st, _ := p.Status("u1")
		if st.Reason != "429 rate limit" {
			t.Fatalf("Status.Reason=%q want 保留真实冷却原因", st.Reason)
		}
	})

	// 子测 2：账号无生效冷却（until 已过期）→ 降权成为最远生效惩罚，正常写降权原因。
	t.Run("no_live_cooldown_writes_degrade_reason", func(t *testing.T) {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		p.SetDegrade(1, time.Minute, time.Minute)
		p.Cooldown("u1", CoolSoft, time.Millisecond, "429 rate limit")
		time.Sleep(5 * time.Millisecond) // 让账号级冷却自然过期
		p.NoteFailures("u1", "")

		p.mu.RLock()
		reason := p.byUID["u1"].reason
		p.mu.RUnlock()
		if reason != degradeReason {
			t.Fatalf("无生效冷却时降权原因应写入: %q", reason)
		}
	})

	// 子测 3：降权截止不早于 until（同刻或更晚）→ 降权成为最远生效惩罚，写降权原因。
	t.Run("degrade_not_earlier_than_until_writes_degrade_reason", func(t *testing.T) {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		p.SetDegrade(1, time.Hour, time.Hour)                     // 降权 1 小时
		p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit") // 冷却 1 分钟（更早）
		p.NoteFailures("u1", "")

		p.mu.RLock()
		reason := p.byUID["u1"].reason
		p.mu.RUnlock()
		if reason != degradeReason {
			t.Fatalf("降权更晚时应写降权原因: %q", reason)
		}
	})
}

// TestStatusCoolingCoversDegradeUntil Status.Cooling 必须与选号口径一致：
// 处于连败降权（degradeUntil）期内的账号不参与选号，Status 不能还报 Cooling=false。
func TestStatusCoolingCoversDegradeUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(1, time.Hour, time.Hour)
	p.NoteFailures("u1", "") // 达阈 → degradeUntil = now+1h

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 不存在")
	}
	if !st.Cooling {
		t.Fatalf("降权期内 Status.Cooling 应为 true: %+v", st)
	}
	if st.DegradeUntil.IsZero() {
		t.Fatal("DegradeUntil 应透出")
	}
	if _, _, cooling, _, _, _ := p.CountsDetailedWithPaused(); cooling != 1 {
		t.Fatalf("countsDetailedForRealm 口径 cooling=1，得到 %d（Status 与计数口径必须一致）", cooling)
	}
}

// TestPersistConcurrentWithRemove Remove/SyncToDir/Flush 并发时落盘必须串行且无竞态
// （用 -race 跑更严格）；只断言不 panic、最终文件内容合法。
func TestPersistConcurrentWithRemove(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	for i := 0; i < 4; i++ {
		p.Add(&auth.Auth{UID: string(rune('a' + i))})
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch j % 3 {
				case 0:
					p.Flush()
				case 1:
					p.SyncToDir([]*auth.Auth{{UID: "a"}, {UID: "b"}, {UID: "c"}, {UID: "d"}})
				default:
					p.Remove("d")
					p.Add(&auth.Auth{UID: "d"})
				}
			}
		}(i)
	}
	wg.Wait()
	p.Flush()
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("并发落盘后 state.json 应存在且合法: %v", err)
	}
}

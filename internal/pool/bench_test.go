package pool

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 本文件为 P 组性能审查的量化基准（go test -bench 可复现），结论见 REVIEW-conflicts-perf.md。

// benchPool 构建 46 账号的池（对齐生产规模），全部 healthy 且 credits 各不相同。
func benchPool(b *testing.B) *Pool {
	p := New("")
	for i := 0; i < 46; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)})
		p.SetCredits(fmt.Sprintf("u%02d", i), int64(1000-i*13%900), 0)
	}
	return p
}

// BenchmarkPick46Accounts P1：46 账号全扫描 + 全排序 + 三因子权重抽签的每次耗时。
func BenchmarkPick46Accounts(b *testing.B) {
	p := benchPool(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Pick()
	}
}

// BenchmarkStateSerialize46Accounts P2：46 账号 state.json 序列化（stateOverviewLocked + MarshalIndent）。
func BenchmarkStateSerialize46Accounts(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.MarshalIndent(sf, "", "  "); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStateSerializeCompact46 P2 对照：json.Marshal（非缩进）耗时，量化写放大的下界。
func BenchmarkStateSerializeCompact46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(sf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotMarshal46 P2/P4 相关：落盘时额外镜像一次 snapshot（含 savedAt）的序列化成本。
func BenchmarkSnapshotMarshal46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := time.Now()
		if _, err := json.Marshal(snapshot{stateFile: sf, SavedAt: now}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotFromStateJSON46 对照 BenchmarkSnapshotMarshal46：复用已编码的
// state.json（json.Compact + 追加 saved_at），而不是对同一 stateFile 再 Marshal 一遍。
func BenchmarkSnapshotFromStateJSON46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := snapshotFromStateJSON(raw, time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGoroutineSpawn P4 量化：单次 fire-and-forget goroutine 起搏的 CPU 成本下界
// （对应对 Session.SetBind / pool SaveState 每次镜像派生 goroutine 的开销，不含网络，
//
//	网络走 5s 超时 fire-and-forget）。
func BenchmarkGoroutineSpawn(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		go func() { _ = 1 }()
	}
}

// BenchmarkModelLockView470x10 面板 /overview 每 5s 都会调 ModelLockView：470 账号、
// 10 个模型各自被 100 个号锁定时，旧实现的内层全池重扫是 O(10×470)。这里钉住优化后
// 的固定成本，避免以后又把 servable 算回「每行扫全池」。
func BenchmarkModelLockView470x10(b *testing.B) {
	p := New("")
	base := time.Now().Add(time.Hour)
	for i := 0; i < 470; i++ {
		uid := fmt.Sprintf("u%03d", i)
		p.Add(&auth.Auth{UID: uid})
	}
	p.mu.Lock()
	for i := 0; i < 470; i++ {
		e := p.byUID[fmt.Sprintf("u%03d", i)]
		e.modelCooldowns = map[string]modelCooldown{}
		for m := 0; m < 10; m++ {
			if i < 100 {
				e.modelCooldowns[fmt.Sprintf("m%02d", m)] = modelCooldown{Until: base}
			}
		}
	}
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := p.ModelLockView(); len(rows) != 10 {
			b.Fatalf("rows=%d want 10", len(rows))
		}
	}
}

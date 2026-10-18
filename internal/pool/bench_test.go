package pool

import (
	"bytes"
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

// BenchmarkPick470Accounts 是生产规模（470 号）的选号基线。46 号基准无法暴露
// 全池遍历/排序在真实账号数下的长尾；并行基准用来观察同一把池写锁的串行化程度。
func BenchmarkPick470Accounts(b *testing.B) {
	p := New("")
	for i := 0; i < 470; i++ {
		uid := fmt.Sprintf("u%03d", i)
		p.Add(&auth.Auth{UID: uid})
		if i%7 != 0 {
			p.SetCredits(uid, int64(1000-i), 0)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Pick()
	}
}

func BenchmarkPick470AccountsParallel(b *testing.B) {
	p := New("")
	for i := 0; i < 470; i++ {
		uid := fmt.Sprintf("u%03d", i)
		p.Add(&auth.Auth{UID: uid})
		if i%7 != 0 {
			p.SetCredits(uid, int64(1000-i), 0)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Pick()
		}
	})
}

// BenchmarkHasRotatable470 是限流轮转的存在性判定基线：只应做 O(n) 扫描，
// 不排序、不分配整表。
func BenchmarkHasRotatable470(b *testing.B) {
	p := New("")
	for i := 0; i < 470; i++ {
		uid := fmt.Sprintf("u%03d", i)
		p.Add(&auth.Auth{UID: uid})
		if i%7 != 0 {
			p.SetCredits(uid, int64(1000-i), 0)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !p.HasRotatable(nil, "", "") {
			b.Fatal("expected rotatable account")
		}
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

// BenchmarkModelLockView470NoLocks 对照无锁场景：绝大多数生产时刻没有模型级
// 冷却，聚合器应只遍历账号、建立少量空地图，不产生每模型字符串键分配。
func BenchmarkModelLockView470NoLocks(b *testing.B) {
	p := New("")
	for i := 0; i < 470; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%03d", i)})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := p.ModelLockView(); rows != nil {
			b.Fatalf("rows=%v want nil", rows)
		}
	}
}

// BenchmarkRealmByUID470 请求完成时用量记账只查域；对照 Status 的完整状态构建，
// 防止把请求热路径又改回 Status(uid)。
func BenchmarkRealmByUID470(b *testing.B) {
	p := New("")
	for i := 0; i < 470; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%03d", i), Nickname: "nick"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := p.RealmByUID("u001"); got != "cn" {
			b.Fatalf("realm=%q want cn", got)
		}
	}
}

// TestCompactJSONIntoMatchesCompactJSON 原地压缩必须与独立缓冲版本逐字节一致，
// 包括字符串内的空白、引号、反斜杠与换行转义；同时覆盖空对象/嵌套对象。
func TestCompactJSONIntoMatchesCompactJSON(t *testing.T) {
	cases := [][]byte{
		[]byte("{}"),
		[]byte(" {\n  \"accounts\" : { }\n} "),
		[]byte("{\"u\":\"a b\\t c\\\"d\\\\e\",\"n\":{\"x\":1, \"y\":[2, 3]}}"),
		[]byte("{\n\t\"k\" : \"中文 空格\\n换行\"\n}"),
	}
	for _, raw := range cases {
		want := compactJSON(append([]byte(nil), raw...))
		got := compactJSONInto(raw, append([]byte(nil), raw...))
		if !bytes.Equal(got, want) {
			t.Fatalf("mismatch for %q: got=%q want=%q", raw, got, want)
		}
	}
}

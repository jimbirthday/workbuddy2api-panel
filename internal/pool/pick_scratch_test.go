package pool

import (
	"fmt"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestPickScratchReuseAfterPoolShrink 钉住 scratch 复用的边界：sync.Pool 中的
// 候选/排序缓冲会保留历史容量，账号删除/筛选变少后必须只读前 len 个，不能把
// 上一轮的旧条目带进本轮抽签。
func TestPickScratchReuseAfterPoolShrink(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for i := 0; i < 12; i++ {
		uid := fmt.Sprintf("u%02d", i)
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, int64(1000-i), 0)
	}

	if got := p.PickExcluding(nil); got == nil {
		t.Fatal("first pick returned nil")
	}

	tried := map[string]bool{}
	for i := 0; i < 10; i++ {
		tried[fmt.Sprintf("u%02d", i)] = true
	}
	for i := 0; i < 50; i++ {
		got := p.PickExcluding(tried)
		if got == nil {
			t.Fatalf("iter %d: no pick among remaining accounts", i)
		}
		if tried[got.UID] {
			t.Fatalf("iter %d: picked tried account %s (stale scratch entry)", i, got.UID)
		}
		if got.UID != "u10" && got.UID != "u11" {
			t.Fatalf("iter %d: picked %s, want only u10/u11", i, got.UID)
		}
	}
}

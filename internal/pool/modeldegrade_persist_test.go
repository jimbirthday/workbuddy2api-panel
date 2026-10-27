package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelDegradesPersistRoundTrip 模型级连败降权表必须持久化往返：
// 达阈降权（Until 非零）与未达阈进度（Fails>0、Until 零）重启后都要恢复。
func TestModelDegradesPersistRoundTrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(2, time.Hour, time.Hour)

	// kimi-k3-1 连败 2 次达阈 → 降权（Fails=0, Until=now+1h）。
	p.NoteFailures("u1", "kimi-k3-1")
	p.NoteFailures("u1", "kimi-k3-1")
	// deepseek-v4.1-flash 只连败 1 次 → 未达阈进度（Fails=1, Until 零）。
	p.NoteFailures("u1", "deepseek-v4.1-flash")

	p.Flush()

	// 重新加载：状态从 state.json 恢复。
	p2 := New(fp)
	if p2.HasRotatable(nil, "kimi-k3-1", "") {
		t.Fatal("重启后 kimi-k3-1 应恢复模型级连败降权，不可选")
	}
	if !p2.HasRotatable(nil, "deepseek-v4.1-flash", "") {
		t.Fatal("deepseek-v4.1-flash 未达阈，不应被降权阻塞")
	}

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后 u1 应存在")
	}
	byModel := make(map[string]ModelDegradeStatus, len(st.DegradedModels))
	for _, row := range st.DegradedModels {
		byModel[row.Model] = row
	}
	if len(byModel) != 2 {
		t.Fatalf("DegradedModels 应含 2 行: %+v", st.DegradedModels)
	}
	kimi, ok := byModel["kimi-k3-1"]
	if !ok {
		t.Fatalf("DegradedModels 应包含 kimi-k3-1: %+v", st.DegradedModels)
	}
	if kimi.Fails != 0 || kimi.Until.IsZero() {
		t.Fatalf("kimi-k3-1 应为已降权态（Fails=0, Until 非零）: %+v", kimi)
	}
	ds, ok := byModel["deepseek-v4.1-flash"]
	if !ok {
		t.Fatalf("DegradedModels 应包含 deepseek-v4.1-flash: %+v", st.DegradedModels)
	}
	if ds.Fails != 1 || !ds.Until.IsZero() {
		t.Fatalf("deepseek-v4.1-flash 应为未达阈进度（Fails=1, Until 零）: %+v", ds)
	}
}

package pool

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 手动权重 >1 必须显著提高被选中概率；权重 1 时现有权重逻辑完全不变。
func TestPickManualWeightPriority(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	dir := t.TempDir()
	for _, uid := range []string{"u1", "u2"} {
		fp := filepath.Join(dir, "workbuddy-"+uid+".json")
		a := &auth.Auth{UID: uid, AccessToken: "at", Weight: 1, FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatal(err)
		}
		p.Add(a)
		p.SetCredits(uid, 100, 0)
	}
	w := 20
	if ok, err := p.SetProfile("u1", &w, nil); !ok || err != nil {
		t.Fatalf("set profile ok=%v err=%v", ok, err)
	}
	counts := map[string]int{}
	for i := 0; i < 4000; i++ {
		counts[p.Pick().UID]++
	}
	if counts["u1"] <= counts["u2"]*3 {
		t.Fatalf("weighted account should dominate: %v", counts)
	}
}

// 资料设置落盘 auth 文件并保留状态；state.json 镜像也能恢复（重启前 auth 尚未重新加载）。
func TestSetProfilePersistsAndStateMirrors(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	p := New(statePath)
	p.Add(a)

	w, note := 9, "客户A"
	if ok, err := p.SetProfile("u1", &w, &note); !ok || err != nil {
		t.Fatalf("set profile ok=%v err=%v", ok, err)
	}
	got, ok := p.Status("u1")
	if !ok || got.Weight != 9 || got.Note != "客户A" {
		t.Fatalf("status=%+v ok=%v", got, ok)
	}
	raw, err := auth.LoadDir(dir)
	if err != nil || len(raw) != 1 || raw[0].WeightValue() != 9 || raw[0].Note != "客户A" {
		t.Fatalf("auth persist=%+v err=%v", raw, err)
	}
	p.Flush()
	// 新池只加载 state.json（模拟 Redis 快照/重启窗口），weight/note 应恢复。
	q := New(statePath)
	q.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	st, ok := q.Status("u1")
	if !ok || st.Weight != 9 || st.Note != "客户A" {
		t.Fatalf("state mirror=%+v ok=%v", st, ok)
	}
}

// auth 文件是资料权威源：手工把 weight 改回 1、清空 note 后，重启
// SyncToDir 不能用 state.json 里的旧镜像把旧值粘回来。
func TestUpsertAuthClearsProfileMirror(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", FilePath: fp, Weight: 9, Note: "旧备注"}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	p := New(statePath)
	p.Add(a)
	p.Flush()

	// 模拟手工编辑 auth 文件：weight=1, note 清空。
	weight, note := 1, ""
	if err := a.SetProfile(&weight, &note); err != nil {
		t.Fatal(err)
	}
	q := New(statePath)
	q.SyncToDir([]*auth.Auth{a})
	st, ok := q.Status("u1")
	if !ok || st.Weight != 1 || st.Note != "" {
		t.Fatalf("auth file must be authoritative after reload: %+v", st)
	}
}

// 批量启停：禁用是人工禁用缘由；启用走解冻，清冷却与熔断。
func TestSetAllEnabled(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2"} {
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 100, 0)
	}
	if n := p.SetAllEnabled(false); n != 2 {
		t.Fatalf("disable changed=%d", n)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if !st.Disabled || st.DisabledReason != ManualDisableReason {
			t.Fatalf("%s disabled state=%+v", uid, st)
		}
	}
	if n := p.SetAllEnabled(true); n != 2 {
		t.Fatalf("enable changed=%d", n)
	}
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := p.Status(uid); st.Disabled || st.Cooling {
			t.Fatalf("%s should be healthy after set-all enable: %+v", uid, st)
		}
	}
}

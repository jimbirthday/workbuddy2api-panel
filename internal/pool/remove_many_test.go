package pool

import (
	"os"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestRemoveManyDeletesAllAndPersistsOnce 验证批量移除「N 个账号只落盘一次」。
// 这是 RemoveMany 存在的唯一理由：循环调 Remove 会做 N 次全量序列化 + fsync +
// Redis 镜像，470 号必然超过面板 15s 超时。用 persistWriteHook 数实际写盘次数
// （mtime 会被同秒写入掩盖），再验证重新装载后的池状态确实反映了删除结果。
func TestRemoveManyDeletesAllAndPersistsOnce(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	for _, uid := range []string{"a", "b", "c", "d"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at"})
	}
	p.Flush() // 先落一次盘，确保后续比较基于完整基线

	writes := 0
	persistWriteHook = func() { writes++ }
	t.Cleanup(func() { persistWriteHook = nil })

	removed, missing := p.RemoveMany([]string{"a", "c", "ghost"})
	if writes != 1 {
		t.Fatalf("RemoveMany wrote state %d times, want exactly 1", writes)
	}
	if len(missing) != 1 || missing[0] != "ghost" {
		t.Fatalf("missing=%v want [ghost]", missing)
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v want 2 entries", removed)
	}
	for _, uid := range []string{"a", "c", "ghost"} {
		if _, ok := p.Status(uid); ok {
			t.Errorf("%s should not be in the pool", uid)
		}
	}
	for _, uid := range []string{"b", "d"} {
		if _, ok := p.Status(uid); !ok {
			t.Errorf("%s must survive", uid)
		}
	}
	// 落盘内容必须已经反映删除结果（不是「返回后才异步写」）。
	reloaded := New(fp)
	reloaded.RestoreFromSnapshot()
	if len(reloaded.List()) != 2 {
		t.Fatalf("reloaded accounts=%d want 2", len(reloaded.List()))
	}
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("state file missing: %v", err)
	}
}

// RemoveMany 的入参边界：空切片 / 全重复 / 不存在的 UID 都不能改动池。
func TestRemoveManyEdgeCases(t *testing.T) {
	p := New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at"})

	if removed, missing := p.RemoveMany(nil); len(removed) != 0 || len(missing) != 0 {
		t.Fatalf("nil uids: removed=%v missing=%v", removed, missing)
	}
	if removed, missing := p.RemoveMany([]string{"ghost", "ghost"}); len(removed) != 0 || len(missing) != 1 {
		t.Fatalf("all-missing: removed=%v missing=%v", removed, missing)
	}
	if _, ok := p.Status("a"); !ok {
		t.Fatal("a must be untouched")
	}
	// 重复 UID 只删一次，重复项不当成 missing。
	removed, missing := p.RemoveMany([]string{"a", "a"})
	if len(removed) != 1 || len(missing) != 0 {
		t.Fatalf("dup uids: removed=%v missing=%v", removed, missing)
	}
	if _, ok := p.Status("a"); ok {
		t.Fatal("a should be deleted")
	}
}

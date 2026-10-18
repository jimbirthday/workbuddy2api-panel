package session

import (
	"testing"
	"time"
)

// 同一会话连续命中在续期窗口内只镜像一次；窗口到期后再续期 Redis TTL，
// 避免高频多轮请求把同一 key 的 SetBind 不断排进写队列。
func TestTouchMirrorsAtMostOncePerWindow(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	if _, ok := r.Resolve("c1"); !ok {
		t.Fatal("first resolve failed")
	}
	if _, ok := r.Resolve("c1"); !ok {
		t.Fatal("second resolve failed")
	}
	st.mu.Lock()
	first := st.setBinds
	st.mu.Unlock()
	if first != 1 {
		t.Fatalf("setBinds after two quick hits=%d want 1", first)
	}

	// 把镜像时间拨到窗口之外，下一次命中必须重新 SetBind 续期。
	r.mu.Lock()
	e := r.entries["c1"]
	e.lastMirror = time.Now().Add(-2 * r.touchEvery)
	r.entries["c1"] = e
	r.mu.Unlock()
	if _, ok := r.Resolve("c1"); !ok {
		t.Fatal("third resolve failed")
	}
	st.mu.Lock()
	after := st.setBinds
	st.mu.Unlock()
	if after != 2 {
		t.Fatalf("setBinds after window expiry=%d want 2", after)
	}
}

// 绑定表达到上限后淘汰最久未活跃的条目；淘汰要镜像 DelBind，不能只删内存。
func TestBindingLimitEvictsOldest(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.maxBindings = 3
	r.Bind("old", "a1")
	r.Bind("b", "a1")
	r.Bind("c", "a1")

	r.mu.Lock()
	e := r.entries["old"]
	e.lastActive = time.Now().Add(-time.Hour)
	r.entries["old"] = e
	r.mu.Unlock()

	r.Bind("new", "a1")
	if _, ok := r.boundUID("old"); ok {
		t.Fatal("oldest binding should be evicted at capacity")
	}
	if n := r.Count(); n != 3 {
		t.Fatalf("count after eviction=%d want 3", n)
	}
	st.mu.Lock()
	del := st.delBinds
	st.mu.Unlock()
	if del != 1 {
		t.Fatalf("DelBind mirrors=%d want 1", del)
	}
}

// 空闲账号判定必须与 entries 同步：Bind 覆盖、Unbind、淘汰后计数不能残留，
// 否则新会话会把仍有绑定的账号误判为空闲，双段策略失真。
func TestBoundUIDIndexStaysInSync(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	r.Bind("c2", "a1")
	if got := r.boundUIDs["a1"]; got != 2 {
		t.Fatalf("a1 bound count=%d want 2", got)
	}
	r.Bind("c1", "a2") // 覆盖：a1 减一，a2 加一
	if r.boundUIDs["a1"] != 1 || r.boundUIDs["a2"] != 1 {
		t.Fatalf("after override counts a1=%d a2=%d", r.boundUIDs["a1"], r.boundUIDs["a2"])
	}
	if !r.Unbind("c2") || r.boundUIDs["a1"] != 0 {
		t.Fatalf("unbind should remove a1 count: %+v", r.boundUIDs)
	}
	r.maxBindings = 1
	r.Bind("c3", "a2")
	if _, ok := r.entries["c1"]; ok {
		t.Fatal("c1 should be evicted at limit 1")
	}
	if r.boundUIDs["a2"] != 1 {
		t.Fatalf("eviction must keep remaining count, got %d", r.boundUIDs["a2"])
	}
}

// 启动恢复同样受内存上限约束，不能把 Redis 里历史积累的绑定全量灌入。
func TestLoadFromStoreHonorsBindingLimit(t *testing.T) {
	st := newCountingStore()
	for _, k := range []string{"c1", "c2", "c3", "c4"} {
		st.binds[k] = "a1"
	}
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.maxBindings = 2
	r.LoadFromStore()
	if n := r.Count(); n != 2 {
		t.Fatalf("restored count=%d want 2", n)
	}
}

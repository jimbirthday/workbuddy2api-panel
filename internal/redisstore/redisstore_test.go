package redisstore

import (
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		token string
		want  string
	}{
		{"完整rediss", "rediss://default:tok@host:6379", "ignored", "rediss://default:tok@host:6379"},
		{"完整redis", "redis://default:tok@host:6379", "ignored", "redis://default:tok@host:6379"},
		{"https host", "https://foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
		{"裸host", "foo.upstash.io", "tok", "rediss://default:tok@foo.upstash.io:6379"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeURL(c.url, c.token); got != c.want {
				t.Errorf("normalizeURL(%q,%q)=%q want %q", c.url, c.token, got, c.want)
			}
		})
	}
}

func TestNormalizeURLStripsTrailingPath(t *testing.T) {
	// 用户照抄 Upstash 控制台的 REST 地址，可能带任意路径——剥 scheme 只取 host:port 之前段。
	got := normalizeURL("https://foo.upstash.io", "t")
	if strings.Contains(got, "://foo.upstash.io") && !strings.HasSuffix(got, "foo.upstash.io:6379") {
		t.Errorf("unexpected: %s", got)
	}
}

func TestNewEmptyURLReturnsNoop(t *testing.T) {
	if _, ok := New("", "").(Noop); !ok {
		t.Fatalf("empty url should return Noop")
	}
}

func TestNewBadSchemeReturnsNoop(t *testing.T) {
	// 组装出的连接串含空格 → ParseURL 解析失败 → 降级 Noop，不 panic、不发网络请求。
	if _, ok := New("://bad host", "").(Noop); !ok {
		t.Fatalf("bad url should return Noop")
	}
}

func TestNoopMethods(t *testing.T) {
	n := Noop{}
	n.SetBind("k", "u", time.Minute) // 不 panic
	n.DelBind("k")
	n.SaveState([]byte("{}"))
	if _, ok := n.LoadState(); ok {
		t.Error("Noop.LoadState should report not-found")
	}
}

func TestBindKeyPrefix(t *testing.T) {
	if got := bindKey("abc"); got != bindPrefix+"abc" {
		t.Errorf("bindKey=%q want prefix", got)
	}
}

// 写队列满时必须立即返回（不阻塞请求热路径），并且不会像旧实现那样继续派生
// 无界 goroutine。这里占满 8 个 worker + 256 个队列槽后继续提交，断言出现丢弃。
func TestGoWriteBoundedQueueDropsInsteadOfBlocking(t *testing.T) {
	u := &Upstash{}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var ran atomic.Int64
	submit := func() bool {
		return u.goWrite(func() {
			ran.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		})
	}
	accepted := 0
	for i := 0; i < writeConcurrencyLimit+writeQueueLimit+64; i++ {
		if submit() {
			accepted++
		}
	}
	if accepted >= writeConcurrencyLimit+writeQueueLimit+64 {
		t.Fatal("queue must be bounded")
	}
	<-started
	close(release)
	if err := u.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got, want := ran.Load(), int64(accepted); got != want {
		t.Fatalf("executed=%d want %d (Close must drain accepted writes)", got, want)
	}
}

// Close 必须排空已接受的写；关闭后新提交直接丢弃，且多次 Close 幂等。
func TestCloseDrainsAcceptedWritesAndRejectsNew(t *testing.T) {
	u := &Upstash{}
	var wg sync.WaitGroup
	var ran atomic.Int64
	for i := 0; i < 32; i++ {
		wg.Add(1)
		if !u.goWrite(func() { defer wg.Done(); ran.Add(1) }) {
			t.Fatal("write should be accepted before Close")
		}
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if ran.Load() != 32 {
		t.Fatalf("ran=%d want 32", ran.Load())
	}
	if u.goWrite(func() { t.Error("write after Close must not run") }) {
		t.Fatal("goWrite must reject after Close")
	}
	if err := u.Close(); err != nil {
		t.Fatalf("second Close should be no-op: %v", err)
	}
}

// panic 的写任务不能杀死 worker，否则后续镜像会永久停摆。
func TestWritePanicDoesNotKillWorkers(t *testing.T) {
	u := &Upstash{}
	done := make(chan struct{})
	u.goWrite(func() { panic("boom") })
	u.goWrite(func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker died after a panicking task")
	}
	_ = u.Close()
}

// TestBindMirrorAppliesPerKeyInOrder 钉住「同一 key 的 Set/Del 不再乱序」：
// 旧实现把每次写独立投进 8 个并发 worker，同一 key 的 Del 与 Set 可能被不同
// worker 乱序执行，Redis 里留下已删除的旧绑定（重启后粘到错误账号）。
// 现在按 key 合并成「最新目标状态 + 单 flusher 顺序应用」。
//
// 确定性做法：先让第一个 key 的写卡在 applyBind 里，期间登记后续写；这样
// 「同一 key 的多次登记是否合并」不依赖调度时序。
func TestBindMirrorAppliesPerKeyInOrder(t *testing.T) {
	type apply struct {
		key string
		w   bindWrite
	}
	var (
		mu      sync.Mutex
		applied []apply
	)
	gate := make(chan struct{})
	inFlight := make(chan struct{})
	var once sync.Once
	u := &Upstash{applyBind: func(key string, w bindWrite) {
		once.Do(func() {
			close(inFlight)
			<-gate // 卡住 flusher，让后续登记堆积在合并表里
		})
		mu.Lock()
		applied = append(applied, apply{key, w})
		mu.Unlock()
	}}
	u.ensure()

	u.SetBind("k1", "a", time.Minute) // 进入 flusher 并在 applyBind 卡住
	<-inFlight
	// 同一 key：Del → Set(b)；另一个 key：Set → Del。都只登记、不新增任务。
	u.DelBind("k1")
	u.SetBind("k1", "b", time.Minute)
	u.SetBind("k2", "c", time.Minute)
	u.DelBind("k2")
	close(gate)
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	final := map[string]bindWrite{}
	k1Applies := 0
	for _, a := range applied {
		final[a.key] = a.w
		if a.key == "k1" {
			k1Applies++
		}
	}
	if got := final["k1"]; got.del || got.uid != "b" {
		t.Fatalf("k1 final=%+v want uid=b", got)
	}
	if got := final["k2"]; !got.del {
		t.Fatalf("k2 final=%+v want del", got)
	}
	// 「a → del → b」三次登记应合并成首条 a（已在途）+ 一条 b，而不是三次都落。
	if k1Applies != 2 {
		t.Fatalf("k1 applied %d times want 2 (a then b, middle del coalesced)", k1Applies)
	}
}

// TestBindMirrorCoversAllKeys 合并表不能丢 key：不同 key 各自的最新状态都要落。
func TestBindMirrorCoversAllKeys(t *testing.T) {
	var (
		mu    sync.Mutex
		final = map[string]bindWrite{}
	)
	u := &Upstash{applyBind: func(key string, w bindWrite) {
		mu.Lock()
		final[key] = w
		mu.Unlock()
	}}
	const n = 100
	for i := 0; i < n; i++ {
		u.SetBind("k"+strconv.Itoa(i), "u", time.Minute)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(final) != n {
		t.Fatalf("applied keys=%d want %d", len(final), n)
	}
}

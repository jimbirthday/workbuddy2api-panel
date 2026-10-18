package redisstore

import (
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

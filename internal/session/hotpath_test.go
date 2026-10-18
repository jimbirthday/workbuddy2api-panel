package session

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestResolveHitDoesNotRebuildWholePool 钉住粘性命中路径的复杂度：可用列表很大时，
// 已绑定请求不应再为整池构建 map。这里用“列表只允许被读取一次”的探针确认：
// 命中只查绑定号所在候选，并返回原绑定。分配量由 hotpath benchmark 持续盯住。
func TestResolveHitDoesNotRebuildWholePool(t *testing.T) {
	avail := make([]string, 470)
	for i := range avail {
		avail[i] = fmt.Sprintf("u%03d", i)
	}
	reads := 0
	r := New(Config{
		TTL:   time.Minute,
		Store: newCountingStore(),
		Available: func() []string {
			reads++
			return avail
		},
	})
	uid, ok := r.Resolve("stable-session")
	if !ok {
		t.Fatal("initial resolve failed")
	}
	reads = 0
	for i := 0; i < 50; i++ {
		got, ok := r.Resolve("stable-session")
		if !ok || got != uid {
			t.Fatalf("resolve hit=%s,%v want %s,true", got, ok, uid)
		}
	}
	if reads != 50 {
		t.Fatalf("availability reads=%d want 50 (one scan per hit; no map rebuild)", reads)
	}
}

// TestResolveBoundAccountMissingReassigns 绑定号不在候选列表时必须重新分配，
// 不能因为快路径改成 Contains 就漏掉失效判定。
func TestResolveBoundAccountMissingReassigns(t *testing.T) {
	avail := []string{"a1", "a2"}
	r := New(Config{TTL: time.Minute, Store: newCountingStore(), Available: func() []string { return avail }})
	uid, ok := r.Resolve("session")
	if !ok {
		t.Fatal("resolve failed")
	}
	avail = []string{"a3"}
	got, ok := r.Resolve("session")
	if !ok || got != "a3" {
		t.Fatalf("stale binding resolve=%s,%v want a3,true (old=%s)", got, ok, uid)
	}
}

func BenchmarkResolveHit470(b *testing.B) {
	avail := make([]string, 470)
	for i := range avail {
		avail[i] = fmt.Sprintf("u%03d", i)
	}
	r := New(Config{TTL: time.Hour, Store: newCountingStore(), Available: func() []string { return avail }})
	if _, ok := r.Resolve("bench-session"); !ok {
		b.Fatal("warmup resolve failed")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Resolve("bench-session"); !ok {
			b.Fatal("resolve hit failed")
		}
	}
}

// BenchmarkResolveHitProductionProvider 模拟生产装配：每次调用 AvailableForModel
// 都会整池扫描、排序并展开虚拟实例。命中路径必须走 CanRoute 的单账号判定，
// 不能因校验而重建整张候选列表。
func BenchmarkResolveHitProductionProvider(b *testing.B) {
	avail := make([]string, 470)
	for i := range avail {
		avail[i] = fmt.Sprintf("u%03d", i)
	}
	calls := 0
	r := New(Config{
		TTL:       time.Hour,
		Store:     newCountingStore(),
		Available: func() []string { return avail },
		AvailableForModel: func(model string) []string {
			calls++
			out := make([]string, 0, len(avail)*3)
			for _, uid := range avail {
				out = append(out, uid, uid, uid)
			}
			return out
		},
		CanRoute: func(model, realm, uid string) bool {
			for _, candidate := range avail {
				if candidate == uid {
					return true
				}
			}
			return false
		},
	})
	if _, ok := r.ResolveForModel("bench-session", "glm-5.3"); !ok {
		b.Fatal("warmup resolve failed")
	}
	calls = 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.ResolveForModel("bench-session", "glm-5.3"); !ok {
			b.Fatal("resolve hit failed")
		}
	}
	if calls != 0 {
		b.Fatalf("fast path rebuilt full candidate list %d times", calls)
	}
}

func legacyDeriveKeyFields(msgs []KeyMessage) string {
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		text := messageTextRaw(m.Content)
		switch m.Role {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				firstUserText = userContentSignatureRaw(m.Content)
			}
		}
		if firstUserText != "" && systemText != "" {
			break
		}
	}
	if firstUserText == "" {
		return ""
	}
	return systemText + "\x00" + firstUserText
}

func benchmarkDeriveMessages(b *testing.B) []KeyMessage {
	b.Helper()
	body := []byte(`{"messages":[{"role":"system","content":"system prompt"},{"role":"user","content":[{"type":"text","text":"please inspect this image"},{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`)
	var req TurnKeyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		b.Fatal(err)
	}
	msgs := make([]KeyMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = KeyMessage{Role: m.Role, Content: m.Content}
	}
	return msgs
}

// BenchmarkDeriveKeyFieldsLegacyReadTwice 是优化前口径：user content 先按
// messageTextRaw 解析一次，再按 userContentSignatureRaw 解析一次。
func BenchmarkDeriveKeyFieldsLegacyReadTwice(b *testing.B) {
	msgs := benchmarkDeriveMessages(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = legacyDeriveKeyFields(msgs)
	}
}

func BenchmarkDeriveKeyFields(b *testing.B) {
	msgs := benchmarkDeriveMessages(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = deriveKeyFields(msgs)
	}
}

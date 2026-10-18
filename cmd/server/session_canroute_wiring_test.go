package main

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// TestSessionCanRouteWiringUsesProductionAdapter 钉住生产装配的 CanRoute 参数顺序。
// session.CanRoute 是 (model, realm, uid)，pool.UIDAvailableForModel 是
// (uid, model, realm)；两者都是三个 string，直接赋值能编译但运行时会全部失配。
// 这里用真实 Pool 跑一次“先分配、再命中”，并确认命中不会重建整池候选列表。
func TestSessionCanRouteWiringUsesProductionAdapter(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u-route"})
	availForModelCalls := 0
	r := session.New(session.Config{
		TTL:       time.Hour,
		Available: p.AvailableUIDs,
		AvailableForModel: func(model string) []string {
			availForModelCalls++
			return func() []string {
				a := p.AvailableUIDs()
				return append(a, a...)
			}()
		},
		CanRoute: func(model, realm, uid string) bool {
			return p.UIDAvailableForModel(uid, model, realm)
		},
	})

	first, ok := r.ResolveForModel("wiring-session", "cn:glm-5.3")
	if !ok || first != "u-route" {
		t.Fatalf("initial resolve = %q,%v want u-route,true", first, ok)
	}
	availForModelCalls = 0
	for i := 0; i < 5; i++ {
		got, ok := r.ResolveForModel("wiring-session", "cn:glm-5.3")
		if !ok || got != first {
			t.Fatalf("hit %d = %q,%v want %q,true", i, got, ok, first)
		}
	}
	if availForModelCalls != 0 {
		t.Fatalf("CanRoute hit rebuilt full candidate list %d times; adapter order is wrong", availForModelCalls)
	}
}

package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// HealthSummary 必须与选号口径一致：模型级冷却的号仍可能可服务（其他模型），
// 但在途占满/日额度用尽的号不能把探活误报成可服务。
func TestHealthSummaryMatchesServableSemantics(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	total, healthy, servable := p.HealthSummary()
	if total != 2 || healthy != 2 || !servable["cn"] || !servable["global"] {
		t.Fatalf("healthy pool summary = total=%d healthy=%d servable=%v", total, healthy, servable)
	}

	// 只有一个 global 号且被模型级冷却时，账号级仍 healthy，其它模型仍可选，
	// 因此探活保持可服务；这与 chat 的模型豁免语义一致。
	p.CooldownSoftForModel("g1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004")
	total, healthy, servable = p.HealthSummary()
	if total != 2 || healthy != 2 || !servable["global"] {
		t.Fatalf("model cooldown must not kill account-level health: total=%d healthy=%d servable=%v", total, healthy, servable)
	}

	// CN 号被在途占满后，cn 域不可服务，但 healthz 的 healthy 计数口径不变。
	p.SetMaxInFlight(1)
	if !p.Acquire("cn1") {
		t.Fatal("acquire cn1")
	}
	total, healthy, servable = p.HealthSummary()
	if total != 2 || healthy != 2 || servable["cn"] {
		t.Fatalf("in-flight-full must make realm unservable: total=%d healthy=%d servable=%v", total, healthy, servable)
	}
	p.Release("cn1")
}

func TestHealthSummaryHonorsDailyLimitFailOpen(t *testing.T) {
	p, f := newDailyPool(t, map[string]DailyTokenUsage{"cn1": {Tokens: 10}}, 10, 0)
	p.Add(&auth.Auth{UID: "cn1"})
	_, _, servable := p.HealthSummary()
	if servable["cn"] {
		t.Fatal("daily-limit exhausted account must not make healthz servable")
	}
	f.ok = false
	_, _, servable = p.HealthSummary()
	if !servable["cn"] {
		t.Fatal("provider failure must fail-open in HealthSummary")
	}
}

// HasRotatable 是 429 轮转的存在性查询，必须与 Pick 同口径，同时不能把
// tried / 模型冷却 / 日额度 / 在途 / 积分保底漏掉。
func TestHasRotatableMatchesPickFilters(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetModelRateOf(rates(map[string]map[string]string{"cn": {"paid": "1.62", "free": "0.00"}}))
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 30, 0)
	p.SetCredits("u2", 1000, 0)

	if !p.HasRotatable(map[string]bool{"u1": true}, "paid", "cn") {
		t.Fatal("u2 should be rotatable for paid model")
	}
	if p.HasRotatable(map[string]bool{"u1": true, "u2": true}, "paid", "cn") {
		t.Fatal("all tried accounts must not be reported rotatable")
	}

	// u2 对当前模型冷却后，只剩触底的 u1；积分保底应让存在性查询返回 false。
	p.CooldownSoftForModel("u2", time.Minute, time.Now().Add(time.Hour), "paid", "6004")
	if p.HasRotatable(nil, "paid", "cn") {
		t.Fatal("floor-blocked account must not satisfy HasRotatable")
	}
	// 免费模型上触底号仍可用，存在性查询必须与 Pick 一致。
	if !p.HasRotatable(nil, "free", "cn") {
		t.Fatal("floor-blocked account should remain rotatable for free model")
	}

	// 在途占满的号不满足存在性；释放后恢复。
	p.SetMaxInFlight(1)
	if !p.Acquire("u2") {
		t.Fatal("acquire u2")
	}
	if p.HasRotatable(map[string]bool{"u1": true}, "free", "cn") {
		t.Fatal("in-flight-full account must not satisfy HasRotatable")
	}
	p.Release("u2")
	if !p.HasRotatable(map[string]bool{"u1": true}, "free", "cn") {
		t.Fatal("released account should be rotatable again")
	}
}

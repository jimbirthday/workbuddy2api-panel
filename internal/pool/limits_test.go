package pool

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type fakeDailyUsage struct {
	mu   sync.Mutex
	data map[string]DailyTokenUsage
	ok   bool
}

func (f *fakeDailyUsage) DailyTokenUsageAt(time.Time) (DailyTokenUsageByUID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, f.ok
}

func (f *fakeDailyUsage) set(data map[string]DailyTokenUsage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = data
}

func newDailyPool(t *testing.T, usage map[string]DailyTokenUsage, daily, perModel int64) (*Pool, *fakeDailyUsage) {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	p := New("")
	p.SetCostExploreInterval(0)
	f := &fakeDailyUsage{data: usage, ok: true}
	p.SetDailyLimits(daily, perModel)
	p.SetDailyTokenUsageProvider(f)
	return p, f
}

func TestDailyTokenLimitFiltersAndDoesNotPark(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{
		"over": {Tokens: 1000},
	}, 1000, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "over"})
	p.Add(&auth.Auth{UID: "fresh"})

	for i := 0; i < 10; i++ {
		a := p.PickExcludingForRealm(nil, "glm-5.3", "")
		if a == nil || a.UID != "fresh" {
			t.Fatalf("pick %d = %+v, want fresh (over daily limit filtered)", i, a)
		}
	}
	st, ok := p.Status("over")
	if !ok {
		t.Fatal("over account disappeared")
	}
	if st.Cooling || st.Disabled || st.Paused {
		t.Fatalf("daily limit must not park the account: %+v", st)
	}

	p.Remove("fresh")
	if a := p.PickExcludingForRealm(nil, "glm-5.3", ""); a != nil {
		t.Fatalf("all accounts over limit should yield nil, got %s", a.UID)
	}
}

func TestDailyTokenLimitRecoversNextDay(t *testing.T) {
	p, f := newDailyPool(t, map[string]DailyTokenUsage{
		"only": {Tokens: 1000},
	}, 1000, 0)
	p.Add(&auth.Auth{UID: "only"})

	if a := p.PickExcludingForRealm(nil, "glm-5.3", ""); a != nil {
		t.Fatalf("want nil while over quota, got %s", a.UID)
	}
	f.set(map[string]DailyTokenUsage{"only": {Tokens: 0}})
	a := p.PickExcludingForRealm(nil, "glm-5.3", "")
	if a == nil || a.UID != "only" {
		t.Fatalf("after local-midnight reset want only, got %+v", a)
	}
}

func TestModelDailyTokenLimitIsModelIsolated(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{
		"u1": {Tokens: 100, Models: map[string]int64{"model-a": 500}},
	}, 10000, 500)
	p.Add(&auth.Auth{UID: "u1"})

	if a := p.PickExcludingForRealm(nil, "model-a", ""); a != nil {
		t.Fatalf("model-a over per-model quota should be filtered, got %s", a.UID)
	}
	if a := p.PickExcludingForRealm(nil, "model-b", ""); a == nil || a.UID != "u1" {
		t.Fatalf("model-b must stay available on same account, got %+v", a)
	}
}

// 调用方传入带域前缀的模型名时，按模型额度不能因为 key 不匹配而绕过。
func TestDailyTokenModelLimitCanonicalizesRealmPrefix(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{
		"u1": {Tokens: 100, Models: map[string]int64{"glm-5.3": 500}},
	}, 10000, 500)
	p.Add(&auth.Auth{UID: "u1"})
	for _, model := range []string{"glm-5.3", "cn:glm-5.3", "global:glm-5.3"} {
		if a := p.PickExcludingForRealm(nil, model, ""); a != nil {
			t.Fatalf("model=%q over per-model quota should be filtered, got %s", model, a.UID)
		}
	}
}

func TestDailyTokenProviderFailureFailsOpen(t *testing.T) {
	p, f := newDailyPool(t, map[string]DailyTokenUsage{
		"u1": {Tokens: 999999, Models: map[string]int64{"model-a": 999999}},
	}, 1, 1)
	f.ok = false
	p.Add(&auth.Auth{UID: "u1"})

	if a := p.PickExcludingForRealm(nil, "model-a", ""); a == nil || a.UID != "u1" {
		t.Fatalf("provider failure must fail-open, got %+v", a)
	}
	if a := p.PickByUIDForModel("u1", "model-a"); a == nil {
		t.Fatal("sticky path must also fail-open when provider read fails")
	}
	// ok=false 必须是“未知 → 放行”，不能被误用成“计数为空/清零”。
	f.set(map[string]DailyTokenUsage{"u1": {Tokens: 0}})
	if a := p.PickExcludingForRealm(nil, "model-a", ""); a == nil {
		t.Fatal("ok=false with an explicit snapshot must still fail-open")
	}
}

func TestDailyTokenLimitRealmIsolation(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{
		"cn-over": {Tokens: 1000},
	}, 1000, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "cn-over", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "global-ok", Domain: "www.workbuddy.ai"})

	if a := p.PickExcludingForRealm(nil, "glm-5.3", "cn"); a != nil {
		t.Fatalf("cn realm has only over-quota account, want nil, got %s", a.UID)
	}
	if a := p.PickExcludingForRealm(nil, "glm-5.3", "global"); a == nil || a.UID != "global-ok" {
		t.Fatalf("global realm must be isolated from cn quota, got %+v", a)
	}
	got := p.AvailableUIDsForModelRealm("glm-5.3", "cn")
	if len(got) != 0 {
		t.Fatalf("AvailableUIDsForModelRealm(cn)=%v, want empty", got)
	}
}

func TestDailyTokenLimitZeroDisables(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{
		"u1": {Tokens: 999999, Models: map[string]int64{"model-a": 999999}},
	}, 0, 0)
	p.Add(&auth.Auth{UID: "u1"})

	if a := p.PickExcludingForRealm(nil, "model-a", ""); a == nil || a.UID != "u1" {
		t.Fatalf("0 = disabled, want u1, got %+v", a)
	}
}

func TestDailyTokenLimitBoundaryAtLimit(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{
		"u1": {Tokens: 1000, Models: map[string]int64{"model-a": 500}},
	}, 1000, 500)
	p.Add(&auth.Auth{UID: "u1"})

	if a := p.PickExcludingForRealm(nil, "model-b", ""); a != nil {
		t.Fatalf("used == limit must block (>= semantics), got %s", a.UID)
	}
	p.SetDailyLimits(1001, 501)
	if a := p.PickExcludingForRealm(nil, "model-a", ""); a == nil || a.UID != "u1" {
		t.Fatalf("used < limit must allow, got %+v", a)
	}
}

func TestDailyTokenLimitAppliesToStickyAndFallback(t *testing.T) {
	p, f := newDailyPool(t, map[string]DailyTokenUsage{
		"sticky": {Tokens: 1000, Models: map[string]int64{"model-a": 1000}},
	}, 1000, 0)
	p.Add(&auth.Auth{UID: "sticky"})

	if a := p.PickByUIDForModel("sticky", "model-a"); a != nil {
		t.Fatalf("sticky over-quota account must return nil, got %s", a.UID)
	}

	// 唯一账号进入已到期的软冷却后走 earliest-expiry 兜底；额度用尽时兜底也不得
	// 把它捞回来，额度恢复后兜底路径同样恢复。
	p.mu.Lock()
	p.byUID["sticky"].until = time.Now().Add(-time.Second)
	p.byUID["sticky"].coolKind = CoolSoft
	p.mu.Unlock()
	if a := p.PickExcludingForRealm(nil, "model-a", ""); a != nil {
		t.Fatalf("fallback must respect daily quota, got %s", a.UID)
	}

	// 次日恢复后，粘性与兜底路径同时恢复。
	f.set(map[string]DailyTokenUsage{})
	if a := p.PickByUIDForModel("sticky", "model-a"); a == nil {
		t.Fatal("sticky path should recover after daily reset")
	}
	if a := p.PickExcludingForRealm(nil, "model-a", ""); a == nil || a.UID != "sticky" {
		t.Fatalf("fallback should recover after daily reset, got %+v", a)
	}
}

func TestDailyTokenLimitsHotUpdate(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{"u1": {Tokens: 10}}, 0, 0)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDailyLimits(10, 0)
	if a := p.PickExcludingForRealm(nil, "m", ""); a != nil {
		t.Fatalf("hot-enabled limit should filter immediately, got %s", a.UID)
	}
	p.SetDailyLimits(0, 0)
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil {
		t.Fatal("hot-disabled limit should allow immediately")
	}
	if d, m := p.DailyLimits(); d != 0 || m != 0 {
		t.Fatalf("DailyLimits = %d/%d, want 0/0", d, m)
	}
}

func TestDailyTokenLimitAvailableUIDs(t *testing.T) {
	p, _ := newDailyPool(t, map[string]DailyTokenUsage{"u1": {Tokens: 10}}, 10, 0)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if got := p.AvailableUIDs(); !reflect.DeepEqual(got, []string{"u2"}) {
		t.Fatalf("AvailableUIDs=%v, want [u2]", got)
	}
	if got := p.WeightedAvailableUIDsForModelRealm("m", ""); !reflect.DeepEqual(got, []string{"u2"}) {
		t.Fatalf("WeightedAvailableUIDsForModelRealm=%v, want [u2]", got)
	}
}

func TestServableNowHonorsAccountDailyLimit(t *testing.T) {
	p, f := newDailyPool(t, map[string]DailyTokenUsage{"u1": {Tokens: 10}}, 10, 0)
	p.Add(&auth.Auth{UID: "u1"})
	if p.ServableNow() {
		t.Fatal("account over daily token limit must make pool unservable for this realm")
	}
	f.ok = false
	if !p.ServableNow() {
		t.Fatal("provider failure must fail-open and keep the pool servable")
	}
}

// 只配置按模型额度时，探活没有请求模型上下文。不能把「今天用过的模型」当成
// 「全部模型」：账号在 m1 上触顶，不代表尚未使用的 m2 不可选，探活必须保持
// 可服务（与 pick 在 m2 上仍能选中该账号一致）。
func TestServableForRealmModelOnlyLimitFailsOpen(t *testing.T) {
	p, f := newDailyPool(t, map[string]DailyTokenUsage{
		"u1": {Tokens: 100, Models: map[string]int64{"model-a": 500, "model-b": 500}},
	}, 0, 500)
	p.Add(&auth.Auth{UID: "u1"})
	if !p.ServableForRealm("") {
		t.Fatal("model-only limits must not make healthz unservable without a request model")
	}

	f.set(map[string]DailyTokenUsage{
		"u1": {Tokens: 100, Models: map[string]int64{"model-a": 500, "model-b": 499}},
	})
	if !p.ServableForRealm("") {
		t.Fatal("one model below per-model limit must keep pool servable")
	}

	// 无任何模型观测时同样不能误判不可服务（例如刚启动）。
	f.set(map[string]DailyTokenUsage{"u1": {Tokens: 0}})
	if !p.ServableForRealm("") {
		t.Fatal("account with no model usage must remain servable")
	}
}

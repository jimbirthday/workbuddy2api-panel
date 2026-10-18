package pool

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func snapshotTestPool(t *testing.T) *Pool {
	t.Helper()
	p := New("")
	p.Add(&auth.Auth{UID: "u2", Nickname: "乙", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	return p
}

func TestUsageRowsIdentityOnlyStableOrder(t *testing.T) {
	p := snapshotTestPool(t)
	p.NoteModelCost("u1", "glm-5.3", 1, 1000)
	p.RecordTokenUsage("u1", TokenUsageDelta{Model: "glm-5.3"})

	rows := p.UsageRows()
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	if rows[0].UID != "u1" || rows[0].Nickname != "甲" || rows[0].Realm != "cn" {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].UID != "u2" || rows[1].Nickname != "乙" || rows[1].Realm != "global" {
		t.Errorf("second row = %+v", rows[1])
	}
}

func TestConnectModelRowsProjectsCostsAndLastModel(t *testing.T) {
	p := snapshotTestPool(t)
	now := time.Now()
	p.mu.Lock()
	p.byUID["u1"].modelCost = map[string]modelCostEntry{}
	p.byUID["u1"].modelCost["stale-model"] = modelCostEntry{CostPer1k: 1, LastSeen: now.Add(-modelCostTTL - time.Minute), Samples: 1}
	p.byUID["u1"].modelCost["glm-5.3"] = modelCostEntry{CostPer1k: 0, LastSeen: now, Samples: 2}
	p.byUID["u1"].tokenUsage.LastModel = "kimi-k3"
	p.mu.Unlock()

	rows := p.ConnectModelRows(now)
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	byUID := map[string]ConnectModelRow{}
	for _, row := range rows {
		byUID[row.UID] = row
	}
	u1 := byUID["u1"]
	if u1.Realm != "cn" || u1.LastModel != "kimi-k3" {
		t.Errorf("u1 identity = %+v", u1)
	}
	if len(u1.ModelCosts) != 1 || u1.ModelCosts[0].Model != "glm-5.3" {
		t.Errorf("u1 costs must exclude expired rows: %+v", u1.ModelCosts)
	}
	if u1.ModelCosts[0].Samples != 2 || u1.ModelCosts[0].CostPer1k != 0 {
		t.Errorf("u1 cost row = %+v", u1.ModelCosts[0])
	}
}

func TestUsageRowsDoesNotSerializeStatusFields(t *testing.T) {
	p := snapshotTestPool(t)
	p.Cooldown("u1", CoolSoft, time.Hour, "429")
	rows := p.UsageRows()
	if len(rows) != 2 || rows[0].UID != "u1" {
		t.Fatalf("rows = %+v", rows)
	}
	// 轻量行只有身份字段；任何 Status 字段都不应出现在 JSON 里。
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"cooling", "cool_remaining_sec", "model_costs", "last_model", "credits"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("UsageRows leaked status field %q: %s", forbidden, b)
		}
	}
}

// TaskAccounts 是调度器保号/旅行/连登遍历的轻量入口。它必须保持与旧的
// List+AuthByUID 相同的筛选口径：禁用号默认跳过、开关打开时纳入，暂停号始终
// 纳入（paused 只退出选号）；输出按 UID 稳定排序。
func TestTaskAccountsFilterAndStableOrder(t *testing.T) {
	p := snapshotTestPool(t)
	p.Add(&auth.Auth{UID: "u0", Nickname: "丙"})
	p.Disable("u2", "manual disable (test)")
	p.Pause("u1")

	got := p.TaskAccounts(false)
	if len(got) != 2 || got[0].UID != "u0" || got[1].UID != "u1" {
		t.Fatalf("default task accounts = %+v want u0,u1 (paused kept, disabled skipped)", got)
	}
	if got[1].Auth == nil || got[1].Auth.UID != "u1" {
		t.Fatalf("task account must carry auth pointer: %+v", got[1])
	}
	if !got[1].Paused {
		t.Fatalf("paused state must be projected: %+v", got[1])
	}

	all := p.TaskAccounts(true)
	if len(all) != 3 || all[2].UID != "u2" {
		t.Fatalf("includeDisabled task accounts = %+v want u0,u1,u2", all)
	}
}

// Overview 必须与三个既有读路径逐项一致：计数（paused 单列）、账号 Status
// 与模型锁视图。任何一个口径漂移，面板首页都会与 /status、模型页互相矛盾。
func TestOverviewMatchesExistingViews(t *testing.T) {
	p := New("")
	base := time.Now()
	for i := 0; i < 4; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i), Nickname: fmt.Sprintf("n%d", i)})
	}
	p.Pause("u1")
	p.Disable("u2", "manual disable (panel)")
	p.Cooldown("u3", CoolSoft, time.Hour, "429 rate limit")
	p.mu.Lock()
	p.byUID["u0"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: base.Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p.byUID["u3"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: base.Add(2 * time.Hour), Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	snap := p.Overview()
	total, healthy, cooling, disabled, paused, inFlightFull := p.CountsDetailedWithPaused()
	if snap.Total != total || snap.Healthy != healthy || snap.Cooling != cooling ||
		snap.Disabled != disabled || snap.Paused != paused || snap.InFlightFull != inFlightFull {
		t.Fatalf("overview counts=%+v want total=%d healthy=%d cooling=%d disabled=%d paused=%d inFlight=%d",
			snap, total, healthy, cooling, disabled, paused, inFlightFull)
	}
	if got, want := len(snap.Accounts), len(p.List()); got != want {
		t.Fatalf("overview accounts=%d want %d", got, want)
	}
	for i, want := range p.List() {
		if got := snap.Accounts[i]; got.UID != want.UID || got.Cooling != want.Cooling ||
			got.CoolRemaining != want.CoolRemaining || len(got.RateLimitedModels) != len(want.RateLimitedModels) {
			t.Fatalf("account row %d = %+v want %+v", i, got, want)
		}
	}
	gotLocks, wantLocks := snap.ModelLocks, p.ModelLockView()
	if fmt.Sprint(gotLocks) != fmt.Sprint(wantLocks) {
		t.Fatalf("model locks = %+v want %+v", gotLocks, wantLocks)
	}
	if len(gotLocks) != 1 || gotLocks[0].State != "locked" || gotLocks[0].Locked != 2 {
		t.Fatalf("unexpected lock view: %+v", gotLocks)
	}
}

// Overview 单次遍历的基线：与 List+ModelLockView+计数三路径的总成本对比，
// 防止以后首页又退回多次全池扫描。
func BenchmarkOverview470(b *testing.B) {
	p := benchPoolN(470)
	base := time.Now().Add(time.Hour)
	p.mu.Lock()
	for i := 0; i < 100; i++ {
		e := p.byUID[fmt.Sprintf("u%03d", i)]
		e.modelCooldowns = map[string]modelCooldown{"glm-5.3": {Until: base}}
	}
	p.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap := p.Overview(); len(snap.Accounts) != 470 || len(snap.ModelLocks) != 1 {
			b.Fatalf("snapshot=%d/%d", len(snap.Accounts), len(snap.ModelLocks))
		}
	}
}

// BenchmarkStatusSnapshot470Admin 与 count/list/model-lock 多扫描路径对照。
func BenchmarkStatusSnapshot470Admin(b *testing.B) {
	p := benchPoolN(470)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap := p.StatusSnapshot(true); len(snap.Accounts) != 470 {
			b.Fatal(len(snap.Accounts))
		}
	}
}

// BenchmarkStatusSnapshot470ClientKey 业务 Key 路径不构建完整 Status。
func BenchmarkStatusSnapshot470ClientKey(b *testing.B) {
	p := benchPoolN(470)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap := p.StatusSnapshot(false); snap.Accounts != nil {
			b.Fatal("client path built accounts")
		}
	}
}

// TestStatusSnapshotMatchesExistingViews 保证单次快照与既有计数/列表/模型锁
// 路径逐项一致；/status 的聚合优化不能改变任何对外口径。
func TestStatusSnapshotMatchesExistingViews(t *testing.T) {
	p := New("")
	base := time.Now()
	for i := 0; i < 4; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i), Nickname: fmt.Sprintf("n%d", i)})
	}
	p.Pause("u2")
	p.Cooldown("u3", CoolSoft, time.Hour, "429 rate limit")
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: base.Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	snap := p.StatusSnapshot(true)
	total, healthy, cooling, disabled, inFlight := p.CountsDetailed()
	if snap.Total != total || snap.Healthy != healthy || snap.Cooling != cooling ||
		snap.Disabled != disabled || snap.InFlightFull != inFlight {
		t.Fatalf("status counts=%+v want total=%d healthy=%d cooling=%d disabled=%d inFlight=%d",
			snap, total, healthy, cooling, disabled, inFlight)
	}
	for _, realm := range []string{"cn", "global"} {
		rt, rh, rc, rd, rf := p.CountsDetailedForRealm(realm)
		got := snap.Realms[realm]
		if got.Total != rt || got.Healthy != rh || got.Cooling != rc || got.Disabled != rd || got.InFlightFull != rf {
			t.Fatalf("realm %s counts=%+v want %d/%d/%d/%d/%d", realm, got, rt, rh, rc, rd, rf)
		}
	}
	if fmt.Sprint(snap.Accounts) != fmt.Sprint(p.List()) {
		t.Fatalf("status accounts=%+v want %+v", snap.Accounts, p.List())
	}
	if fmt.Sprint(snap.ModelLocks) != fmt.Sprint(p.ModelLockView()) {
		t.Fatalf("status model locks=%+v want %+v", snap.ModelLocks, p.ModelLockView())
	}
	if hidden := p.StatusSnapshot(false); hidden.Accounts != nil {
		t.Fatalf("client-key snapshot must not build accounts: %+v", hidden.Accounts)
	}
}

func BenchmarkOverviewLegacyThreePasses470(b *testing.B) {
	p := benchPoolN(470)
	base := time.Now().Add(time.Hour)
	p.mu.Lock()
	for i := 0; i < 100; i++ {
		e := p.byUID[fmt.Sprintf("u%03d", i)]
		e.modelCooldowns = map[string]modelCooldown{"glm-5.3": {Until: base}}
	}
	p.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, _, _, _ = p.CountsDetailedWithPaused()
		_ = p.List()
		_ = p.ModelLockView()
	}
}

// BenchmarkPanelPoolList470 是轻量快照的对照组：完整 List 会为 470 个账号
// 构建 Status（限流台账、冷却剩余秒、成本行等），用于量化面板热路径的放大。
func BenchmarkPanelPoolList470(b *testing.B) {
	p := benchPoolN(470)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := p.List(); len(rows) != 470 {
			b.Fatal(len(rows))
		}
	}
}

func BenchmarkUsageRows470(b *testing.B) {
	p := benchPoolN(470)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := p.UsageRows(); len(rows) != 470 {
			b.Fatal(len(rows))
		}
	}
}

func BenchmarkConnectModelRows470(b *testing.B) {
	p := benchPoolN(470)
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rows := p.ConnectModelRows(now); len(rows) != 470 {
			b.Fatal(len(rows))
		}
	}
}

func benchPoolN(n int) *Pool {
	p := New("")
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%03d", i), Nickname: "nick"})
	}
	return p
}

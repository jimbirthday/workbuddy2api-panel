package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func remainingPanelGet(t *testing.T, p *Panel, path string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer admin-super-secret")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out, rr.Body.String()
}

// 剩余额度接口：账号/模型两个维度都要返回，限额 0 表示关闭，必须给出 null
// 而不是伪造 0；有配额时 remaining 按 limit-used 计算，达限行显式标记。
func TestUsageRemainingAccountAndModelLimits(t *testing.T) {
	rec := usage.New("")
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rec.Add(day.Add(time.Hour), "cn", "u1", "glm-5.3", usage.Delta{TotalTokens: 80, HasTotal: true}, true)
	rec.Add(day.Add(2*time.Hour), "cn", "u1", "kimi-k3", usage.Delta{TotalTokens: 20, HasTotal: true}, true)
	rec.Add(day.Add(3*time.Hour), "cn", "u2", "glm-5.3", usage.Delta{TotalTokens: 5, HasTotal: true}, true)

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	pl.Add(&auth.Auth{UID: "u2", Nickname: "乙"})
	pl.SetDailyLimits(100, 50)

	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl, Usage: rec})
	code, out, body := remainingPanelGet(t, p, "/panel/api/usage/remaining")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if out["data_available"] != true || out["day"] != day.Format("2006-01-02") {
		t.Fatalf("window metadata wrong: %+v", out)
	}
	limits := out["limits"].(map[string]any)
	if limits["account"].(float64) != 100 || limits["model"].(float64) != 50 ||
		limits["account_enabled"] != true || limits["model_enabled"] != true {
		t.Fatalf("limits = %+v", limits)
	}

	accounts := out["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(accounts))
	}
	byUID := map[string]map[string]any{}
	for _, raw := range accounts {
		a := raw.(map[string]any)
		byUID[a["uid"].(string)] = a
	}
	u1 := byUID["u1"]
	if u1["used"].(float64) != 100 || u1["remaining"].(float64) != 0 || u1["limited"] != true {
		t.Fatalf("u1 account row = %+v", u1)
	}
	u2 := byUID["u2"]
	if u2["used"].(float64) != 5 || u2["remaining"].(float64) != 95 || u2["limited"] != false {
		t.Fatalf("u2 account row = %+v", u2)
	}
	models := u1["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("u1 models=%d want 2", len(models))
	}
	byModel := map[string]map[string]any{}
	for _, raw := range models {
		m := raw.(map[string]any)
		byModel[m["model"].(string)] = m
	}
	if m := byModel["glm-5.3"]; m["used"].(float64) != 80 || m["remaining"].(float64) != 0 || m["limited"] != true {
		t.Fatalf("glm row = %+v", m)
	}
	if m := byModel["kimi-k3"]; m["used"].(float64) != 20 || m["remaining"].(float64) != 30 || m["limited"] != false {
		t.Fatalf("kimi row = %+v", m)
	}
	summary := out["summary"].(map[string]any)
	if summary["limited_accounts"].(float64) != 1 || summary["limited_models"].(float64) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

// 限额 0 = 关闭：remaining 必须是 null，不能显示成 0 可用或 0 剩余。
func TestUsageRemainingZeroLimitIsDisabled(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	rec := usage.New("")
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rec.Add(day.Add(time.Hour), "cn", "u1", "glm-5.3", usage.Delta{TotalTokens: 7, HasTotal: true}, true)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl, Usage: rec})

	_, out, _ := remainingPanelGet(t, p, "/panel/api/usage/remaining")
	limits := out["limits"].(map[string]any)
	if limits["account_enabled"] != false || limits["model_enabled"] != false {
		t.Fatalf("zero limits must be disabled: %+v", limits)
	}
	a := out["accounts"].([]any)[0].(map[string]any)
	if a["remaining"] != nil || a["limit_enabled"] != false || a["limited"] != false {
		t.Fatalf("account row with zero limit = %+v", a)
	}
	m := a["models"].([]any)[0].(map[string]any)
	if m["remaining"] != nil || m["limit_enabled"] != false || m["limited"] != false {
		t.Fatalf("model row with zero limit = %+v", m)
	}
}

// 没有用量记录器/无快照时必须显式 data_available=false + reason，不能返回
// used=0 让人误以为今日额度完全可用。
func TestUsageRemainingNoSnapshotIsExplicit(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.SetDailyLimits(100, 10)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl})
	_, out, body := remainingPanelGet(t, p, "/panel/api/usage/remaining")
	if out["data_available"] != false || out["reason"] == "" {
		t.Fatalf("missing snapshot must be explicit: %s", body)
	}
	a := out["accounts"].([]any)[0].(map[string]any)
	if a["used"] != nil || a["remaining"] != nil {
		t.Fatalf("used/remaining must be null without snapshot: %+v", a)
	}
}

// 活跃但从未加载/写入任何桶的记录器同样没有可信快照：DailyTokenUsageAt
// 会返回空 map + ok=true，面板不能因此把 used=0 / remaining=全额度当成事实。
func TestUsageRemainingEmptyLiveRecorderIsExplicit(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.SetDailyLimits(100, 10)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl, Usage: usage.New("")})
	_, out, body := remainingPanelGet(t, p, "/panel/api/usage/remaining")
	if out["data_available"] != false || out["reason"] == "" {
		t.Fatalf("empty recorder must be explicit: %s", body)
	}
	a := out["accounts"].([]any)[0].(map[string]any)
	if a["used"] != nil || a["remaining"] != nil {
		t.Fatalf("used/remaining must be null for empty recorder: %+v", a)
	}
}

// 只统计本地自然日：昨天与明天的桶都不能混进今日剩余。
func TestUsageRemainingLocalDayWindow(t *testing.T) {
	loc := time.Local
	day := time.Date(2026, 10, 18, 0, 0, 0, 0, loc)
	r := usage.New("")
	r.Add(day.Add(-time.Hour), "cn", "u1", "m", usage.Delta{TotalTokens: 900, HasTotal: true}, true)
	r.Add(day.Add(2*time.Hour), "cn", "u1", "m", usage.Delta{TotalTokens: 12, HasTotal: true}, true)
	r.Add(day.Add(25*time.Hour), "cn", "u1", "m", usage.Delta{TotalTokens: 800, HasTotal: true}, true)
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.SetDailyLimits(100, 0)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl, Usage: r})

	got := p.remainingUsageSnapshot(day.Add(5 * time.Hour))
	a := got["accounts"].([]remainingUsageAccount)[0]
	if a.Used == nil || *a.Used != 12 || a.Remaining == nil || *a.Remaining != 88 {
		t.Fatalf("local day window wrong: %+v", a)
	}
}

// 接入信息绝不回显真实密钥：响应与示例里都只能出现占位符。
func TestConnectNeverEchoesSecrets(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "admin-super-secret"})
	code, out, body := remainingPanelGet(t, p, "/panel/api/connect")
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, body)
	}
	for _, secret := range []string{"admin-super-secret", "client-super-secret", "access_token", "refresh_token", "device_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("connect response leaked %q: %s", secret, body)
		}
	}
	if out["api_key_placeholder"] != "$WB_API_KEY" {
		t.Fatalf("placeholder = %v", out["api_key_placeholder"])
	}
	if !strings.Contains(body, "$WB_API_KEY") {
		t.Fatalf("examples must use $WB_API_KEY placeholder: %s", body)
	}
	if out["auth_required"] != true || out["v1_url"] == "" || out["base_url"] == "" {
		t.Fatalf("connect metadata incomplete: %+v", out)
	}
	examples := out["examples"].(map[string]any)
	for _, key := range []string{"openai_sdk", "curl", "anthropic_env", "anthropic_messages", "responses_api"} {
		text := examples[key].(string)
		if !strings.Contains(text, "WB_API_KEY") {
			t.Fatalf("example %s missing WB_API_KEY reference: %v", key, examples[key])
		}
	}
}

// Responses / Anthropic Messages 已是真实路由，接入页不能再给出“未实现/需兼容层”
// 的错误示例；同时仍只能出现占位符。
func TestConnectExamplesMatchImplementedProtocols(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "admin-super-secret"})
	_, out, body := remainingPanelGet(t, p, "/panel/api/connect")
	examples := out["examples"].(map[string]any)
	for _, key := range []string{"anthropic_messages", "responses_api"} {
		text, _ := examples[key].(string)
		if text == "" || !strings.Contains(text, "$WB_API_KEY") {
			t.Fatalf("%s example missing placeholder: %q", key, text)
		}
	}
	if !strings.Contains(examples["anthropic_messages"].(string), "/v1/messages") {
		t.Fatalf("anthropic example must target /v1/messages: %v", examples["anthropic_messages"])
	}
	if !strings.Contains(examples["responses_api"].(string), "/v1/responses") {
		t.Fatalf("responses example must target /v1/responses: %v", examples["responses_api"])
	}
	for _, bad := range []string{"未实现", "需经", "兼容层（如"} {
		if strings.Contains(body, bad) {
			t.Fatalf("connect response still claims protocol unavailable (%q): %s", bad, body)
		}
	}
}

// 接入示例必须使用请求 Host 推导 base_url，并正确处理反代的 https 头。
func TestConnectUsesRequestHostAndForwardedProto(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "admin-super-secret"})
	req := httptest.NewRequest(http.MethodGet, "http://internal:7863/panel/api/connect", nil)
	req.Host = "gateway.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Authorization", "Bearer admin-super-secret")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "https://gateway.example.com/v1") {
		t.Fatalf("base url not derived from host/proto: %s", rr.Body)
	}
}

// Host 只用于展示；带 userinfo、路径或空白时必须回落，不能把凭证/注入内容
// 拼进示例或复制内容。
func TestConnectRejectsUnsafeHost(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "admin-super-secret"})
	for _, host := range []string{"user:pass@evil.example", "gateway/../../etc", "a b", "x\ty"} {
		req := httptest.NewRequest(http.MethodGet, "http://internal:7863/panel/api/connect", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer admin-super-secret")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		body := rr.Body.String()
		if rr.Code != http.StatusOK {
			t.Fatalf("host %q code=%d body=%s", host, rr.Code, body)
		}
		if strings.Contains(body, "user:pass@") || strings.Contains(body, "gateway/..") {
			t.Fatalf("unsafe host leaked into response: %s", body)
		}
		if !strings.Contains(body, "127.0.0.1") {
			t.Fatalf("unsafe host %q must fall back to loopback: %s", host, body)
		}
	}
}

// 客户端 Key 只表示网关鉴权已启用；管理面板是否要求鉴权只看顶层 api_key。
func TestConnectClientAuthUsesLiveKeyring(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{
		APIKey: "admin-super-secret",
		Keys:   []httpauth.Key{{ID: "client-a", Key: "client-secret", Enabled: true}},
	})
	p := New(Config{Version: "test", Live: live})
	_, out, _ := remainingPanelGet(t, p, "/panel/api/connect")
	if out["auth_required"] != true || out["panel_auth_required"] != true {
		t.Fatalf("client/panel auth flags drifted: %+v", out)
	}
}

// 今日 0 用量是可信的 0（有快照），必须与“无快照”区分：used=0、remaining=限额。
func TestUsageRemainingZeroUsageIsKnown(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	pl.SetDailyLimits(100, 50)
	rec := usage.New("")
	// 先建立快照，但今日没有任何 token：DailyTokenUsageAt 仍返回 ok=true。
	rec.Add(time.Now().Add(-48*time.Hour), "cn", "u1", "glm-5.3", usage.Delta{TotalTokens: 1, HasTotal: true}, true)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl, Usage: rec})
	_, out, body := remainingPanelGet(t, p, "/panel/api/usage/remaining")
	if out["data_available"] != true {
		t.Fatalf("expected available snapshot: %s", body)
	}
	a := out["accounts"].([]any)[0].(map[string]any)
	if a["used"].(float64) != 0 || a["remaining"].(float64) != 100 || a["limited"] != false {
		t.Fatalf("known zero usage must be 0/100: %+v", a)
	}
	if models := a["models"].([]any); len(models) != 0 {
		t.Fatalf("models must only contain today's activity: %+v", models)
	}
}

// remaining/connect 的读路径不得再调用完整 Pool.List：List 会为每个账号构建
// Status（限流台账、冷却剩余秒、成本行），470 账号时是 panel 热路径的主要放大。
// 这里做静态回归闸，避免以后顺手改回 List。
func TestPanelSnapshotsDoNotUseFullPoolList(t *testing.T) {
	for _, file := range []string{"remaining.go", "connect.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), ".Pool.List(") || strings.Contains(string(raw), "Pool.List()") {
			t.Fatalf("%s must use lightweight pool snapshots, not Pool.List", file)
		}
	}
}

// 接入模型列表按 30s TTL 缓存：新观测在 TTL 内不触发重建，过期后立即可见。
func TestConnectModelsCacheTTL(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	pl.NoteModelCost("u1", "glm-5.3", 0, 1000)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl})
	t.Cleanup(func() { forgetConnectModelCache(p) })

	first, source := p.connectModels()
	if source != "observed" || !hasConnectModel(first, "cn:glm-5.3") {
		t.Fatalf("first = %+v source=%q", first, source)
	}

	pl.NoteModelCost("u1", "kimi-k3", 0, 1000)
	cached, _ := p.connectModels()
	if hasConnectModel(cached, "cn:kimi-k3") {
		t.Fatalf("fresh observation must be hidden by TTL cache: %+v", cached)
	}

	c := connectModelCacheFor(p)
	c.mu.Lock()
	c.at = time.Now().Add(-connectModelsCacheTTL - time.Second)
	c.mu.Unlock()
	refreshed, _ := p.connectModels()
	if !hasConnectModel(refreshed, "cn:kimi-k3") {
		t.Fatalf("expired cache must rebuild with new model: %+v", refreshed)
	}
}

// 并发请求共用同一个缓存槽：首个构建完成后的等待者必须读缓存，而不是各建一份。
// 这里验证返回内容一致且缓存只保留最新一次构建结果。
func TestConnectModelsCacheSingleFlightAppearance(t *testing.T) {
	pl := pool.New("")
	for i := 0; i < 50; i++ {
		pl.Add(&auth.Auth{UID: "u" + string(rune('a'+i))})
	}
	pl.NoteModelCost("ua", "glm-5.3", 0, 1000)
	p := New(Config{Version: "test", APIKey: "admin-super-secret", Pool: pl})
	t.Cleanup(func() { forgetConnectModelCache(p) })

	const workers = 16
	var wg sync.WaitGroup
	results := make([][]connectModel, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], _ = p.connectModels()
		}(i)
	}
	wg.Wait()
	for i := 1; i < workers; i++ {
		if !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("worker %d got %+v, want %+v", i, results[i], results[0])
		}
	}
	if c := connectModelCacheFor(p); c.at.IsZero() || !hasConnectModel(c.models, "cn:glm-5.3") {
		t.Fatalf("cache not populated: %+v", c)
	}
}

// 缓存按面板实例隔离且有固定上限，测试/嵌入创建大量 Panel 时不能无限增长。
func TestConnectModelCacheBounded(t *testing.T) {
	clearConnectModelCaches()
	t.Cleanup(clearConnectModelCaches)
	// 先占满容量，再多建一个，最老的槽必须被淘汰。
	for i := 0; i < connectModelCacheMax; i++ {
		p := New(Config{Version: "test"})
		connectModelCacheFor(p)
	}
	old := New(Config{Version: "test"})
	connectModelCacheFor(old)
	newest := New(Config{Version: "test"})
	connectModelCacheFor(newest)

	connectModelCaches.Lock()
	defer connectModelCaches.Unlock()
	if len(connectModelCaches.byPanel) > connectModelCacheMax || len(connectModelCaches.order) > connectModelCacheMax {
		t.Fatalf("cache unbounded: map=%d order=%d", len(connectModelCaches.byPanel), len(connectModelCaches.order))
	}
	if _, ok := connectModelCaches.byPanel[newest]; !ok {
		t.Fatal("newest cache entry must be retained")
	}
}

// 缓存构建与并发读共享同一把槽锁；race 下不得出现 map/切片竞争。
func TestConnectModelsCacheRace(t *testing.T) {
	pl := pool.New("")
	for i := 0; i < 100; i++ {
		pl.Add(&auth.Auth{UID: "r" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	pl.NoteModelCost("raa", "glm-5.3", 0, 1000)
	p := New(Config{Version: "test", Pool: pl})
	t.Cleanup(func() { forgetConnectModelCache(p) })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = p.buildConnectModels(time.Now())
			_, _ = p.connectModels()
		}()
	}
	wg.Wait()
}

// forgetConnectModelCache 清理面板实例缓存，避免全局指针表在测试间残留。
func forgetConnectModelCache(p *Panel) {
	connectModelCaches.Lock()
	defer connectModelCaches.Unlock()
	delete(connectModelCaches.byPanel, p)
	for i, entry := range connectModelCaches.order {
		if entry == p {
			connectModelCaches.order = append(connectModelCaches.order[:i], connectModelCaches.order[i+1:]...)
			return
		}
	}
}

// clearConnectModelCaches 重置全局缓存表。
func clearConnectModelCaches() {
	connectModelCaches.Lock()
	defer connectModelCaches.Unlock()
	connectModelCaches.byPanel = map[*Panel]*connectModelCache{}
	connectModelCaches.order = nil
}

func hasConnectModel(models []connectModel, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// 业务 Key 的 /status 只能看聚合计数：完整账号池（UID/昵称/余额/冷却）属于
// 管理面。管理密钥（顶层 api_key）仍拿到 accounts 详情，保持运维兼容。
func TestStatusHidesAccountPoolFromClientKeys(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "admin-secret", Keys: []httpauth.Key{
		{ID: "legacy", Key: "admin-secret", Enabled: true},
		{ID: "client", Key: "client-secret", Enabled: true},
	}})
	p := testPoolWith(&auth.Auth{UID: "13800138000", Nickname: "秋叶", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Live: live})

	get := func(tok string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/status", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status code=%d body=%s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	admin := get("admin-secret")
	if _, ok := admin["accounts"].([]any); !ok {
		t.Fatalf("admin status must include account details: %+v", admin)
	}

	client := get("client-secret")
	if _, ok := client["accounts"]; ok {
		t.Fatalf("client status must not include account pool: %s", client)
	}
	if client["accounts_visible"] != false || client["total"] == nil {
		t.Fatalf("client status must expose aggregate counts only: %+v", client)
	}
	// 防止字段改名后误把完整账号表塞回其它键。
	raw, _ := json.Marshal(client)
	for _, leak := range []string{"13800138000", "秋叶", "access_token"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("client status leaked %q: %s", leak, raw)
		}
	}
}

// legacy 短 key 与 api_keys 同时存在时都必须可认证；热更新后旧 key 立即失效、
// 新 key 立即生效（无需重启）。
func TestLegacyAndMultiKeyHotReload(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	live := livecfg.New(livecfg.Snapshot{Keys: []httpauth.Key{
		{ID: "legacy", Key: "k", Enabled: true},
		{ID: "client", Key: "client-secret", Enabled: true},
	}})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, Live: live,
	})
	status := func(tok string) int {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := status("k"); got != http.StatusOK {
		t.Fatalf("legacy short key code=%d", got)
	}
	if got := status("client-secret"); got != http.StatusOK {
		t.Fatalf("api key code=%d", got)
	}
	if got := status("wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong key code=%d", got)
	}

	live.Store(livecfg.Snapshot{Keys: []httpauth.Key{{ID: "client", Key: "client-secret", Enabled: true}}})
	if got := status("k"); got != http.StatusUnauthorized {
		t.Fatalf("removed legacy key still works after hot reload: %d", got)
	}
	if got := status("client-secret"); got != http.StatusOK {
		t.Fatalf("existing key broken after hot reload: %d", got)
	}
	live.Store(livecfg.Snapshot{Keys: []httpauth.Key{{ID: "new", Key: "new-secret", Enabled: true}}})
	if got := status("new-secret"); got != http.StatusOK {
		t.Fatalf("new key not effective after hot reload: %d", got)
	}
}

// 并发认证与热更新同时发生时不能 panic/数据竞争；每个请求都应得到完整身份。
func TestMultiKeyAuthConcurrentWithReload(t *testing.T) {
	keysA := []httpauth.Key{{ID: "a", Key: "key-a", Enabled: true}}
	keysB := []httpauth.Key{{ID: "b", Key: "key-b", Enabled: true}}
	live := livecfg.New(livecfg.Snapshot{Keys: keysA})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%8 == 0 {
				if i%16 == 0 {
					live.Store(livecfg.Snapshot{Keys: keysB})
				} else {
					live.Store(livecfg.Snapshot{Keys: keysA})
				}
			}
			for j := 0; j < 50; j++ {
				snap := live.Load()
				tok := "key-a"
				want := "a"
				if len(snap.Keys) > 0 && snap.Keys[0].ID == "b" {
					tok, want = "key-b", "b"
				}
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("Authorization", "Bearer "+tok)
				id, ok := httpauth.VerifyAny(r, snap.Keys)
				if !ok || id.ID != want {
					t.Errorf("i=%d j=%d id=%+v ok=%v want=%s", i, j, id, ok, want)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

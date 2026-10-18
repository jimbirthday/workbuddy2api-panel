package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// 面板与网关共用 keyring：命中任一 enabled key 即可登录，禁用条目不可认证。
func TestPanelMultiKeyAuth(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "legacy-admin", Keys: []httpauth.Key{
		{ID: "legacy", Key: "legacy-admin", Enabled: true},
		{ID: "a", Key: "key-a", Enabled: true},
		{ID: "off", Key: "key-off", Enabled: false},
	}})
	p := New(Config{Version: "test", Live: live})
	for _, c := range []struct {
		token string
		want  int
	}{
		// 面板只接受管理密钥（legacy api_key）。客户端业务 key 不得获得
		// 账号管理/配置读写权限。
		{"legacy-admin", http.StatusOK},
		{"key-a", http.StatusUnauthorized},
		{"key-off", http.StatusUnauthorized},
		{"wrong", http.StatusUnauthorized},
		{"", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest("GET", "/panel/api/config", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		// 未装配 LoadConfig 时命中鉴权后应回 501，而不是 401；用它区分认证是否通过。
		p.ServeHTTP(rec, req)
		got := rec.Code
		if c.want == http.StatusOK && got != http.StatusNotImplemented {
			t.Errorf("token=%q code=%d want 501 after auth", c.token, got)
		}
		if c.want == http.StatusUnauthorized && got != http.StatusUnauthorized {
			t.Errorf("token=%q code=%d want 401", c.token, got)
		}
	}
}

// 管理面板 fail-closed：未配置顶层管理 key 时不得匿名开放（客户端 api_keys[]
// 存在也不算“管理鉴权已就绪”）；配置了管理 key 但业务 key 被禁用时仍只认管理 key。
func TestPanelAuthRequiredAndAllDisabled(t *testing.T) {
	open := New(Config{Version: "test"})
	if open.authRequired() {
		t.Fatal("empty keyring should not require auth")
	}
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("panel without admin key must fail closed, code=%d", rec.Code)
	}

	openClient := New(Config{Version: "test", Live: livecfg.New(livecfg.Snapshot{
		Keys: []httpauth.Key{{ID: "client-a", Key: "client-secret", Enabled: true}},
	})})
	if openClient.authRequired() {
		t.Fatal("client key alone must not mark panel auth as configured")
	}
	rec = httptest.NewRecorder()
	openClient.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("client-only keys must not open panel, code=%d", rec.Code)
	}

	allOff := New(Config{Version: "test", Live: livecfg.New(livecfg.Snapshot{
		APIKey: "admin-secret",
		Keys:   []httpauth.Key{{ID: "off", Key: "secret", Enabled: false}},
	})})
	if !allOff.authRequired() {
		t.Fatal("legacy admin key should still require auth")
	}
	// 业务 key 即使 enabled 也不能登录面板。
	req := httptest.NewRequest("GET", "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	allOff.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("business key must not access panel, code=%d", rec.Code)
	}
	req = httptest.NewRequest("GET", "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	allOff.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("admin key should pass auth (501 without LoadConfig), code=%d", rec.Code)
	}
}

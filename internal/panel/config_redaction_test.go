package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

func TestGetConfigRedactsSecrets(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "admin-secret",
		LoadConfig: func() (any, error) {
			return map[string]any{
				"api_key": "admin-secret",
				"api_keys": []any{
					map[string]any{"id": "client", "key": "client-secret", "realm": "cn"},
				},
			}, nil
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "admin-secret") || strings.Contains(body, "client-secret") {
		t.Fatalf("config response leaked a secret: %s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q want no-store", rec.Header().Get("Cache-Control"))
	}
	var out struct {
		Config struct {
			APIKey    string `json:"api_key"`
			APIKeySet bool   `json:"api_key_set"`
			APIKeys   []struct {
				Key    string `json:"key"`
				KeySet bool   `json:"key_set"`
			} `json:"api_keys"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Config.APIKey != "***" || !out.Config.APIKeySet {
		t.Fatalf("legacy key redaction wrong: %+v", out.Config)
	}
	if len(out.Config.APIKeys) != 1 || out.Config.APIKeys[0].Key != "***" || !out.Config.APIKeys[0].KeySet {
		t.Fatalf("api_keys redaction wrong: %+v", out.Config.APIKeys)
	}
}

func TestPanelRejectsClientKey(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "admin-secret",
	})
	p.cfg.Live = livecfg.New(livecfg.Snapshot{
		APIKey: "admin-secret",
		Keys: []httpauth.Key{
			{ID: "legacy", Key: "admin-secret", Enabled: true},
			{ID: "client", Key: "client-secret", Enabled: true},
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer client-secret")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("client key must not enter panel, code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// Upstash token 与 URL userinfo 属于凭据，配置页回显必须一并脱敏；
// 前端表单不包含 upstash，保存不会把掩码写回。
func TestGetConfigRedactsUpstashCredentials(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "admin-secret",
		LoadConfig: func() (any, error) {
			return map[string]any{
				"upstash": map[string]any{
					"url":   "rediss://default:redis-pass@example.upstash.io:6379",
					"token": "redis-token",
				},
			}, nil
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"redis-token", "redis-pass"} {
		if strings.Contains(body, secret) {
			t.Fatalf("config response leaked upstash secret %q: %s", secret, body)
		}
	}
	var out struct {
		Config struct {
			Upstash struct {
				Token     string `json:"token"`
				TokenSet  bool   `json:"token_set"`
				URL       string `json:"url"`
				URLRedact bool   `json:"url_redacted"`
			} `json:"upstash"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Config.Upstash.Token != "***" || !out.Config.Upstash.TokenSet {
		t.Fatalf("upstash token redaction wrong: %+v", out.Config.Upstash)
	}
	if !out.Config.Upstash.URLRedact || strings.Contains(out.Config.Upstash.URL, "default") {
		t.Fatalf("upstash url userinfo not stripped: %+v", out.Config.Upstash)
	}
}

// device_token 是真实设备凭证：GET 必须脱敏；把 GET 结果原样 POST 回去
// 也必须保留原值，不能把哨兵写进配置文件。
func TestGetConfigRedactsDeviceToken(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "admin-secret",
		LoadConfig: func() (any, error) {
			return map[string]any{
				"upstream": map[string]any{"device_token": "device-secret"},
			}, nil
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "device-secret") {
		t.Fatalf("device token leaked: %s", rec.Body.String())
	}
	var out struct {
		Config struct {
			Upstream struct {
				DeviceToken    string `json:"device_token"`
				DeviceTokenSet bool   `json:"device_token_set"`
			} `json:"upstream"`
		} `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Config.Upstream.DeviceToken != "***" || !out.Config.Upstream.DeviceTokenSet {
		t.Fatalf("device token redaction wrong: %+v", out.Config.Upstream)
	}
}

func TestSaveConfigPreservesMaskedSecrets(t *testing.T) {
	current := map[string]any{
		"api_key": "admin-secret",
		"api_keys": []any{
			map[string]any{"id": "client", "key": "client-secret"},
		},
		"upstream": map[string]any{"device_token": "device-secret"},
		"upstash": map[string]any{
			"url":   "rediss://default:redis-pass@example.upstash.io:6379",
			"token": "redis-token",
		},
	}
	var saved []byte
	p := New(Config{
		Version: "test",
		APIKey:  "admin-secret",
		LoadConfig: func() (any, error) {
			return current, nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			saved = append([]byte(nil), raw...)
			return nil, nil
		},
	})
	body := `{
		"api_key":"***","api_key_set":true,
		"api_keys":[{"id":"client","key":"***","key_set":true}],
		"upstream":{"device_token":"***","device_token_set":true},
		"upstash":{"url":"rediss://example.upstash.io:6379","token":"***","token_set":true,"url_redacted":true}
	}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(saved) == 0 {
		t.Fatal("SaveConfig not called")
	}
	var got map[string]any
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatal(err)
	}
	if got["api_key"] != "admin-secret" {
		t.Fatalf("api_key overwritten: %+v", got["api_key"])
	}
	if keys := got["api_keys"].([]any); len(keys) != 1 || keys[0].(map[string]any)["key"] != "client-secret" {
		t.Fatalf("api_keys overwritten: %#v", got["api_keys"])
	}
	up := got["upstream"].(map[string]any)
	if up["device_token"] != "device-secret" {
		t.Fatalf("device token overwritten: %#v", up)
	}
	if _, exists := up["device_token_set"]; exists {
		t.Fatalf("device_token_set should not be persisted: %#v", up)
	}
	ust := got["upstash"].(map[string]any)
	if ust["token"] != "redis-token" {
		t.Fatalf("upstash token overwritten: %#v", ust)
	}
	if !strings.Contains(ust["url"].(string), "redis-pass") {
		t.Fatalf("upstash url lost userinfo: %#v", ust)
	}
	if _, exists := ust["url_redacted"]; exists {
		t.Fatalf("url_redacted should not be persisted: %#v", ust)
	}
}

// 显式 device_token_set=false 必须能清空；新 token 必须原样保存。
func TestSaveConfigExplicitDeviceTokenChange(t *testing.T) {
	current := map[string]any{"upstream": map[string]any{"device_token": "old-device"}}
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"clear", `{"upstream":{"device_token":"***","device_token_set":false}}`, ""},
		{"replace", `{"upstream":{"device_token":"new-device","device_token_set":true}}`, "new-device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var saved map[string]any
			p := New(Config{
				Version:    "test",
				APIKey:     "admin-secret",
				LoadConfig: func() (any, error) { return current, nil },
				SaveConfig: func(raw []byte) ([]string, error) {
					if err := json.Unmarshal(raw, &saved); err != nil {
						return nil, err
					}
					return nil, nil
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/panel/api/config", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer admin-secret")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
			}
			got, _ := saved["upstream"].(map[string]any)["device_token"].(string)
			if got != tc.want {
				t.Fatalf("device token = %q want %q", got, tc.want)
			}
		})
	}
}

// api_key_set=false 必须显式清空 legacy key；不能因为 GET 返回 *** 就错误保留。
func TestSaveConfigExplicitLegacyKeyClear(t *testing.T) {
	var saved map[string]any
	p := New(Config{
		Version:    "test",
		APIKey:     "admin-secret",
		LoadConfig: func() (any, error) { return map[string]any{"api_key": "admin-secret"}, nil },
		SaveConfig: func(raw []byte) ([]string, error) {
			if err := json.Unmarshal(raw, &saved); err != nil {
				return nil, err
			}
			return nil, nil
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/config",
		strings.NewReader(`{"api_key":"***","api_key_set":false}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := saved["api_key"].(string); got != "" {
		t.Fatalf("api_key=%q want empty after explicit clear", got)
	}
	if _, exists := saved["api_key_set"]; exists {
		t.Fatalf("api_key_set should not be persisted: %#v", saved)
	}
}

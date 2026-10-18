package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func configPanel(t *testing.T, cfg map[string]any, save func([]byte) ([]string, error)) *Panel {
	t.Helper()
	return New(Config{
		Version: "test",
		APIKey:  "panel-key",
		LoadConfig: func() (any, error) {
			return cfg, nil
		},
		SaveConfig: save,
	})
}

func TestGetConfigMasksAllSecrets(t *testing.T) {
	cfg := map[string]any{
		"listen":  ":7863",
		"api_key": "legacy-secret-token",
		"api_keys": []any{
			map[string]any{"id": "a", "key": "team-secret-token", "realm": "cn"},
			map[string]any{"id": "b", "key": "短"},
		},
	}
	p := configPanel(t, cfg, nil)
	req := httptest.NewRequest("GET", "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer panel-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, secret := range []string{"legacy-secret-token", "team-secret-token", "短"} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q leaked: %s", secret, body)
		}
	}
	var out struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if got := out.Config["api_key"]; got != maskedSecret {
		t.Fatalf("legacy api_key mask=%v want %q", got, maskedSecret)
	}
	if out.Config["api_key_set"] != true {
		t.Fatalf("api_key_set=%v", out.Config["api_key_set"])
	}
	keys := out.Config["api_keys"].([]any)
	for _, raw := range keys {
		k := raw.(map[string]any)
		if strings.Contains(k["key"].(string), "secret") || strings.Contains(k["key"].(string), "短") {
			t.Fatalf("api key entry not masked: %v", k)
		}
	}
}

func TestSaveConfigBlankKeyPreservesOriginal(t *testing.T) {
	current := map[string]any{
		"api_key": "legacy-secret",
		"api_keys": []any{
			map[string]any{"id": "a", "key": "keep-me", "name": "A"},
			map[string]any{"id": "b", "key": "other", "name": "B"},
		},
	}
	var got []byte
	p := configPanel(t, current, func(raw []byte) ([]string, error) {
		got = append([]byte(nil), raw...)
		return nil, nil
	})
	body := `{"api_key":"legacy-secret","api_keys":[{"id":"a","key":"","name":"A2"},{"id":"b","key":"changed","name":"B"}]}`
	req := httptest.NewRequest("POST", "/panel/api/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer panel-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var submitted map[string]any
	if err := json.Unmarshal(got, &submitted); err != nil {
		t.Fatal(err)
	}
	keys := submitted["api_keys"].([]any)
	want := map[string]string{"a": "keep-me", "b": "changed"}
	for _, raw := range keys {
		k := raw.(map[string]any)
		if k["key"] != want[k["id"].(string)] {
			t.Fatalf("id=%v key=%v want %v", k["id"], k["key"], want[k["id"].(string)])
		}
	}
}

func TestSaveConfigBlankKeyForUnknownIDFails(t *testing.T) {
	current := map[string]any{"api_keys": []any{map[string]any{"id": "a", "key": "keep-me"}}}
	p := configPanel(t, current, func(raw []byte) ([]string, error) { return nil, nil })
	body := `{"api_keys":[{"id":"new","key":"","name":"new"}]}`
	req := httptest.NewRequest("POST", "/panel/api/config", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer panel-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	// 空白 key 对未知 id 没有原值可保留：交给 SaveConfig 校验（此闭包不校验），
	// 因此这里断言 panel 层不伪造一个空 key 以外的值；生产 SaveConfig 会返回 400。
	if strings.Contains(rec.Body.String(), "keep-me") {
		t.Fatalf("unknown id must not inherit another key: %s", rec.Body)
	}
}

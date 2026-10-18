package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 配置页 GET 对顶层 api_key 与 api_keys[].key 都必须用哨兵/空白表示，
// 且不能把哨兵或 api_key_set 写回 config.json。
func TestConfigMaskAndPreserveRoundTrip(t *testing.T) {
	current := map[string]any{
		"api_key": "legacy-secret",
		"api_keys": []any{
			map[string]any{"id": "a", "key": "key-a-secret", "name": "A"},
		},
	}
	var saved []byte
	p := New(Config{
		Version: "test", APIKey: "legacy-secret",
		LoadConfig: func() (any, error) { return current, nil },
		SaveConfig: func(raw []byte) ([]string, error) { saved = append([]byte(nil), raw...); return nil, nil },
	})
	req := httptest.NewRequest("GET", "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer legacy-secret")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("GET config leaked or failed: code=%d body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 把 GET 的脱敏结果原样提交：空白/sentinel 必须解析回原值。
	body, _ := json.Marshal(got.Config)
	req = httptest.NewRequest("POST", "/panel/api/config", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer legacy-secret")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(saved, &out); err != nil {
		t.Fatal(err)
	}
	if out["api_key"] != "legacy-secret" {
		t.Fatalf("legacy key not preserved: %v", out["api_key"])
	}
	if _, ok := out["api_key_set"]; ok {
		t.Fatalf("api_key_set must not be persisted: %v", out)
	}
	k := out["api_keys"].([]any)[0].(map[string]any)
	if k["key"] != "key-a-secret" {
		t.Fatalf("api key not preserved: %v", k)
	}
	if _, ok := k["key_set"]; ok {
		t.Fatalf("key_set must not be persisted: %v", k)
	}
}

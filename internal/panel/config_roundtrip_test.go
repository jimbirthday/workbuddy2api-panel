package panel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 保存时空白 key 表示保留原值；对 api_keys 必须按 id 逐条保留，而不是一律
// 用第一条/旧 key 覆盖。这里同时钉住“修改 name 但 key 留空”不会清掉密钥。
func TestSaveConfigPreservesEachKeyByID(t *testing.T) {
	current := map[string]any{
		"api_key": "legacy-secret",
		"api_keys": []any{
			map[string]any{"id": "a", "key": "key-a", "name": "A"},
			map[string]any{"id": "b", "key": "key-b", "name": "B"},
		},
	}
	var saved map[string]any
	p := New(Config{
		Version: "test", APIKey: "legacy-secret",
		LoadConfig: func() (any, error) { return current, nil },
		SaveConfig: func([]byte) ([]string, error) { return nil, nil },
	})
	// 直接调用 helper 断言写回内容，避免依赖 SaveConfig 闭包收到的原始 JSON。
	raw := []byte(`{"api_key":"***","api_keys":[{"id":"a","key":"","name":"A2"},{"id":"b","key":"***","name":"B2"}]}`)
	merged, err := preserveBlankAPIKeys(raw, p.cfg.LoadConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(merged, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["api_key"] != "legacy-secret" {
		t.Fatalf("legacy key=%v", saved["api_key"])
	}
	keys := saved["api_keys"].([]any)
	if keys[0].(map[string]any)["key"] != "key-a" || keys[1].(map[string]any)["key"] != "key-b" {
		t.Fatalf("per-id preserve failed: %v", keys)
	}
	if strings.Contains(string(merged), "api_key_set") || strings.Contains(string(merged), "key_set") {
		t.Fatalf("auxiliary flags must not be persisted: %s", merged)
	}
}

// GET 回显的 api_key=*** 原样提交必须保留原值；显式 api_key_set=false 才清空。
func TestSaveConfigExplicitClearLegacyKey(t *testing.T) {
	current := map[string]any{"api_key": "legacy-secret"}
	p := New(Config{
		Version: "test", APIKey: "legacy-secret",
		LoadConfig: func() (any, error) { return current, nil },
		SaveConfig: func([]byte) ([]string, error) { return nil, nil },
	})
	raw := []byte(`{"api_key":"***","api_key_set":false}`)
	merged, err := preserveBlankAPIKeys(raw, p.cfg.LoadConfig)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatal(err)
	}
	if out["api_key"] != "" {
		t.Fatalf("explicit clear failed: %v", out["api_key"])
	}
}

// 读取旧配置失败必须 fail-closed：此时无法判断提交的空串是“真的清空”还是
// GET 掩码哨兵，继续保存会把 api_key / api_keys 写成空。返回错误让上层 400，
// 磁盘与运行配置都保持不变。
func TestPreserveBlankSecretsFailsClosedWhenLoadFails(t *testing.T) {
	raw := []byte(`{"api_key":"***","api_keys":[{"id":"a","key":""}],"upstream":{"device_token":"***"},"upstash":{"token":"***","url":"***"}}`)
	_, err := preserveBlankAPIKeys(raw, func() (any, error) {
		return nil, errors.New("config file unreadable")
	})
	if err == nil {
		t.Fatal("expected save to be rejected when current config cannot be reloaded")
	}
	if !strings.Contains(err.Error(), "reload current config failed") {
		t.Fatalf("error should explain fail-closed reason: %v", err)
	}
}

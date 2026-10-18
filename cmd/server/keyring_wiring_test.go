package main

import "testing"

func TestConfigKeyringIncludesLegacyAndAPIKeys(t *testing.T) {
	c, err := ParseConfig([]byte(`{
		"api_key":"k",
		"api_keys":[
			{"id":"client","key":"client-secret","realm":"cn","models":["deepseek/*"],"enabled":true},
			{"id":"off","key":"off-secret","enabled":false}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	keys := c.keyring()
	if len(keys) != 3 {
		t.Fatalf("keyring len=%d want 3: %+v", len(keys), keys)
	}
	if keys[0].ID != "legacy" || keys[0].Key != "k" || !keys[0].Enabled {
		t.Fatalf("legacy key must stay compatible: %+v", keys[0])
	}
	if keys[1].ID != "client" || keys[1].Realm != "cn" || !keys[1].Enabled {
		t.Fatalf("client key not wired: %+v", keys[1])
	}
	if keys[2].Enabled {
		t.Fatalf("explicit false must disable: %+v", keys[2])
	}
}

func TestConfigKeyringEnabledDefaultsTrue(t *testing.T) {
	c, err := ParseConfig([]byte(`{"api_keys":[{"id":"client","key":"client-secret"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	keys := c.keyring()
	if len(keys) != 1 || !keys[0].Enabled {
		t.Fatalf("enabled must default true: %+v", keys)
	}
}

package main

import "testing"

func TestDailyTokenLimitConfigDefaultsAndClamp(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize defaults: %v", err)
	}
	if c.Pool.DailyTokenLimit != 0 || c.Pool.ModelDailyTokenLimit != 0 {
		t.Fatalf("daily limits default = %d/%d, want 0/0", c.Pool.DailyTokenLimit, c.Pool.ModelDailyTokenLimit)
	}

	configured, err := ParseConfig([]byte(`{"pool":{"daily_token_limit":200000,"model_daily_token_limit":50000}}`))
	if err != nil {
		t.Fatalf("parse configured limits: %v", err)
	}
	if configured.Pool.DailyTokenLimit != 200000 || configured.Pool.ModelDailyTokenLimit != 50000 {
		t.Fatalf("configured limits = %d/%d", configured.Pool.DailyTokenLimit, configured.Pool.ModelDailyTokenLimit)
	}

	clamped, err := ParseConfig([]byte(`{"pool":{"daily_token_limit":-1,"model_daily_token_limit":-2}}`))
	if err != nil {
		t.Fatalf("parse negative limits: %v", err)
	}
	if clamped.Pool.DailyTokenLimit != 0 || clamped.Pool.ModelDailyTokenLimit != 0 {
		t.Fatalf("negative limits must clamp to 0, got %d/%d", clamped.Pool.DailyTokenLimit, clamped.Pool.ModelDailyTokenLimit)
	}
}

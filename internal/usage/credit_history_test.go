package usage

import (
	"strings"
	"testing"
	"time"
)

func creditHistoryNow() time.Time {
	return time.Now().Truncate(time.Hour).Add(-3 * time.Hour)
}

// 历史记录按时间倒序，并明确标注小时/日聚合粒度；每行保留该桶的请求数。
func TestCreditHistoryAggregatedSpend(t *testing.T) {
	r := New("")
	base := creditHistoryNow()
	r.Add(base, "cn", "uid-123456789", "cn:glm-5.3", Delta{
		Credit: 1.25, HasCredit: true, TotalTokens: 100, HasTotal: true,
	}, true)
	r.Add(base, "cn", "uid-123456789", "cn:glm-5.3", Delta{
		Credit: 0.75, HasCredit: true, TotalTokens: 50, HasTotal: true,
	}, true)
	r.Add(base.Add(2*time.Hour), "global", "uid-abcdefgh", "kimi-k3", Delta{
		Credit: 2, HasCredit: true, TotalTokens: 200, HasTotal: true,
	}, true)

	res := r.CreditHistory(CreditHistoryQuery{Window: Window{Hours: 0}, Limit: 10})
	if res.Total != 2 || len(res.Records) != 2 {
		t.Fatalf("records=%d total=%d, want 2", len(res.Records), res.Total)
	}
	first := res.Records[0]
	if first.Kind != CreditKindSpend || first.Credit != 2 || first.Model != "kimi-k3" ||
		first.Scope != "hour" || first.Granularity != "hourly" || first.Requests != 1 {
		t.Fatalf("first record = %+v", first)
	}
	second := res.Records[1]
	if second.Kind != CreditKindSpend || second.Credit != 2 || second.Requests != 2 ||
		second.Tokens != 150 || second.Granularity != "hourly" {
		t.Fatalf("aggregated record = %+v, want one hourly row with 2 requests", second)
	}
	if res.Credits != 4 || res.Samples != 2 || res.Tokens != 350 || !res.Aggregated || res.GrantAvailable {
		t.Fatalf("summary = %+v", res)
	}
}

// 负向 credit 只说明出现了冲正/返还观测，不能据此还原领取流水：不输出 grant 行。
func TestCreditHistoryDoesNotFabricateGrant(t *testing.T) {
	r := New("")
	base := creditHistoryNow()
	r.Add(base, "cn", "u", "m", Delta{Credit: -30, HasCredit: true, TotalTokens: 10, HasTotal: true}, true)
	r.Add(base.Add(time.Hour), "cn", "u", "m", Delta{Credit: 1, HasCredit: true, TotalTokens: 10, HasTotal: true}, true)

	res := r.CreditHistory(CreditHistoryQuery{Window: Window{Hours: 0}, Kind: CreditKindAll})
	if res.Total != 1 || len(res.Records) != 1 || res.Records[0].Credit != 1 {
		t.Fatalf("negative credit must not become grant: %+v", res)
	}
	grant := r.CreditHistory(CreditHistoryQuery{Window: Window{Hours: 0}, Kind: CreditKindGrant})
	if grant.Total != 0 || len(grant.Records) != 0 || grant.GrantAvailable {
		t.Fatalf("grant kind must be empty and flagged unavailable: %+v", grant)
	}
	for _, rec := range res.Records {
		if rec.Kind != CreditKindSpend {
			t.Fatalf("all rows must be spend: %+v", rec)
		}
	}
}

// kind 过滤与窗口、limit 保持一致；限制条数要回显 truncated。
func TestCreditHistoryKindWindowLimit(t *testing.T) {
	r := New("")
	base := creditHistoryNow()
	for i := 0; i < 5; i++ {
		r.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u", "m", Delta{
			Credit: float64(i + 1), HasCredit: true, TotalTokens: int64(10 * (i + 1)), HasTotal: true,
		}, true)
	}

	spend := r.CreditHistory(CreditHistoryQuery{Window: Window{Hours: 0}, Kind: CreditKindSpend, Limit: 2})
	if spend.Total != 5 || len(spend.Records) != 2 || !spend.Truncated {
		t.Fatalf("spend limit result = total=%d records=%d truncated=%v", spend.Total, len(spend.Records), spend.Truncated)
	}
	if spend.Records[0].Credit != 5 || spend.Records[1].Credit != 4 {
		t.Fatalf("limit should keep newest rows: %+v", spend.Records)
	}
	if spend.Credits != 15 || spend.Samples != 5 {
		t.Fatalf("summary must cover all matched rows before limit: %+v", spend)
	}

	from := base.Add(3 * time.Hour)
	to := base.Add(4 * time.Hour)
	windowed := r.CreditHistory(CreditHistoryQuery{Window: Window{From: from, To: to}, Limit: 10})
	if windowed.Total != 2 || windowed.WindowFrom == "" || windowed.WindowTo == "" {
		t.Fatalf("window = %+v", windowed)
	}
	if windowed.Records[0].Credit != 5 || windowed.Records[1].Credit != 4 {
		t.Fatalf("window rows = %+v", windowed.Records)
	}
}

// 折叠后的日桶必须显示 daily 粒度，不能伪装成小时或逐请求记录。
func TestCreditHistoryDailyGranularity(t *testing.T) {
	r := New("")
	old := time.Now().Add(-120 * 24 * time.Hour).Truncate(time.Hour)
	r.Add(old, "cn", "u", "m", Delta{Credit: 3, HasCredit: true, TotalTokens: 30, HasTotal: true}, true)
	r.Rollup(time.Now())
	res := r.CreditHistory(CreditHistoryQuery{Window: Window{Hours: 0}, Limit: 10})
	if len(res.Records) != 1 {
		t.Fatalf("records = %+v", res.Records)
	}
	if got := res.Records[0]; got.Scope != "day" || got.Granularity != "daily" || got.Credit != 3 {
		t.Fatalf("daily rollup record = %+v", got)
	}
}

// 没有积分观测的旧桶不产生 0 积分记录；限额默认与上限也要生效。
func TestCreditHistorySkipsLegacyAndCapsLimit(t *testing.T) {
	r := New("")
	base := creditHistoryNow()
	r.Add(base, "cn", "u", "m", Delta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 10, HasTotal: true}, true)
	r.Add(base.Add(time.Hour), "cn", "u", "m", Delta{Credit: 1, HasCredit: true, TotalTokens: 10, HasTotal: true}, true)

	if res := r.CreditHistory(CreditHistoryQuery{}); res.Total != 1 || len(res.Records) != 1 {
		t.Fatalf("legacy zero-credit bucket must be skipped: %+v", res)
	}
	if res := r.CreditHistory(CreditHistoryQuery{Limit: CreditHistoryMaxLimit + 1000}); res.Limit != CreditHistoryMaxLimit {
		t.Fatalf("limit cap = %d, want %d", res.Limit, CreditHistoryMaxLimit)
	}
	if res := r.CreditHistory(CreditHistoryQuery{}); res.Limit != CreditHistoryDefaultLimit {
		t.Fatalf("default limit = %d, want %d", res.Limit, CreditHistoryDefaultLimit)
	}
}

// 账号标签只保留 uid8；空 recorder 返回稳定空数组。
func TestCreditHistoryJSONShape(t *testing.T) {
	r := New("")
	r.Add(creditHistoryNow(), "cn", "secret-full-uid", "m", Delta{Credit: 1, HasCredit: true, TotalTokens: 1, HasTotal: true}, true)
	res := r.CreditHistory(CreditHistoryQuery{})
	if len(res.Records) != 1 {
		t.Fatalf("records = %+v", res.Records)
	}
	if strings.Contains(res.Records[0].Account, "secret-full-uid") || res.Records[0].Account != "secret-f" {
		t.Fatalf("account label must be uid8, got %q", res.Records[0].Account)
	}
	var nilRec *Recorder
	if got := nilRec.CreditHistory(CreditHistoryQuery{}); got.Records == nil || len(got.Records) != 0 {
		t.Fatalf("nil recorder should return empty array: %+v", got)
	}
}

// 历史读取与并发记账/折叠共享分片桶；读侧只持分片读锁，不能出现 data race。
func TestCreditHistoryConcurrentRead(t *testing.T) {
	r := New("")
	base := creditHistoryNow()
	r.Add(base, "cn", "u", "m", Delta{Credit: 1, HasCredit: true, TotalTokens: 10, HasTotal: true}, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.Add(base.Add(time.Duration(i)*time.Minute), "cn", "u", "m", Delta{
				Credit: 0.1, HasCredit: true, TotalTokens: 1, HasTotal: true,
			}, true)
			r.Rollup(base.Add(100 * 24 * time.Hour))
		}
	}()
	for i := 0; i < 50; i++ {
		_ = r.CreditHistory(CreditHistoryQuery{Window: Window{Hours: 0}, Limit: 100})
	}
	<-done
}

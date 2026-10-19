package auth

import (
	"encoding/base64"
	"testing"
)

func jwtToken(t *testing.T, payload string) string {
	t.Helper()
	seg := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return "eyJhbGciOiJub25lIn0." + seg + ".sig"
}

// TestIsEnterprise 企业版判定：enterpriseId 非空即企业版（纯空白视为非企业）。
// 该判定驱动 scheduler 五类任务与 panel 成长任务/签到入口的门控——企业版在这些
// 端点上被上游一律拒绝（400 code 10001 / 403 growth-only），误判为个人号会导致
// 每日对注定失败的端点空发请求。
func TestIsEnterprise(t *testing.T) {
	cases := []struct {
		name string
		ent  string
		want bool
	}{
		{"个人号（空）", "", false},
		{"纯空白", "   ", false},
		{"企业号", "fn7xvyc51dkw", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", EnterpriseID: c.ent}
			if got := a.IsEnterprise(); got != c.want {
				t.Errorf("EnterpriseID=%q IsEnterprise()=%v want %v", c.ent, got, c.want)
			}
		})
	}
}

// TestIsEnterpriseAndIsGlobalIndependent 企业标识与 realm 是正交维度：
// 企业号恒为 CN realm，但两者判定互不干扰（门控引用处并列书写）。
func TestIsEnterpriseAndIsGlobalIndependent(t *testing.T) {
	a := &Auth{UID: "u1", EnterpriseID: "ent-1"}
	if !a.IsEnterprise() {
		t.Error("应判为企业版")
	}
	if a.IsGlobal() {
		t.Error("企业号不应被判为 global")
	}
}

// 企业号凭据文件的 enterpriseId 经常为空，真实标识只在 accessToken 的 JWT
// claim 里。Parse 必须回填，否则调度器/任务扫描/额度端点都会把企业号当个人号。
func TestParseEnterpriseIDFromJWTClaim(t *testing.T) {
	tok := jwtToken(t, `{"sub":"u-ent","enterprise_id":"ent-jwt"}`)
	raw := []byte(`{"auth":{"accessToken":"` + tok + `","refreshToken":"rt"},"account":{"uid":"u-ent","enterpriseId":""}}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.EnterpriseID != "ent-jwt" || !a.IsEnterprise() {
		t.Fatalf("enterprise id=%q isEnterprise=%v", a.EnterpriseID, a.IsEnterprise())
	}
	if a.EnterpriseIDValue() != "ent-jwt" {
		t.Fatalf("EnterpriseIDValue=%q", a.EnterpriseIDValue())
	}
}

func TestParseEnterpriseIDCamelCaseJWTClaim(t *testing.T) {
	tok := jwtToken(t, `{"enterpriseId":"ent-camel"}`)
	a, err := Parse([]byte(`{"accessToken":"` + tok + `","uid":"u1"}`))
	if err != nil || a.EnterpriseID != "ent-camel" {
		t.Fatalf("camel claim: %+v err=%v", a, err)
	}
}

func TestMalformedJWTDoesNotMarkEnterprise(t *testing.T) {
	a, err := Parse([]byte(`{"accessToken":"not-a-jwt","uid":"u1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.IsEnterprise() || a.EnterpriseIDValue() != "" {
		t.Fatalf("malformed token must not produce enterprise id: %q", a.EnterpriseID)
	}
}

// 显式 enterpriseId 与 token claim 冲突时，claim 才是当前身份：旧导出/手工文件
// 可能留着上一个租户 ID，若它压过新 token 会把聊天与额度送到错误企业。
func TestParseClaimOverridesStaleExplicitEnterpriseID(t *testing.T) {
	tok := jwtToken(t, `{"enterprise_id":"ent-new"}`)
	a, err := Parse([]byte(`{"auth":{"accessToken":"` + tok + `","refreshToken":"rt"},"account":{"uid":"u1","enterpriseId":"ent-old"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.EnterpriseIDValue(); got != "ent-new" {
		t.Fatalf("stale explicit must not override claim: got=%q", got)
	}
}

// 旧版凭据文件可能只有 enterpriseId、token 不带企业 claim；首次加载必须保留显式值。
func TestParseKeepsExplicitEnterpriseWhenTokenHasNoClaim(t *testing.T) {
	tok := jwtToken(t, `{"sub":"u1"}`)
	a, err := Parse([]byte(`{"auth":{"accessToken":"` + tok + `","refreshToken":"rt"},"account":{"uid":"u1","enterpriseId":"ent-file"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.EnterpriseIDValue(); got != "ent-file" {
		t.Fatalf("explicit enterprise id lost for legacy token: got=%q", got)
	}
}

// 导入/登录直接构造 Auth（绕过 Parse）后也要按同一规则初始化：claim 覆盖旧值，
// 无 claim 保留 API 显式值。
func TestInitEnterpriseIdentityMatchesParseRules(t *testing.T) {
	a := &Auth{AccessToken: jwtToken(t, `{"enterprise_id":"ent-claim"}`), EnterpriseID: "ent-stale"}
	a.InitEnterpriseIdentity()
	if got := a.EnterpriseIDValue(); got != "ent-claim" {
		t.Fatalf("claim must override explicit: got=%q", got)
	}
	b := &Auth{AccessToken: jwtToken(t, `{"sub":"u2"}`), EnterpriseID: "ent-api"}
	b.InitEnterpriseIdentity()
	if got := b.EnterpriseIDValue(); got != "ent-api" {
		t.Fatalf("explicit api id must survive no-claim token: got=%q", got)
	}
}

// TestRefreshToPersonalClearsEnterprise 企业 token 刷新成个人 token 后，旧企业
// 身份必须失效；否则额度端点、X-Enterprise-Id 与任务门控会继续按旧租户路由。
func TestRefreshToPersonalClearsEnterprise(t *testing.T) {
	entTok := jwtToken(t, `{"enterprise_id":"ent-a"}`)
	a := &Auth{AccessToken: entTok, RefreshToken: "rt", UID: "u1"}
	if !a.IsEnterprise() || a.EnterpriseIDValue() != "ent-a" {
		t.Fatalf("initial enterprise id=%q", a.EnterpriseIDValue())
	}
	a.Lock()
	a.AccessToken = jwtToken(t, `{"sub":"u1"}`)
	a.Unlock()
	if a.IsEnterprise() || a.EnterpriseIDValue() != "" {
		t.Fatalf("stale enterprise id=%q after token switch", a.EnterpriseIDValue())
	}
}

// TestReparseEnterpriseIDAfterRefreshTokenSwap refresh 写回新 token 后，调用方在
// 同一把锁内重算身份。这条路径与 RefreshToken 的真实写回段一致：不能依赖下一次
// 懒解析才发现租户变化，否则紧随其后的额度查询可能已经打到旧企业端点。
func TestReparseEnterpriseIDAfterRefreshTokenSwap(t *testing.T) {
	a := &Auth{AccessToken: jwtToken(t, `{"enterprise_id":"ent-old"}`), RefreshToken: "rt", UID: "u1"}
	if a.EnterpriseIDValue() != "ent-old" {
		t.Fatalf("initial=%q", a.EnterpriseIDValue())
	}

	a.Lock()
	a.AccessToken = jwtToken(t, `{"enterprise_id":"ent-new"}`)
	a.ReparseEnterpriseID()
	a.Unlock()
	if got := a.EnterpriseIDValue(); got != "ent-new" {
		t.Fatalf("after refresh to another enterprise got=%q want ent-new", got)
	}

	a.Lock()
	a.AccessToken = jwtToken(t, `{"sub":"u1"}`)
	a.ReparseEnterpriseID()
	a.Unlock()
	if a.IsEnterprise() || a.EnterpriseIDValue() != "" {
		t.Fatalf("after refresh to personal stale=%q", a.EnterpriseIDValue())
	}
}

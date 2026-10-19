package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func creditJWT(t *testing.T, payload string) string {
	t.Helper()
	seg := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return "eyJhbGciOiJub25lIn0." + seg + ".sig"
}

// TestEnterpriseIDForClaimWins 文件里的 enterpriseId 可能是旧租户，JWT claim
// 才是当前身份。只读文件字段会把企业号打到个人端点或错误租户。
func TestEnterpriseIDForClaimWins(t *testing.T) {
	var af authFile
	af.Auth.AccessToken = creditJWT(t, `{"enterprise_id":"ent-new"}`)
	af.Account.EnterpriseID = "ent-old"
	if got := enterpriseIDFor(&af); got != "ent-new" {
		t.Fatalf("enterpriseIDFor=%q want ent-new", got)
	}

	af.Auth.AccessToken = creditJWT(t, `{"sub":"u1"}`)
	af.Account.EnterpriseID = "ent-file"
	if got := enterpriseIDFor(&af); got != "ent-file" {
		t.Fatalf("legacy explicit id lost: got=%q", got)
	}
	af.Account.EnterpriseID = ""
	if got := enterpriseIDFor(&af); got != "" {
		t.Fatalf("personal token with empty explicit id got=%q", got)
	}
}

// TestAuthFileSnakeCaseCredential 兼容 cockpit/扁平导入的 snake_case 字段。
// 该工具解析的是独立结构体，不会自动套用 internal/auth.Parse；漏读会让可用账号
// 误报 "no accessToken"，并把带 enterprise_id 的企业号按个人额度端点查询。
func TestAuthFileSnakeCaseCredential(t *testing.T) {
	raw := []byte(`{"auth":{"access_token":"plain-at"},"account":{"uid":"u1","enterprise_id":"ent-snake"}}`)
	var af authFile
	if err := json.Unmarshal(raw, &af); err != nil {
		t.Fatal(err)
	}
	if got := af.accessToken(); got != "plain-at" {
		t.Fatalf("accessToken=%q want plain-at", got)
	}
	if got := enterpriseIDFor(&af); got != "ent-snake" {
		t.Fatalf("enterpriseIDFor=%q want ent-snake", got)
	}

	camel := `{"auth":{"accessToken":"camel-at"},"account":{"enterpriseId":"ent-camel"}}`
	var af2 authFile
	if err := json.Unmarshal([]byte(camel), &af2); err != nil {
		t.Fatal(err)
	}
	if got := af2.accessToken(); got != "camel-at" {
		t.Fatalf("camel accessToken=%q", got)
	}
	if got := enterpriseIDFor(&af2); got != "ent-camel" {
		t.Fatalf("camel enterpriseIDFor=%q", got)
	}
}

// TestFetchEnterpriseResourceEndpointAndHeaders 企业号必须走
// get-enterprise-user-usage 且同时带双租户头；remain=limitNum-credit。
func TestFetchEnterpriseResourceEndpointAndHeaders(t *testing.T) {
	oldCN, oldGlobal := billingBaseCN, billingBaseGlobal
	t.Cleanup(func() { billingBaseCN, billingBaseGlobal = oldCN, oldGlobal })
	var gotPath, gotEnt, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEnt = r.Header.Get("X-Enterprise-Id")
		gotTenant = r.Header.Get("X-Tenant-Id")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"credit": 812.45, "limitNum": 2000},
		})
	}))
	defer srv.Close()
	billingBaseCN = srv.URL
	billingBaseGlobal = srv.URL

	var af authFile
	af.Auth.AccessToken = creditJWT(t, `{"enterprise_id":"ent-7"}`)
	af.Account.UID = "u1"
	af.Account.EnterpriseID = "ent-stale"
	remain, used, size, packs, err := fetchEnterpriseResource(&af)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/v2/billing/meter/get-enterprise-user-usage") {
		t.Errorf("path=%s", gotPath)
	}
	if gotEnt != "ent-7" || gotTenant != "ent-7" {
		t.Errorf("tenant headers=%q/%q want ent-7/ent-7", gotEnt, gotTenant)
	}
	if remain != 1188 || used != 812 || size != 2000 || packs != 1 {
		t.Errorf("resource remain/used/size/packs=%d/%d/%d/%d want 1188/812/2000/1", remain, used, size, packs)
	}
}

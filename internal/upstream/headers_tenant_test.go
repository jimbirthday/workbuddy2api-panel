package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersCarryTenantForEnterprise 企业聊天必须同时带 X-Enterprise-Id 与
// X-Tenant-Id；个人号不得出现任一企业头。只带一个头会让上游把聊天落到默认租户
// （与 billing 的双头口径不一致）。
func TestChatHeadersCarryTenantForEnterprise(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name       string
		a          *auth.Auth
		wantEnt    string
		wantTenant string
	}{
		{"enterprise", &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "ent-9"}, "ent-9", "ent-9"},
		{"personal", &auth.Auth{UID: "u2", AccessToken: "at"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://upstream/v2/chat/completions", nil)
			c.ChatHeaders(req, tc.a, "", ChatMeta{})
			if got := req.Header.Get("X-Enterprise-Id"); got != tc.wantEnt {
				t.Errorf("X-Enterprise-Id=%q want %q", got, tc.wantEnt)
			}
			if got := req.Header.Get("X-Tenant-Id"); got != tc.wantTenant {
				t.Errorf("X-Tenant-Id=%q want %q", got, tc.wantTenant)
			}
			if tc.wantEnt == "" && req.Header.Get("X-No-Enterprise-Id") != "1" {
				t.Errorf("personal chat must declare X-No-Enterprise-Id=1")
			}
		})
	}
}

// TestChatHeadersDoNotFabricateConversationID 客户端没给会话 ID 时不得发送
// X-Conversation-ID。上游按此头绑定对话/图片状态，伪造一个账号级固定值会把
// 所有无会话键的客户端并进同一个上游对话。
func TestChatHeadersDoNotFabricateConversationID(t *testing.T) {
	c := &Client{}
	req := httptest.NewRequest(http.MethodPost, "http://upstream/v2/chat/completions", nil)
	c.ChatHeaders(req, &auth.Auth{UID: "u1", AccessToken: "at"}, "", ChatMeta{})
	if got := req.Header.Get("X-Conversation-ID"); got != "" {
		t.Fatalf("X-Conversation-ID=%q want absent", got)
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got == "" {
		t.Fatal("X-Conversation-Request-ID must still be generated")
	}
}

// TestEnterpriseModelCatalogCarriesTenant 企业模型目录（/v3/config 与企业端点家族）
// 必须与聊天同一租户。缺 X-Enterprise-Id / X-Tenant-Id 时，上游会返回默认租户
// 目录，企业私有模型缺失，前端表现为「企业号没有模型」。
func TestEnterpriseModelCatalogCarriesTenant(t *testing.T) {
	a := &auth.Auth{UID: "u-ent", AccessToken: "at", EnterpriseID: "ent-7", Domain: "www.workbuddy.ai"}
	a.BackfillRealm()
	c := New()
	var v3Ent, v3Tenant, catEnt, catTenant string
	var mu sync.Mutex
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v3/config":
			mu.Lock()
			v3Ent, v3Tenant = r.Header.Get("X-Enterprise-Id"), r.Header.Get("X-Tenant-Id")
			mu.Unlock()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"ent-v3","name":"ent-v3"}]}}`))}, nil
		case "/v2/enterprises/personal/models":
			mu.Lock()
			catEnt, catTenant = r.Header.Get("X-Enterprise-Id"), r.Header.Get("X-Tenant-Id")
			mu.Unlock()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"ent-v2","name":"ent-v2"}]}}`))}, nil
		default:
			return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
	})}
	names, infos := c.fetchGlobalModelsOnce(a)
	mu.Lock()
	defer mu.Unlock()
	if v3Ent != "ent-7" || v3Tenant != "ent-7" {
		t.Fatalf("v3/config tenant headers = %q/%q, want ent-7/ent-7", v3Ent, v3Tenant)
	}
	if catEnt != "ent-7" || catTenant != "ent-7" {
		t.Fatalf("catalog tenant headers = %q/%q, want ent-7/ent-7", catEnt, catTenant)
	}
	if len(names) == 0 || len(infos) == 0 {
		t.Fatalf("enterprise catalog empty: names=%v infos=%v", names, infos)
	}
}

// TestCNEnterpriseModelCatalogCarriesTenant 覆盖 CN 企业号的个人模型目录
// （/console/enterprises/personal/models）。该端点与 global /v3/config 是两条
// 独立探测链：漏带租户头时上游会返回默认租户目录，企业私有模型同样缺失。
func TestCNEnterpriseModelCatalogCarriesTenant(t *testing.T) {
	a := &auth.Auth{UID: "u-cn-ent", AccessToken: "at", EnterpriseID: "ent-cn"}
	a.BackfillRealm()
	c := New()
	var gotEnt, gotTenant string
	c.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/console/enterprises/personal/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotEnt = r.Header.Get("X-Enterprise-Id")
		gotTenant = r.Header.Get("X-Tenant-Id")
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"ent-cn-model","name":"ent-cn-model"}],"agents":[{"name":"cli","models":["ent-cn-model"]}]}}`))}, nil
	})}
	infos, err := c.fetchEnterpriseModels(a)
	if err != nil {
		t.Fatalf("fetchEnterpriseModels: %v", err)
	}
	if gotEnt != "ent-cn" || gotTenant != "ent-cn" {
		t.Fatalf("CN enterprise catalog tenant headers = %q/%q, want ent-cn/ent-cn", gotEnt, gotTenant)
	}
	if len(infos) != 1 || infos[0].ID != "ent-cn-model" {
		t.Fatalf("CN enterprise catalog models = %+v", infos)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

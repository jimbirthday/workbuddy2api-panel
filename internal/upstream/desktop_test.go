package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	var got []map[string]any
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		if r.Header.Get("X-User-Id") != "u-dt" {
			t.Errorf("X-User-Id=%q want u-dt", r.Header.Get("X-User-Id"))
		}
		if r.Header.Get("X-Product") != "SaaS" {
			t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie"},
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}
	if gotUA != desktopUA {
		t.Errorf("UA=%q want %q", gotUA, desktopUA)
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev["extName"] != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", ev["extName"])
	}
	if ev["ideType"] != "WorkBuddy" || ev["ideName"] != "WorkBuddy" {
		t.Errorf("ideType/ideName=%v/%v want WorkBuddy", ev["ideType"], ev["ideName"])
	}
	if ev["userId"] != "u-dt" {
		t.Errorf("userId=%v want u-dt", ev["userId"])
	}
	if ev["userNickname"] != "RenJie" {
		t.Errorf("userNickname=%v want RenJie", ev["userNickname"])
	}
	if ev["mode"] != "craft" {
		t.Errorf("业务字段 mode=%v 被指纹覆盖或缺失", ev["mode"])
	}
	// machineId 应为 uid 稳定派生（36 hex）。
	if len(ev["machineId"].(string)) != 36 {
		t.Errorf("machineId 长度=%d want 36", len(ev["machineId"].(string)))
	}
}

// TestReportWebAndMPEventsCarryTenantHeaders 企业身份必须同时出现在事件 body 与
// 出站头里：body 供上游业务归属，头供网关按租户鉴权。缺失头会在未来复用这些
// 上报路径时串到个人空间。
func TestReportWebAndMPEventsCarryTenantHeaders(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u-ent", EnterpriseID: "ent-42"}
	var webEnt, webTenant, mpEnt, mpTenant string
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Header.Get("X-Client-Product"))
		// X-Origin 在 Web 事件里等于 webBase，用它区分同路径的 Web/小程序请求。
		isWeb := r.Header.Get("X-Client-Product") == ""
		if r.Header.Get("X-Client-Product") == "workbuddy-mp" {
			mpEnt, mpTenant = r.Header.Get("X-Enterprise-Id"), r.Header.Get("X-Tenant-Id")
		}
		if isWeb {
			webEnt, webTenant = r.Header.Get("X-Enterprise-Id"), r.Header.Get("X-Tenant-Id")
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	// 两个 method 共用同一路径时，Web 请求没有 X-Client-Product，小程序有。
	// 若两者都打到同一 srv，用顺序区分更脆弱；这里用 path 前缀区分。
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, WebBaseCN: srv.URL, BillingBaseCN: srv.URL, GlobalEnabled: false}
	if err := c.ReportWebEvent(a, "library_doc_intro_click", srv.URL+"/library", "el", "文档"); err != nil {
		t.Fatalf("web event: %v", err)
	}
	if err := c.ReportMPEvent(a, map[string]any{"eventCode": "chat_request_send"}); err != nil {
		t.Fatalf("mp event: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("report hits=%v", hits)
	}
	if webEnt != "ent-42" || webTenant != "ent-42" {
		t.Errorf("web headers enterprise=%q tenant=%q", webEnt, webTenant)
	}
	if mpEnt != "ent-42" || mpTenant != "ent-42" {
		t.Errorf("mp headers enterprise=%q tenant=%q", mpEnt, mpTenant)
	}
}

// TestDesktopChatSequenceShape 断言完整对话事件链的事件码顺序与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}
	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Errorf("chat_message_response.isSuccessful=%v want true（RichMeow 判据）", resp["isSuccessful"])
	}
	if resp["finishReason"] != "stop" {
		t.Errorf("finishReason=%v want stop", resp["finishReason"])
	}
	if resp["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v want conv-1", resp["conversationId"])
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != desktopUA {
			t.Errorf("UA=%q want desktop UA", r.Header.Get("User-Agent"))
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(&auth.Auth{AccessToken: "at", UID: "u1"}, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	if got["kind"] != "theme" || got["resource_key"] != "theme-tkmw7j" {
		t.Errorf("body=%v want kind=theme resource_key=theme-tkmw7j", got)
	}
}

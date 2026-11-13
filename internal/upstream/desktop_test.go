package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	const wantDesktopUA = "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0"
	var got []map[string]any
	var gotUA, gotDomain, gotRequestID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		gotDomain = r.Header.Get("X-Domain")
		gotRequestID = r.Header.Get("X-Request-ID")
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
	if gotUA != wantDesktopUA {
		t.Errorf("UA=%q want %q", gotUA, wantDesktopUA)
	}
	// 真实客户端 /v2/report 的 X-Domain 是裸域名；X-Request-ID 是 32 位 hex。
	if gotDomain != "www.workbuddy.cn" {
		t.Errorf("X-Domain=%q want www.workbuddy.cn", gotDomain)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(gotRequestID) {
		t.Errorf("X-Request-ID=%q want 32 lowercase hex", gotRequestID)
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
	if ev["ideVersion"] != "5.9.3" || ev["extVersion"] != "5.9.3" {
		t.Errorf("ideVersion/extVersion=%v/%v want 5.9.3", ev["ideVersion"], ev["extVersion"])
	}
	if ev["commit"] != desktopCommit || ev["releaseDate"] != float64(desktopReleaseDate) {
		t.Errorf("release identity=%v/%v want %v/%v", ev["commit"], ev["releaseDate"], desktopCommit, desktopReleaseDate)
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
	// 设备/会话/匿名设备标识必须对齐真实客户端形状：machineId/sessionId 是 UUID，
	// qimei36 是 36 位小写 hex。
	if v, _ := ev["machineId"].(string); !regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`).MatchString(v) {
		t.Errorf("machineId=%q want uppercase UUID", v)
	}
	if v, _ := ev["sessionId"].(string); !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(v) {
		t.Errorf("sessionId=%q want lowercase UUID", v)
	}
	if v, _ := ev["qimei36"].(string); !regexp.MustCompile(`^[0-9a-f]{36}$`).MatchString(v) {
		t.Errorf("qimei36=%q want 36 lowercase hex", v)
	}
	if ev["reportDelay"] != float64(0) {
		t.Errorf("reportDelay=%v want 0（真实 5.9.3 样本）", ev["reportDelay"])
	}
}

// TestDesktopFingerprintSessionStable 同一进程内同一账号的 sessionId 必须稳定，
// 不同账号不能共享；真实客户端的 sessionId 只在客户端进程重启后变化。
func TestDesktopFingerprintSessionStable(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "session-test-a", Nickname: "a"}
	b := &auth.Auth{UID: "session-test-b", Nickname: "b"}
	s1, _ := c.desktopFingerprint(a)["sessionId"].(string)
	s2, _ := c.desktopFingerprint(a)["sessionId"].(string)
	s3, _ := c.desktopFingerprint(b)["sessionId"].(string)
	if s1 == "" || s1 != s2 {
		t.Fatalf("same account sessionId unstable: %q vs %q", s1, s2)
	}
	if s1 == s3 {
		t.Fatalf("different accounts share sessionId: %q", s1)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(s1) {
		t.Fatalf("sessionId=%q want lowercase UUID", s1)
	}
}

// TestReportDesktopEventUsesConfiguredVersion 桌面事件版本必须跟随 ClientVersion，
// 不能像旧实现那样在 UA、ideVersion、extVersion、X-IDE-Version 四处各写死 5.5.6。
func TestReportDesktopEventUsesConfiguredVersion(t *testing.T) {
	var gotUA, gotHeaderVersion string
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotHeaderVersion = r.Header.Get("X-IDE-Version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, ClientVersion: "6.1.0", CliVersion: "2.999.0"}
	if err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u1"}, DesktopEvent{"eventCode": "chat_message_send"}); err != nil {
		t.Fatal(err)
	}
	if want := "WorkBuddy/6.1.0 WorkBuddy/6.1.0 CLI/2.999.0"; gotUA != want {
		t.Fatalf("UA=%q want %q", gotUA, want)
	}
	if len(got) != 1 || got[0]["ideVersion"] != "6.1.0" || got[0]["extVersion"] != "6.1.0" {
		t.Fatalf("event version=%v", got)
	}
	// 未知版本没有可信的 commit/releaseDate，不能继续复用 5.9.3 的发布元数据。
	if _, ok := got[0]["releaseDate"]; ok {
		t.Fatalf("custom version must not reuse 5.9.3 releaseDate: %v", got[0]["releaseDate"])
	}
	if _, ok := got[0]["commit"]; ok {
		t.Fatalf("custom version must not reuse 5.9.3 commit: %v", got[0]["commit"])
	}
	// ReportDesktopEvent 仍保留旧头族；此断言确保它至少与 UA 同版本，避免再写死。
	if gotHeaderVersion != "" && gotHeaderVersion != "6.1.0" {
		t.Fatalf("X-IDE-Version=%q want 6.1.0", gotHeaderVersion)
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
	req := events[2]
	for _, removed := range []string{"maxToken", "maxSteps", "temperature", "maxRetries", "skillCount", "totalCount"} {
		if _, ok := req[removed]; ok {
			t.Errorf("chat_request_send 不应带旧版虚构字段 %s", removed)
		}
	}
	if req["isMaxMode"] != false || req["source"] != "work" {
		t.Errorf("chat_request_send new fields=%v/%v", req["isMaxMode"], req["source"])
	}
	if req["vcsType"] != "unknown" || req["vcsRepo"] != "" || req["vcsBranchName"] != "" || req["vcsRevId"] != "" {
		t.Errorf("chat_request_send vcs fields=%v/%v/%v/%v", req["vcsType"], req["vcsRepo"], req["vcsBranchName"], req["vcsRevId"])
	}
	if req["buddyId"] != "" || req["buddyName"] != "" {
		t.Errorf("chat_request_send buddy fields=%v/%v", req["buddyId"], req["buddyName"])
	}
	if trace, _ := req["traceId"].(string); !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(trace) {
		t.Errorf("traceId=%q want 32 lowercase hex", trace)
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0" {
			t.Errorf("UA=%q want current desktop UA", r.Header.Get("User-Agent"))
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

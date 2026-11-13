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

// TestReportChatActivitySendsArrayWithUserID 断言出站 body 是数组、含 userId、eventCode 正确。
func TestReportChatActivitySendsArrayWithUserID(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		var arr []map[string]any
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Fatalf("body must be a JSON array: %v (body=%s)", err, raw)
		}
		got = arr
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	if err := c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u-active"}, "wb2api-123", ""); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v want chat_request_send", ev["eventCode"])
	}
	if ev["userId"] != "u-active" {
		t.Errorf("userId=%v want u-active（缺失则服务端 200 但静默丢弃）", ev["userId"])
	}
	if ev["conversationId"] != "wb2api-123" {
		t.Errorf("conversationId=%v want wb2api-123", ev["conversationId"])
	}
	if ev["mode"] != "craft" {
		t.Errorf("mode=%v want craft", ev["mode"])
	}
	if ev["agentName"] != "cli" || ev["agentType"] != "main" || ev["source"] != "work" {
		t.Errorf("agent/source=%v/%v/%v want cli/main/work", ev["agentName"], ev["agentType"], ev["source"])
	}
	requestID, _ := ev["requestId"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(requestID) {
		t.Errorf("requestId=%q want 32 lowercase hex", requestID)
	}
	traceID, _ := ev["traceId"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traceID) {
		t.Errorf("traceId=%q want 32 lowercase hex", traceID)
	}
	for _, removed := range []string{"maxToken", "maxSteps", "temperature", "maxRetries", "skillCount", "totalCount"} {
		if _, ok := ev[removed]; ok {
			t.Errorf("chat_request_send 不应带旧版虚构字段 %s", removed)
		}
	}
	// 真实 5.9.3 payload 固定携带 VCS/Buddy 字段（空工作区为 unknown/空串）。
	if ev["vcsType"] != "unknown" || ev["vcsRepo"] != "" || ev["vcsBranchName"] != "" || ev["vcsRevId"] != "" {
		t.Errorf("vcs fields=%v/%v/%v/%v want unknown/empty", ev["vcsType"], ev["vcsRepo"], ev["vcsBranchName"], ev["vcsRevId"])
	}
	if ev["buddyId"] != "" || ev["buddyName"] != "" {
		t.Errorf("buddy fields=%v/%v want empty", ev["buddyId"], ev["buddyName"])
	}
	// 出站 body 必须是数组（以 [ 开头），不是单个对象。
}

// TestReportChatActivityServerError 业务 code 非 0 返回 *Error。
func TestReportChatActivityServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseCN: srv.URL}
	err := c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u1"}, "cid", "")
	if err == nil {
		t.Fatal("want error on 500")
	}
}

package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestBenefitsOverviewAggregates 福利概览必须把账号、签到分母、成长待办与
// 最近活动一次聚合完，且只回 uid8。页面首屏只看这一份快照，不能再自己拼。
func TestBenefitsOverviewAggregates(t *testing.T) {
	dir := t.TempDir()
	store := activity.New(activity.Config{Path: filepath.Join(dir, activity.FileName()), MaxBytes: 1 << 20})
	now := time.Now()
	if err := store.Append(activity.Record{
		Time: now.Add(-time.Minute), UID: "13800138000", Nickname: "秋叶", Realm: "cn",
		Task: activity.TaskCheckin, Trigger: activity.TriggerManual, OK: true, Message: "签到成功",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(activity.Record{
		Time: now.Add(-30 * time.Second), UID: "13800138000", Nickname: "秋叶", Realm: "cn",
		Task: activity.TaskDailyChat, Trigger: activity.TriggerManual, OK: false, Message: "HTTP 500",
	}); err != nil {
		t.Fatal(err)
	}

	pl := pool.New("")
	cn := &auth.Auth{UID: "13800138000", Nickname: "秋叶"}
	global := &auth.Auth{UID: "global-user", Nickname: "Global", Domain: "www.workbuddy.ai"}
	ent := &auth.Auth{UID: "enterprise-user", Nickname: "企业", EnterpriseID: "ent-1"}
	pl.Add(cn)
	pl.Add(global)
	pl.Add(ent)
	pl.NoteCheckinDone(cn.UID)

	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Activity: store})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/benefits", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if leak := "13800138000"; strings.Contains(rr.Body.String(), leak) {
		t.Fatalf("benefits leaked full UID: %s", rr.Body.String())
	}
	var out struct {
		Summary struct {
			Accounts       int `json:"accounts"`
			CN             int `json:"cn"`
			Global         int `json:"global"`
			Enterprise     int `json:"enterprise"`
			CheckinDone    int `json:"checkin_done"`
			CheckinPending int `json:"checkin_pending"`
			TodayFailed    int `json:"today_failed"`
		} `json:"summary"`
		Accounts []struct {
			UID            string `json:"uid"`
			Ref            string `json:"ref"`
			LastTask       string `json:"last_task"`
			LastOK         bool   `json:"last_ok"`
			CheckinDone    bool   `json:"checkin_done"`
			DailyChatToday bool   `json:"daily_chat_today"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Summary.Accounts != 3 || out.Summary.CN != 2 || out.Summary.Global != 1 || out.Summary.Enterprise != 1 {
		t.Fatalf("summary counts=%+v", out.Summary)
	}
	// 企业号不计入签到分母；国内个人号已签 1、待签 0。
	if out.Summary.CheckinDone != 1 || out.Summary.CheckinPending != 0 {
		t.Fatalf("checkin summary=%+v", out.Summary)
	}
	if out.Summary.TodayFailed != 1 {
		t.Fatalf("today_failed=%d want 1", out.Summary.TodayFailed)
	}
	if len(out.Accounts) != 3 {
		t.Fatalf("accounts=%+v", out.Accounts)
	}
	for _, a := range out.Accounts {
		if a.UID == "13800138000" || len(a.UID) > 8 {
			t.Fatalf("account uid not masked: %+v", a)
		}
		if a.Ref == "" || strings.Contains(rr.Body.String(), a.Ref+"\x00") {
			t.Fatalf("account ref missing or malformed: %+v", a)
		}
		if a.UID == "13800138" {
			if a.LastTask != "daily_chat" || a.LastOK || !a.CheckinDone {
				t.Fatalf("last activity/checkin mismatch: %+v", a)
			}
		}
	}
}

package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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

// TestBenefitsWelfareGrantsFromSnapshot 福利页的总积分与最近发放必须来自
// 同一份积分快照：只统计签到/成长/活动等福利包，排除免费套餐，并且只暴露
// uid8。没有快照时必须显式 available=false，不能把“尚未刷新”伪装成 0 元福利。
func TestBenefitsWelfareGrantsFromSnapshot(t *testing.T) {
	pl := pool.New("")
	const uid = "13800138000"
	const entUID = "enterprise-138001"
	pl.Add(&auth.Auth{UID: uid, Nickname: "秋叶"})
	pl.Add(&auth.Auth{UID: entUID, Nickname: "企业", EnterpriseID: "ent-1"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	now := time.Now()
	p.rememberCreditSnapshot(uid, "秋叶", "cn", []upstream.CreditPackage{
		{Name: "Buddy 加油站签到奖励", Size: 30, Remain: 30, CreatedAt: now.Add(-time.Minute).Format(time.RFC3339)},
		{Name: "成长计划奖励", Size: 50, Remain: 20, CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)},
		{Name: "免费套餐", Size: 1500, Remain: 1500, CreatedAt: now.Add(-3 * time.Hour).Format(time.RFC3339)},
		{Name: "普通体验包", Size: 300, Remain: 300, CreatedAt: now.Add(-4 * time.Hour).Format(time.RFC3339)},
	})
	p.rememberCreditSnapshot(entUID, "企业", "cn", []upstream.CreditPackage{
		{Name: "企业签到奖励", Size: 999, Remain: 999, CreatedAt: now.Format(time.RFC3339)},
	})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/benefits", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), uid) {
		t.Fatalf("benefits grants leaked full UID: %s", rr.Body.String())
	}
	var out struct {
		Summary struct {
			WelfareCreditsTotal    int64  `json:"welfare_credits_total"`
			WelfareGrantCount      int    `json:"welfare_grant_count"`
			WelfareGrantsAvailable bool   `json:"welfare_grants_available"`
			WelfareSnapshotAt      string `json:"welfare_snapshot_at"`
			Enterprise             int    `json:"enterprise"`
		} `json:"summary"`
		Recent []struct {
			UID       string `json:"uid"`
			Name      string `json:"name"`
			Size      int64  `json:"size"`
			CreatedAt string `json:"created_at"`
		} `json:"recent_grants"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Summary.WelfareGrantsAvailable || out.Summary.WelfareCreditsTotal != 80 || out.Summary.WelfareGrantCount != 2 || out.Summary.Enterprise != 1 {
		t.Fatalf("welfare summary=%+v", out.Summary)
	}
	if len(out.Recent) != 2 || out.Recent[0].Name != "Buddy 加油站签到奖励" || out.Recent[0].Size != 30 {
		t.Fatalf("recent grants not sorted/filtered: %+v", out.Recent)
	}
	for _, r := range out.Recent {
		if r.UID == uid || len(r.UID) > 8 {
			t.Fatalf("grant uid not masked: %+v", r)
		}
	}

	empty := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	req = httptest.NewRequest(http.MethodGet, "/panel/api/benefits", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr = httptest.NewRecorder()
	empty.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"welfare_grants_available":false`) {
		t.Fatalf("empty snapshot must be explicit: %d %s", rr.Code, rr.Body.String())
	}
}

// TestBenefitsFocusReusesGrowthSnapshot 福利页的聚焦任务明细必须来自同一次
// 成长快照，不能为聚焦账号额外打任务列表端点；focus 使用短引用且只回显 uid8。
func TestBenefitsFocusReusesGrowthSnapshot(t *testing.T) {
	pl := pool.New("")
	first := &auth.Auth{UID: "13800138000", Nickname: "甲", AccessToken: "at1"}
	second := &auth.Auth{UID: "13900139000", Nickname: "乙", AccessToken: "at2"}
	ent := &auth.Auth{UID: "enterprise-1", Nickname: "企业", EnterpriseID: "ent-1", AccessToken: "at3"}
	gl := &auth.Auth{UID: "global-1", Nickname: "国际", Domain: "www.workbuddy.ai", AccessToken: "at4"}
	pl.Add(first)
	pl.Add(second)
	pl.Add(ent)
	pl.Add(gl)

	calls := 0
	up := upstream.New()
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"code":0,"msg":"ok","data":{"tasks":[]}}`
		switch {
		case strings.Contains(r.URL.Path, "/activity/growth/streak"):
			body = `{"code":0,"msg":"ok","data":{"streak":{"days":3},"makeup_cards":{"balance":1},"redemption_status":{"tier_7d_status":"locked","tier_14d_status":"locked","tier_28d_status":"locked"}}}`
		case strings.Contains(r.URL.Path, "/activity/growth/lottery/summary"):
			body = `{"code":0,"msg":"ok","data":{"chances":2}}`
		case strings.Contains(r.URL.Path, "/activity/growth/energy"):
			body = `{"code":0,"msg":"ok","data":{"balance":7}}`
		case strings.Contains(r.URL.Path, "/buddy/travel/status"):
			body = `{"code":0,"msg":"ok","data":{"state":"home"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, Upstream: up})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/benefits", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), first.UID) || strings.Contains(rr.Body.String(), second.UID) {
		t.Fatalf("benefits leaked full UID: %s", rr.Body.String())
	}
	var out struct {
		Accounts []struct {
			UID     string `json:"uid"`
			Ref     string `json:"ref"`
			Focused bool   `json:"focused"`
			Realm   string `json:"realm"`
		} `json:"accounts"`
		TaskRows []upstream.Task `json:"task_rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var focused string
	for _, a := range out.Accounts {
		if a.Focused {
			if focused != "" {
				t.Fatalf("multiple focused accounts: %+v", out.Accounts)
			}
			focused = a.Ref
			if a.UID != statusUID8(first.UID) {
				t.Fatalf("default focus=%q want first CN personal account", a.UID)
			}
		}
	}
	if focused == "" {
		t.Fatalf("no focused account: %+v", out.Accounts)
	}
	if out.TaskRows == nil {
		t.Fatal("task_rows must be an empty array, not null")
	}
	firstCalls := calls

	req = httptest.NewRequest(http.MethodGet, "/panel/api/benefits?focus="+benefitRefFor(second.UID), nil)
	req.Header.Set("Authorization", "Bearer k")
	rr = httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("focus code=%d body=%s", rr.Code, rr.Body.String())
	}
	out.Accounts = nil
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Accounts {
		if a.Focused && a.UID != statusUID8(second.UID) {
			t.Fatalf("focus selected %q want %q", a.UID, statusUID8(second.UID))
		}
	}
	// 第二次快照只重跑同一份成长快照；focus 不应再额外打任务列表端点。
	// 首次 12 次 = 2 个国内个人号 ×（streak/lottery/energy/travel/task/mp-task）。
	if delta := calls - firstCalls; delta != firstCalls {
		t.Fatalf("focus caused extra upstream calls: delta=%d first=%d", delta, firstCalls)
	}
}

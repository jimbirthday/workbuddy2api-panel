package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func activityHistoryRequest(t *testing.T, p *Panel, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

// TestActivityHistoryFiltersAndLimit 活动接口必须与 Store 使用同一套过滤口径：
// uid/task/result/from/to/limit 生效，响应带 total 与 has_more，且最新在前。
func TestActivityHistoryFiltersAndLimit(t *testing.T) {
	dir := t.TempDir()
	store := activity.New(activity.Config{Path: filepath.Join(dir, activity.FileName()), MaxBytes: 1 << 20})
	base := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	records := []activity.Record{
		{Time: base, UID: "u1", Task: activity.TaskCheckin, Trigger: activity.TriggerManual, OK: true, Message: "one"},
		{Time: base.Add(time.Minute), UID: "u2", Task: activity.TaskActivity, Trigger: activity.TriggerSchedule, OK: false, Message: "two"},
		{Time: base.Add(2 * time.Minute), UID: "u1", Task: activity.TaskTravel, Trigger: activity.TriggerManual, OK: true, Message: "three"},
		{Time: base.Add(3 * time.Minute), UID: "u1", Task: activity.TaskCheckin, Trigger: activity.TriggerSchedule, OK: true, Message: "four"},
	}
	for _, rec := range records {
		if err := store.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Activity: store})

	rr := activityHistoryRequest(t, p, "/panel/api/activity/history?uid=u1&task=checkin&result=ok&limit=1")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Records []activity.Record `json:"records"`
		Total   int               `json:"total"`
		Limit   int               `json:"limit"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 2 || out.Limit != 1 || len(out.Records) != 1 || !out.HasMore {
		t.Fatalf("unexpected response: %+v", out)
	}
	if out.Records[0].Message != "four" {
		t.Fatalf("newest record=%q want four", out.Records[0].Message)
	}

	from := base.Add(90 * time.Second).Unix()
	to := base.Add(150 * time.Second).Unix()
	rr = activityHistoryRequest(t, p, "/panel/api/activity/history?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10))
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || len(out.Records) != 1 || out.Records[0].Message != "three" {
		t.Fatalf("time range response: %+v", out)
	}
}

// TestActivityHistoryInvalidFilters 非法 task/result 返回 400；空 Store 返回 501；
// 未带鉴权返回 401。这些是输入边界，不能把内部错误当 200 或 500。
func TestActivityHistoryInvalidFilters(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Activity: activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})})
	for _, path := range []string{
		"/panel/api/activity/history?task=nope",
		"/panel/api/activity/history?result=nope",
	} {
		rr := activityHistoryRequest(t, p, path)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s code=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}

	noStore := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	if rr := activityHistoryRequest(t, noStore, "/panel/api/activity/history"); rr.Code != http.StatusNotImplemented {
		t.Fatalf("nil store code=%d want 501", rr.Code)
	}

	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/panel/api/activity/history", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code=%d want 401", rr.Code)
	}
}

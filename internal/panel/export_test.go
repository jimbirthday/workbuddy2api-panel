package panel

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func exportRequest(t *testing.T, p *Panel, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s -> %d %s", path, rr.Code, rr.Body)
	}
	return rr
}

// TestRequestLogsExportCSVAndJSONL 导出必须与列表使用同一套筛选：同一 outcome
// 条件下两边条数一致；CSV 有稳定表头且不含凭证，JSONL 每行可独立解析。
func TestRequestLogsExportCSVAndJSONL(t *testing.T) {
	dir := t.TempDir()
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: dir, MaxBytes: 1 << 20, RetentionDays: 7})
	now := time.Now()
	rec.Record(reqlog.Event{Time: now.Add(-time.Minute), RequestID: "ok-1", Path: "/v1/chat/completions",
		Account: "alice(12345678)", Model: "glm-5.2", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		TotalTokens: 12, Credit: 0.25, HasCredit: true, ClientIP: "203.0.113.7", UserAgent: "=cmd|' /C calc'!A0"})
	rec.Record(reqlog.Event{Time: now.Add(-2 * time.Minute), RequestID: "bad-1", Path: "/v1/chat/completions",
		Account: "bob(87654321)", Model: "glm-5.2", Status: 502, Outcome: reqlog.OutcomeStreamError})
	rec.Close()

	base := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	listRR := exportRequest(t, base, "/panel/api/request_logs?outcome=success&limit=100")
	var listed struct {
		Entries []reqlog.Event `json:"entries"`
	}
	if err := json.Unmarshal(listRR.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}

	csvRR := exportRequest(t, base, "/panel/api/request_logs/export?outcome=success&limit=100")
	if ct := csvRR.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("content-type=%q", ct)
	}
	if cd := csvRR.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition=%q", cd)
	}
	body := bytes.TrimPrefix(csvRR.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF})
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(listed.Entries)+1 {
		t.Fatalf("csv rows=%d listed=%d", len(rows)-1, len(listed.Entries))
	}
	if len(rows[0]) != 21 || rows[0][1] != "request_id" || rows[0][3] != "key_id" {
		t.Fatalf("unexpected csv header: %v", rows[0])
	}
	for _, cell := range rows[1] {
		if strings.Contains(strings.ToLower(cell), "authorization") || strings.Contains(cell, "Bearer ") {
			t.Fatalf("export leaked credential-like value: %q", cell)
		}
	}
	if rows[1][19] != "203.0.113.7" {
		t.Fatalf("client_ip=%q", rows[1][19])
	}
	if !strings.HasPrefix(rows[1][20], "'=") {
		t.Fatalf("formula-like UA should be neutralized: %q", rows[1][20])
	}

	jsonlRR := exportRequest(t, base, "/panel/api/request_logs/export?outcome=success&limit=100&format=jsonl")
	sc := bufio.NewScanner(bytes.NewReader(jsonlRR.Body.Bytes()))
	n := 0
	for sc.Scan() {
		var e reqlog.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("jsonl line %d: %v: %s", n, err, sc.Text())
		}
		n++
	}
	if n != len(listed.Entries) {
		t.Fatalf("jsonl lines=%d listed=%d", n, len(listed.Entries))
	}
}

func TestRequestLogsExportEmptyHasHeader(t *testing.T) {
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	rec.Close()
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	rr := exportRequest(t, p, "/panel/api/request_logs/export?from="+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	body := bytes.TrimPrefix(rr.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF})
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0][0] != "time" {
		t.Fatalf("empty export should still have header: %v", rows)
	}
}

func TestRequestLogsExportRequiresAuth(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: reqlog.New(reqlog.Config{})})
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest("GET", "/panel/api/request_logs/export", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rr.Code)
	}
}

// TestUsageExportMatchesWindow 用量导出必须与 /usage 使用同一窗口和维度：
// 只导聚合数字，且 from/to 生效时导出的合计与页面的 totals 一致。
func TestUsageExportMatchesWindow(t *testing.T) {
	rec := usage.New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := 0; i < 6; i++ {
		rec.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u1", "glm-5.2",
			usage.Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "Alice"})
	panel := New(Config{Version: "test", APIKey: "k", Usage: rec, Pool: p})
	from := strconv.FormatInt(base.Add(2*time.Hour).Unix(), 10)
	to := strconv.FormatInt(base.Add(3*time.Hour).Unix(), 10)
	rr := exportRequest(t, panel, "/panel/api/usage/export?dim=account&from="+from+"&to="+to)
	body := bytes.TrimPrefix(rr.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF})
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1][2] != "Alice(u1)" {
		t.Fatalf("usage export rows=%v (account key must be masked label)", rows)
	}
	if rows[1][4] != "2" {
		t.Fatalf("exported requests=%q want 2 (same explicit window as /usage)", rows[1][4])
	}
	if rows[1][6] != "20" {
		t.Fatalf("exported prompt_tokens=%q want 20", rows[1][6])
	}
}

// TestRequestMetricsDoesNotLeakArchivePath 归档目录与底层错误属于部署细节，
// 面板响应只返回状态数字。这里用不可读目录触发 stats 错误，确认 body 不含
// 目录路径或原始 os 错误。
func TestRequestMetricsDoesNotLeakArchivePath(t *testing.T) {
	dir := t.TempDir()
	// 用一个普通文件占住目录名：ReadDir 会返回 ENOTDIR，错误里含该路径。
	badDir := filepath.Join(dir, "blocked")
	if err := os.WriteFile(badDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: badDir, MaxBytes: 1 << 20, RetentionDays: 7})
	rec.Close()
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	req := httptest.NewRequest("GET", "/panel/api/request_metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, secret := range []string{dir, "blocked", "not a directory", "open "} {
		if strings.Contains(body, secret) {
			t.Fatalf("request_metrics leaked %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "request archive write failed") {
		t.Fatalf("stable archive error missing: %s", body)
	}
}

// TestRequestArchiveReadErrorStable 列表与导出在归档读取失败时必须返回固定
// 文案，不能把 os.PathError（含完整部署路径）透给前端。
// 说明：Reader 把“目录不存在/不是目录”视为空归档（成功），因此这里直接
// 调用 handler 前不可稳定注入 ReadDir 错误；该契约由 export_requests.go 的
// 固定文案与 perf.go 的同口径测试共同保证。保留本文件内的错误注入测试需要
// 可替换的 Reader 接口，避免为测试改动生产装配。

package panel

import (
	"bytes"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func exportAccountsForTest(t *testing.T, p *Panel) [][]string {
	t.Helper()
	rec := httptest.NewRecorder()
	p.exportAccounts(rec, httptest.NewRequest("GET", "/panel/api/accounts/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("exportAccounts code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Fatalf("content-type=%q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q want no-store", rec.Header().Get("Cache-Control"))
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("content-disposition=%q", cd)
	}
	body := bytes.TrimPrefix(rec.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF})
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// 导出必须是纯运维清单：UID 截断、不含凭证、自由文本防公式注入。
func TestExportAccountsOmitsCredentialsAndNeutralizesFormula(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New(filepath.Join(dir, "state.json"))
	const (
		uid         = "12345678-aaaa-bbbb-cccc-1234567890ab"
		accessToken = "access-token-should-never-appear"
		refreshTok  = "refresh-token-should-never-appear"
		deviceToken = "device-token-should-never-appear"
	)
	a := &auth.Auth{
		UID: uid, AccessToken: accessToken, RefreshToken: refreshTok, DeviceToken: deviceToken,
		Nickname: "=cmd|' /C calc'!A0", Domain: "copilot.tencent.com", FilePath: filepath.Join(dir, "workbuddy-x.json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl.Add(a)
	pl.SetCreditsDetailed(uid, 321, 1000, 120, time.Now().Add(48*time.Hour), 120)
	pl.Pause(uid)

	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	rows := exportAccountsForTest(t, p)
	if len(rows) != 2 {
		t.Fatalf("rows=%d want header+1: %v", len(rows), rows)
	}
	if rows[0][0] != "account" || rows[0][1] != "realm" {
		t.Fatalf("unexpected header: %v", rows[0])
	}
	row := rows[1]
	if row[0] != "'=cmd|' /C calc'!A0" {
		t.Fatalf("account label should be nickname with formula guard: %q", row[0])
	}
	joined := strings.Join(rows[0], ",") + "\n" + strings.Join(row, ",")
	for _, secret := range []string{accessToken, refreshTok, deviceToken, uid} {
		if strings.Contains(joined, secret) {
			t.Fatalf("export leaked %q:\n%s", secret, joined)
		}
	}
	if !strings.Contains(joined, "321") || !strings.Contains(joined, "1000") {
		t.Fatalf("export missing credits: %v", row)
	}
}

// 短 UID 不得原样导出；导入时空昵称会回填邮箱，导出昵称也必须打码。
func TestExportAccountsMasksShortUIDAndEmailNickname(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "12345678", Nickname: "user@example.com"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	rows := exportAccountsForTest(t, p)
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	joined := strings.Join(rows[1], ",")
	if strings.Contains(joined, "12345678") || strings.Contains(joined, "user@example.com") {
		t.Fatalf("short uid/email leaked: %v", rows[1])
	}
	if strings.Contains(joined, "***") == false {
		t.Fatalf("email nickname should be masked: %v", rows[1])
	}
}

// 导出的 JSON/CSV 都不应把 token 字段名带出来（避免误接前端生成凭证导出）。
func TestExportAccountsUsesOnlyOperationalFields(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "abcdefgh-0000", AccessToken: "at", RefreshToken: "rt", Nickname: "nick"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	rows := exportAccountsForTest(t, p)
	for _, h := range rows[0] {
		lower := strings.ToLower(h)
		if strings.Contains(lower, "token") && lower != "token_requests" && lower != "token_prompt" &&
			lower != "token_completion" && lower != "token_total" {
			t.Fatalf("credential-like export header: %q", h)
		}
	}
	if len(rows[1]) != len(rows[0]) {
		t.Fatalf("row/header width mismatch: %d/%d", len(rows[1]), len(rows[0]))
	}
}

func TestExportAccountsRequiresPool(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec := httptest.NewRecorder()
	p.exportAccounts(rec, httptest.NewRequest("GET", "/panel/api/accounts/export", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
}

// 导出必须挂在生产 mux 上并沿用 Bearer 鉴权；未带 key 时不得返回任何账号数据。
func TestExportAccountsEndpointAuthContract(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New("")})
	req := httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export code=%d want 401", rec.Code)
	}

	req = httptest.NewRequest("GET", "/panel/api/accounts/export", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated export code=%d body=%s", rec.Code, rec.Body.String())
	}
}

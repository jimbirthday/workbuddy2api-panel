package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func cockpitImportRequest(t *testing.T, p *Panel, accounts string, query string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte(accounts))
	for key, value := range parseImportQuery(query) {
		_ = mw.WriteField(key, value)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit"+query, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	p.importCockpit(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode import response: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, out
}

func parseImportQuery(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" {
		return out
	}
	raw = strings.TrimPrefix(raw, "?")
	for _, part := range strings.Split(raw, "&") {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

func importNumber(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing response field %q: %v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s type=%T want number", key, v)
	}
	return int(f)
}

func newImportPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New(filepath.Join(dir, "state.json"))
	return New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: dir}), dir
}

// dry-run 必须只解析分类，不写 auth 文件、不入池、不触发任何上游动作。
func TestImportCockpitDryRunHasNoSideEffects(t *testing.T) {
	p, dir := newImportPanel(t)
	const token = "super-secret-access-token"
	body := fmt.Sprintf(`[{"id":"row-1","uid":"uid-dry-run","access_token":%q,"refresh_token":"refresh-secret","nickname":"tester"}]`, token)
	rec, out := cockpitImportRequest(t, p, body, "?dryRun=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["dry_run"] != true || out["overwrite"] != false {
		t.Fatalf("dry-run flags=%v", out)
	}
	if importNumber(t, out, "added") != 1 || importNumber(t, out, "updated") != 0 || importNumber(t, out, "invalid") != 0 {
		t.Fatalf("unexpected dry-run result: %v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-dry-run.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write auth file, stat err=%v", err)
	}
	if p.cfg.Pool.AuthByUID("uid-dry-run") != nil {
		t.Fatal("dry-run must not add account to pool")
	}
	if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "refresh-secret") {
		t.Fatalf("response leaked credentials: %s", rec.Body.String())
	}
}

// 默认不覆盖同 UID：已有文件时跳过，不写盘、不替换 token；overwrite=true 才更新。
func TestImportCockpitSkipByDefaultAndOverwrite(t *testing.T) {
	p, dir := newImportPanel(t)
	file := filepath.Join(dir, "workbuddy-uid-existing.json")
	old := &auth.Auth{UID: "uid-existing", AccessToken: "old-access", RefreshToken: "old-refresh", Nickname: "old", FilePath: file}
	if err := old.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	p.cfg.Pool.Add(old)
	if p.cfg.Pool.Pause("uid-existing") != true {
		t.Fatal("pause existing account")
	}

	body := `[{"uid":"uid-existing","access_token":"new-access","refresh_token":"new-refresh","nickname":"new"}]`
	rec, out := cockpitImportRequest(t, p, body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if importNumber(t, out, "skipped") != 1 || importNumber(t, out, "added") != 0 || importNumber(t, out, "updated") != 0 {
		t.Fatalf("default import should skip existing UID: %v", out)
	}
	if got := p.cfg.Pool.AuthByUID("uid-existing").AccessTokenValue(); got != "old-access" {
		t.Fatalf("default import overwrote token: %q", got)
	}

	rec, out = cockpitImportRequest(t, p, body, "?overwrite=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if importNumber(t, out, "updated") != 1 || importNumber(t, out, "skipped") != 0 || importNumber(t, out, "added") != 0 {
		t.Fatalf("overwrite import should update existing UID: %v", out)
	}
	if got := p.cfg.Pool.AuthByUID("uid-existing").AccessTokenValue(); got != "new-access" {
		t.Fatalf("overwrite token=%q want new-access", got)
	}
	if st, ok := p.cfg.Pool.Status("uid-existing"); !ok || st.Disabled || !st.Paused {
		t.Fatalf("overwrite must preserve operator health state, got %+v", st)
	}
	parsed, err := auth.Parse(mustReadFile(t, file))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AccessToken != "new-access" || parsed.RefreshToken != "new-refresh" {
		t.Fatalf("overwritten auth file has stale credentials: %+v", parsed)
	}
}

// 非法 UID/缺字段计 invalid；错误只含行 id/uid，不含任何 token。
func TestImportCockpitInvalidRowsRedacted(t *testing.T) {
	p, dir := newImportPanel(t)
	body := `[
  {"id":"bad-uid","uid":"../evil","access_token":"tok-one","refresh_token":"ref-one"},
  {"id":"missing","uid":"ok-uid","access_token":"tok-two"},
  {"id":"ok","uid":"uid-good","access_token":"tok-three","refresh_token":"ref-three"}
]`
	rec, out := cockpitImportRequest(t, p, body, "?dryRun=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if importNumber(t, out, "invalid") != 2 || importNumber(t, out, "added") != 1 {
		t.Fatalf("invalid counts=%v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-good.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run must not write any file, stat err=%v", err)
	}
	raw := rec.Body.String()
	for _, secret := range []string{"tok-one", "ref-one", "tok-two", "tok-three", "ref-three"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("invalid-row response leaked %q: %s", secret, raw)
		}
	}
}

// 同文件重复 UID 只处理一次，避免覆盖模式并发写同一文件。
func TestImportCockpitDuplicateUIDInPayload(t *testing.T) {
	p, dir := newImportPanel(t)
	body := `[
  {"uid":"uid-dup","access_token":"a1","refresh_token":"r1"},
  {"uid":"uid-dup","access_token":"a2","refresh_token":"r2"}
]`
	rec, out := cockpitImportRequest(t, p, body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if importNumber(t, out, "added") != 1 || importNumber(t, out, "skipped") != 1 {
		t.Fatalf("duplicate handling=%v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-dup.json")); err != nil {
		t.Fatalf("first duplicate should be written: %v", err)
	}
}

func TestImportCockpitShutdownLeavesNoPartialWrites(t *testing.T) {
	p, dir := newImportPanel(t)
	p.StopWaiting()
	body := `[{"uid":"uid-shutdown","access_token":"a","refresh_token":"r"}]`
	rec, out := cockpitImportRequest(t, p, body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if importNumber(t, out, "added") != 0 || importNumber(t, out, "skipped") != 1 {
		t.Fatalf("shutdown result=%v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-shutdown.json")); !os.IsNotExist(err) {
		t.Fatalf("shutdown import must not write file, stat err=%v", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 并发导入多个账号时，计数与错误切片必须在 worker 并发下保持一致；-race
// 还要能检出共享结果的写竞争。
func TestCockpitImportConcurrentJobsAreRaceFree(t *testing.T) {
	dir := t.TempDir()
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), AuthDir: dir})
	var accounts []cockpitAccount
	for i := 0; i < 8; i++ {
		accounts = append(accounts, cockpitAccount{
			UID: fmt.Sprintf("uid-concurrent-%d", i), AccessToken: "at", RefreshToken: "rt",
		})
	}
	res := p.importCockpitAccounts(accounts, false, false)
	if res.Added != 8 || res.Skipped != 0 || res.Invalid != 0 {
		t.Fatalf("concurrent import result = %+v", res)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
}

// overwrite 下同文件重复 UID 应保留最后一条，且只产生一次写盘/更新。
func TestCockpitImportOverwriteDuplicateUIDUsesLast(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	base := &auth.Auth{UID: "uid-dup", AccessToken: "old-at", RefreshToken: "old-rt", FilePath: filepath.Join(dir, "workbuddy-uid-dup.json")}
	if err := base.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl.Add(base)
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: dir})
	accounts := []cockpitAccount{
		{UID: "uid-dup", AccessToken: "first-at", RefreshToken: "first-rt"},
		{UID: "uid-dup", AccessToken: "last-at", RefreshToken: "last-rt"},
	}
	res := p.importCockpitAccounts(accounts, false, true)
	if res.Updated != 1 || res.Added != 0 || res.Skipped != 0 {
		t.Fatalf("duplicate overwrite result = %+v", res)
	}
	if got := pl.AuthByUID("uid-dup").AccessTokenValue(); got != "last-at" {
		t.Fatalf("token=%q want last-at", got)
	}
}

// 导入错误不能回显完整 UID；覆盖导入应清除旧凭据造成的自动禁用，但保留
// 人工禁用/暂停决定。
func TestCockpitImportMasksUIDAndPreservesManualState(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	const fullUID = "13800138000"
	a := &auth.Auth{UID: fullUID, AccessToken: "old-at", RefreshToken: "old-rt", FilePath: filepath.Join(dir, "workbuddy-13800138000.json")}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl.Add(a)
	pl.Disable(fullUID, "12153 session dead")
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, AuthDir: dir})
	res := p.importCockpitAccounts([]cockpitAccount{{UID: fullUID, AccessToken: "new-at", RefreshToken: "new-rt"}}, false, false)
	if res.Skipped != 1 || len(res.Errors) != 1 || strings.Contains(res.Errors[0], fullUID) {
		t.Fatalf("skip error must mask uid: %+v", res)
	}
	res = p.importCockpitAccounts([]cockpitAccount{{UID: fullUID, AccessToken: "new-at", RefreshToken: "new-rt"}}, false, true)
	if res.Updated != 1 {
		t.Fatalf("overwrite result = %+v", res)
	}
	if st, ok := pl.Status(fullUID); !ok || st.Disabled {
		t.Fatalf("automatic disable must clear on credential replacement: %+v", st)
	}

	pl.Disable(fullUID, pool.ManualDisableReason)
	pl.Pause(fullUID)
	res = p.importCockpitAccounts([]cockpitAccount{{UID: fullUID, AccessToken: "newer-at", RefreshToken: "newer-rt"}}, false, true)
	if res.Updated != 1 {
		t.Fatalf("manual overwrite result = %+v", res)
	}
	if st, ok := pl.Status(fullUID); !ok || !st.Disabled || !st.Paused {
		t.Fatalf("manual disabled/paused must be preserved: %+v", st)
	}
}

package panel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

func profileTestPanel(t *testing.T) (*Panel, *pool.Pool, *auth.Auth) {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", Nickname: "甲", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl := pool.New(filepath.Join(dir, "state.json"))
	pl.Add(a)
	return New(Config{Version: "test", APIKey: "k", Pool: pl}), pl, a
}

func profilePost(t *testing.T, p *Panel, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestAccountProfileAPI(t *testing.T) {
	p, pl, _ := profileTestPanel(t)
	rec := profilePost(t, p, "/panel/api/accounts/u1/profile", `{"weight":12,"note":"客户A"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := pl.Status("u1")
	if !ok || st.Weight != 12 || st.Note != "客户A" {
		t.Fatalf("status=%+v ok=%v", st, ok)
	}
	// 只更新备注：权重保持上次值，而不是被缺省 1 覆盖。
	rec = profilePost(t, p, "/panel/api/accounts/u1/profile", `{"note":"自用"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("note-only code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Weight != 12 || st.Note != "自用" {
		t.Fatalf("partial update status=%+v", st)
	}
}

func TestAccountProfileNoteUsesRuneLimit(t *testing.T) {
	p, pl, a := profileTestPanel(t)
	long := strings.Repeat("备", 240)
	rec := profilePost(t, p, "/panel/api/accounts/u1/profile", `{"note":"`+long+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	want := strings.Repeat("备", 200)
	if st, _ := pl.Status("u1"); st.Note != want {
		t.Fatalf("pool note len=%d want 200", len([]rune(st.Note)))
	}
	weight, note, _, _ := a.ProfileSnapshot()
	if note != want || weight != 1 {
		t.Fatalf("auth profile weight=%d note-len=%d", weight, len([]rune(note)))
	}
	loaded, err := auth.LoadDir(filepath.Dir(a.FilePath))
	if err != nil || len(loaded) != 1 || loaded[0].Note != want {
		t.Fatalf("auth file note len=%d err=%v", len([]rune(loaded[0].Note)), err)
	}
}

func TestAccountProfileValidationAndAuth(t *testing.T) {
	p, pl, _ := profileTestPanel(t)
	for _, body := range []string{``, `{}`, `{"weight":0}`, `{"weight":101}`, `{"weight":"x"}`} {
		if rec := profilePost(t, p, "/panel/api/accounts/u1/profile", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%q code=%d want 400", body, rec.Code)
		}
	}
	if rec := profilePost(t, p, "/panel/api/accounts/missing/profile", `{"weight":2}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing account code=%d want 404", rec.Code)
	}
	if st, _ := pl.Status("u1"); st.Weight != 1 || st.Note != "" {
		t.Errorf("invalid request mutated profile: %+v", st)
	}
	// 无管理密钥必须 401，且不能泄露账号存在性。
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/profile", strings.NewReader(`{"weight":2}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth code=%d want 401", rec.Code)
	}
}

func TestAccountsSetAllAPI(t *testing.T) {
	p, pl, _ := profileTestPanel(t)
	// 第二个账号：验证 uids[] 只影响选中账号，不误伤其他号。
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u2.json")
	a2 := &auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", Nickname: "乙", FilePath: fp}
	if err := a2.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl.Add(a2)
	rec := profilePost(t, p, "/panel/api/accounts/set_all", `{"enabled":false,"uids":["u1","u2"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":2`) {
		t.Fatalf("disable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); !st.Disabled || st.DisabledReason != pool.ManualDisableReason {
		t.Fatalf("disable state=%+v", st)
	}
	rec = profilePost(t, p, "/panel/api/accounts/set_all", `{"enabled":true,"uids":["u1","u2"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Disabled {
		t.Fatalf("enable state=%+v", st)
	}
	if rec := profilePost(t, p, "/panel/api/accounts/set_all", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing enabled code=%d want 400", rec.Code)
	}
	// 作用域红线：即使 enabled 合法，缺失 uids 也不能退化成“整池”。
	// 先重新禁用，确认缺失 uids 的请求既返回 400 又不改状态。
	if rec := profilePost(t, p, "/panel/api/accounts/set_all", `{"enabled":false,"uids":["u1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("disable u1 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := profilePost(t, p, "/panel/api/accounts/set_all", `{"enabled":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing uids code=%d want 400", rec.Code)
	}
	if st, _ := pl.Status("u1"); !st.Disabled {
		t.Errorf("missing uids must not enable the whole pool: %+v", st)
	}
}

func TestAccountsSetAllSelectedUIDs(t *testing.T) {
	p, pl, _ := profileTestPanel(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u2.json")
	a2 := &auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", FilePath: fp}
	if err := a2.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	pl.Add(a2)
	rec := profilePost(t, p, "/panel/api/accounts/set_all", `{"enabled":false,"uids":["u1"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"changed":1`) {
		t.Fatalf("selected disable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); !st.Disabled {
		t.Fatalf("u1 should be disabled: %+v", st)
	}
	if st, _ := pl.Status("u2"); st.Disabled {
		t.Fatalf("u2 must stay enabled: %+v", st)
	}
	if rec := profilePost(t, p, "/panel/api/accounts/set_all", `{"enabled":true,"uids":["u1","missing"]}`); rec.Code != http.StatusOK {
		t.Fatalf("selected enable code=%d body=%s", rec.Code, rec.Body.String())
	}
	if st, _ := pl.Status("u1"); st.Disabled {
		t.Fatalf("u1 should be enabled: %+v", st)
	}
}

func TestAccountRemoveQuarantinesAuthFile(t *testing.T) {
	p, pl, a := profileTestPanel(t)
	dir := filepath.Dir(a.FilePath)
	rec := profilePost(t, p, "/panel/api/accounts/u1/remove", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("remove code=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := pl.Status("u1"); ok {
		t.Fatal("u1 should be removed from pool")
	}
	loaded, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("removed auth file must not be reloaded after restart: %+v", loaded)
	}
	if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
		t.Fatalf("auth file should be gone from load path, stat err=%v", err)
	}
}

// 并发移除同一账号只能有一个请求真正成功；失败者不得把已隔离的凭证文件
// rename 回 auths/。否则重启后账号会复活。
func TestAccountRemoveConcurrentDoesNotResurrect(t *testing.T) {
	p, pl, a := profileTestPanel(t)
	dir := filepath.Dir(a.FilePath)
	const workers = 12
	codes := make(chan int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/u1/remove", nil)
			req.Header.Set("Authorization", "Bearer k")
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, req)
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)

	okCount := 0
	for code := range codes {
		switch code {
		case http.StatusOK:
			okCount++
		case http.StatusNotFound, http.StatusConflict:
			// 已由胜者移除，或账号锁被胜者占用。
		default:
			t.Fatalf("unexpected remove status %d", code)
		}
	}
	if okCount != 1 {
		t.Fatalf("successful remove count=%d want 1", okCount)
	}
	if _, ok := pl.Status("u1"); ok {
		t.Fatal("u1 should be removed from pool")
	}
	loaded, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("removed auth file must not be reloaded after concurrent remove: %+v", loaded)
	}
	if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
		t.Fatalf("auth file should stay quarantined, stat err=%v", err)
	}
}

package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// bulkTestPanel 构造带 n 个已落盘账号的 panel。每个账号的 auth 文件都在同一个
// t.TempDir() 下，便于断言「文件被隔离/被保留」。
func bulkTestPanel(t *testing.T, n int) (*Panel, *pool.Pool, []*auth.Auth) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New(filepath.Join(dir, "state.json"))
	accounts := make([]*auth.Auth, 0, n)
	for i := 0; i < n; i++ {
		uid := string(rune('a' + i))
		fp := filepath.Join(dir, "workbuddy-"+uid+".json")
		a := &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", Nickname: "n" + uid, FilePath: fp}
		if err := a.SaveAtomic(); err != nil {
			t.Fatal(err)
		}
		pl.Add(a)
		accounts = append(accounts, a)
	}
	return New(Config{Version: "test", APIKey: "k", Pool: pl}), pl, accounts
}

type bulkRemoveResp struct {
	OK          bool     `json:"ok"`
	Requested   int      `json:"requested"`
	Removed     int      `json:"removed"`
	Skipped     int      `json:"skipped"`
	Failed      int      `json:"failed"`
	RemovedUIDs []string `json:"removed_uids"`
	Results     []struct {
		UID    string `json:"uid"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"results"`
}

func decodeBulk(t *testing.T, body string) bulkRemoveResp {
	t.Helper()
	var out bulkRemoveResp
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return out
}

// remove_all 只删选中账号：未选中的账号、其 auth 文件与池状态都必须原样保留。
func TestAccountsRemoveAllRemovesSelectedOnly(t *testing.T) {
	p, pl, accts := bulkTestPanel(t, 3)
	rec := profilePost(t, p, "/panel/api/accounts/remove_all", `{"uids":["a","c"],"confirm":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBulk(t, rec.Body.String())
	if got.Removed != 2 || got.Failed != 0 || got.Skipped != 0 {
		t.Fatalf("removed=%d failed=%d skipped=%d body=%s", got.Removed, got.Failed, got.Skipped, rec.Body.String())
	}
	for _, uid := range []string{"a", "c"} {
		if _, ok := pl.Status(uid); ok {
			t.Errorf("%s should be gone from the pool", uid)
		}
	}
	if _, ok := pl.Status("b"); !ok {
		t.Error("unselected account b must stay in the pool")
	}
	// auths/ 目录里只剩未被选中的那个账号。
	left, err := auth.LoadDir(filepath.Dir(accts[0].FilePath))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(left) != 1 || left[0].UID != "b" {
		t.Fatalf("auths/ should keep only b, got %+v", left)
	}
	// 隔离文件必须清理干净：残留 .removed-* 会在运维排查时造成"账号还在"的错觉。
	if junk, _ := filepath.Glob(filepath.Join(filepath.Dir(accts[0].FilePath), "workbuddy-*.json.removed-*")); len(junk) != 0 {
		t.Errorf("quarantine files should be cleaned up, got %v", junk)
	}
	// 被删的两个文件必须离开 auths/，否则 LoadDir 会把账号复活。
	for _, i := range []int{0, 2} {
		if _, err := os.Stat(accts[i].FilePath); err == nil {
			t.Errorf("auth file for %s must be removed from auths/", accts[i].UID)
		}
	}
	if _, err := os.Stat(accts[1].FilePath); err != nil {
		t.Errorf("auth file for b must be kept: %v", err)
	}
	// 池状态必须已落盘：新开一个 Pool 从同一 state.json 恢复，不应再看到被删账号。
	reloaded := pool.New(filepath.Join(filepath.Dir(accts[0].FilePath), "state.json"))
	reloaded.RestoreFromSnapshot()
	if _, ok := reloaded.Status("a"); ok {
		t.Error("removed account resurrected after reload")
	}
	if _, ok := reloaded.Status("b"); !ok {
		t.Error("surviving account lost after reload")
	}
}

// confirm 必须等于 uids 条数，否则整批拒绝且池不变。
func TestAccountsRemoveAllRequiresMatchingConfirm(t *testing.T) {
	p, pl, _ := bulkTestPanel(t, 2)
	for _, body := range []string{
		`{"uids":["a","b"]}`,
		`{"uids":["a","b"],"confirm":1}`,
		`{"uids":["a","b"],"confirm":3}`,
	} {
		rec := profilePost(t, p, "/panel/api/accounts/remove_all", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s code=%d want 400", body, rec.Code)
		}
	}
	for _, uid := range []string{"a", "b"} {
		if _, ok := pl.Status(uid); !ok {
			t.Errorf("%s must be untouched after rejected requests", uid)
		}
	}
}

// 作用域红线与批量上限：空 uids / 超上限都不能退化成整池删除。
func TestAccountsRemoveAllRejectsEmptyAndTooMany(t *testing.T) {
	p, pl, _ := bulkTestPanel(t, 1)
	for _, body := range []string{`{}`, `{"uids":[],"confirm":0}`, `{"uids":["  "],"confirm":1}`, `not-json`} {
		if rec := profilePost(t, p, "/panel/api/accounts/remove_all", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s code=%d want 400", body, rec.Code)
		}
	}
	// 超上限：构造 2001 个重复 UID 也要在解码后拒绝（去重前按条数计）。
	many := strings.Repeat(`"a",`, accountsRemoveAllLimit) + `"a"`
	body := `{"uids":[` + many + `],"confirm":` + strconv.Itoa(accountsRemoveAllLimit+1) + `}`
	if rec := profilePost(t, p, "/panel/api/accounts/remove_all", body); rec.Code != http.StatusBadRequest {
		t.Errorf("too many uids code=%d want 400", rec.Code)
	}
	if _, ok := pl.Status("a"); !ok {
		t.Error("account must survive rejected batch")
	}
}

// 锁冲突的账号跳过、不中止整批：批量移除时一个号正在签到，其余号仍应删掉。
func TestAccountsRemoveAllSkipsLockedAccount(t *testing.T) {
	p, pl, accts := bulkTestPanel(t, 2)
	if !p.tryLockAccount("a") {
		t.Fatal("failed to take the account lock")
	}
	defer p.unlockAccount("a")
	rec := profilePost(t, p, "/panel/api/accounts/remove_all", `{"uids":["a","b"],"confirm":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBulk(t, rec.Body.String())
	if got.Removed != 1 || got.Skipped != 1 {
		t.Fatalf("removed=%d skipped=%d body=%s", got.Removed, got.Skipped, rec.Body.String())
	}
	if _, ok := pl.Status("a"); !ok {
		t.Error("locked account must stay in the pool")
	}
	if _, err := os.Stat(accts[0].FilePath); err != nil {
		t.Error("locked account's auth file must stay in place")
	}
	if _, ok := pl.Status("b"); ok {
		t.Error("unlocked account should be removed")
	}
}

// 隔离失败的账号不能出池：否则重启后 LoadDir 会把凭证重新拉回来。
func TestAccountsRemoveAllKeepsAccountWhenIsolationFails(t *testing.T) {
	p, pl, accts := bulkTestPanel(t, 2)
	// 把 a 的文件放进一个只读目录：rename 需要父目录写权限，会以 EACCES 失败，
	// 从而走「文件无法移出 → 不出池」分支。用不存在的父目录不行——那会得到
	// ENOENT，被判成"文件本来就不在"而放行。
	roDir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(roDir, "workbuddy-a.json")
	if err := os.WriteFile(bad, []byte(`{"auth":{"uid":"a","accessToken":"at"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Skipf("chmod not supported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
	if err := os.Rename(bad, bad+".probe"); err == nil {
		_ = os.Rename(bad+".probe", bad)
		t.Skip("directory is still writable (running as root?); cannot exercise the failure path")
	}
	pl.Add(&auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", Nickname: "na", FilePath: bad})
	rec := profilePost(t, p, "/panel/api/accounts/remove_all", `{"uids":["a","b"],"confirm":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBulk(t, rec.Body.String())
	if got.Removed != 1 || got.Failed != 1 {
		t.Fatalf("removed=%d failed=%d body=%s", got.Removed, got.Failed, rec.Body.String())
	}
	if _, ok := pl.Status("a"); !ok {
		t.Error("account whose file could not be isolated must stay in the pool")
	}
	if _, ok := pl.Status("b"); ok {
		t.Error("other accounts must still be removed")
	}
	_ = accts
}

// 已不存在的 UID 报 skipped，不影响其余账号。
func TestAccountsRemoveAllReportsMissingUID(t *testing.T) {
	p, pl, _ := bulkTestPanel(t, 1)
	rec := profilePost(t, p, "/panel/api/accounts/remove_all", `{"uids":["a","ghost"],"confirm":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	got := decodeBulk(t, rec.Body.String())
	if got.Removed != 1 || got.Skipped != 1 {
		t.Fatalf("removed=%d skipped=%d body=%s", got.Removed, got.Skipped, rec.Body.String())
	}
	if _, ok := pl.Status("a"); ok {
		t.Error("a should be removed")
	}
}

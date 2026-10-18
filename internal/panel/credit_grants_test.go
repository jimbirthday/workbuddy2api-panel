package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func grantPanel(t *testing.T, pl *pool.Pool, store *activity.Store) *Panel {
	t.Helper()
	return New(Config{Version: "test", APIKey: "k", Pool: pl, Activity: store})
}

func grantJSON(t *testing.T, p *Panel, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	var out map[string]any
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v body=%s", err, rr.Body)
		}
	}
	return rr, out
}

func TestCreditGrantsAuthAndEmptySnapshot(t *testing.T) {
	p := grantPanel(t, pool.New(""), nil)
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/panel/api/credits/grants", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth code=%d body=%s", rr.Code, rr.Body)
	}

	rr, out := grantJSON(t, p, "/panel/api/credits/grants")
	if rr.Code != http.StatusOK {
		t.Fatalf("empty code=%d body=%s", rr.Code, rr.Body)
	}
	if out["rows"] == nil || len(out["rows"].([]any)) != 0 || out["count"].(float64) != 0 {
		t.Fatalf("empty response must be a successful empty array: %+v", out)
	}
	if out["note"] == "" {
		t.Fatalf("empty response must explain how to populate the snapshot: %+v", out)
	}
}

// top-k 必须与「全量按展示顺序排序后截断」完全一致。此前堆方向反了，
// limit=2 时会返回最旧与最新各一条；这里用确定性/随机两组数据锁住集合与顺序。
func TestCreditGrantTopKMatchesFullSort(t *testing.T) {
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	rows := make([]creditGrantRow, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, creditGrantRow{
			Nickname:  "u" + strconv.Itoa(i%3),
			Name:      "pkg-" + strconv.Itoa(i%5),
			createdAt: base.Add(time.Duration(i) * time.Minute),
			seq:       i,
		})
	}
	for _, limit := range []int{1, 2, 3, 7, 20, 25} {
		var top []creditGrantRow
		for _, row := range rows {
			insertCreditGrantTop(&top, row, limit)
		}
		want := append([]creditGrantRow(nil), rows...)
		sort.SliceStable(want, func(i, j int) bool { return creditGrantBefore(want[i], want[j]) })
		if limit < len(want) {
			want = want[:limit]
		}
		if len(top) != len(want) {
			t.Fatalf("limit=%d len=%d want=%d", limit, len(top), len(want))
		}
		sort.SliceStable(top, func(i, j int) bool { return creditGrantBefore(top[i], top[j]) })
		for i := range want {
			if top[i].seq != want[i].seq || top[i].Name != want[i].Name {
				t.Fatalf("limit=%d row %d: got seq=%d name=%s want seq=%d name=%s (top=%+v)",
					limit, i, top[i].seq, top[i].Name, want[i].seq, want[i].Name, top)
			}
		}
	}
}

func TestCreditGrantsStatusSummaryAndMasking(t *testing.T) {
	pl := pool.New("")
	const uid = "uid-1234567890"
	pl.Add(&auth.Auth{UID: uid, Nickname: "秋叶", Domain: "www.workbuddy.ai"})
	p := grantPanel(t, pl, nil)
	now := time.Now()
	future := now.Add(72 * time.Hour).UnixMilli()
	past := now.Add(-time.Hour).UnixMilli()
	p.rememberCreditSnapshot(uid, "秋叶", "cn", []upstream.CreditPackage{
		{Name: "活动包", Size: 100, Remain: 40, Used: 60, CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339), ExpiresAt: future, EndTime: time.UnixMilli(future).Format(time.RFC3339)},
		{Name: "已过期", Size: 20, Remain: 5, Used: 15, CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339), ExpiresAt: past, EndTime: time.UnixMilli(past).Format(time.RFC3339)},
		{Name: "已用完", Size: 30, Remain: 0, Used: 30, CreatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339), EndTime: "2099-01-01 00:00:00"},
		{Name: "长期包", Size: 10, Remain: 10, Used: 0, CreatedAt: now.Add(-72 * time.Hour).Format(time.RFC3339)},
	})

	rr, out := grantJSON(t, p, "/panel/api/credits/grants")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if out["count"].(float64) != 4 || out["total_size"].(float64) != 160 ||
		out["total_remain"].(float64) != 55 || out["total_used"].(float64) != 105 {
		t.Fatalf("summary=%+v", out)
	}
	if out["fetched_at"] == nil || out["generated_at"] == nil {
		t.Fatalf("timestamps must be explicit: %+v", out)
	}
	rows := out["rows"].([]any)
	status := map[string]string{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		status[row["name"].(string)] = row["status"].(string)
	}
	if status["活动包"] != "active" || status["已过期"] != "expired" ||
		status["已用完"] != "used" || status["长期包"] != "active" {
		t.Fatalf("statuses=%+v", status)
	}
	body := rr.Body.String()
	for _, secret := range []string{uid, "access_token", "refresh_token", "device_token", "Cookie"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked %q: %s", secret, body)
		}
	}
}

func TestCreditGrantsActionAssociationAndSnapshotRetention(t *testing.T) {
	dir := t.TempDir()
	store := activity.New(activity.Config{Path: filepath.Join(dir, activity.FileName()), MaxBytes: 1 << 20})
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := store.Append(activity.Record{Time: base, UID: "uid-abcdefghij", Task: activity.TaskCheckin, Trigger: activity.TriggerManual, OK: true}); err != nil {
		t.Fatal(err)
	}
	pl := pool.New("")
	const uid = "uid-abcdefghij"
	pl.Add(&auth.Auth{UID: uid, Nickname: "小满", Domain: "www.workbuddy.ai"})
	p := grantPanel(t, pl, store)
	p.rememberCreditSnapshot(uid, "小满", "cn", []upstream.CreditPackage{{
		Name: "签到奖励", Size: 10, Remain: 10, CreatedAt: base.Add(12 * time.Second).Format(time.RFC3339),
	}})

	_, out := grantJSON(t, p, "/panel/api/credits/grants")
	row := out["rows"].([]any)[0].(map[string]any)
	action, ok := row["action"].(map[string]any)
	if !ok || action["task"] != "checkin" || action["ok"] != true || action["delta_seconds"].(float64) != 12 {
		t.Fatalf("action link=%+v", row)
	}

	// 后续查询失败不会调用 remember；接口必须继续返回上一份成功快照。
	_, again := grantJSON(t, p, "/panel/api/credits/grants")
	if again["count"].(float64) != 1 || again["fetched_at"] != out["fetched_at"] {
		t.Fatalf("successful snapshot must survive a failed refresh: before=%+v after=%+v", out, again)
	}
}

func TestCreditGrantsSnapshotDropsRemovedAccounts(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "uid-1234567890", Nickname: "甲", Domain: "www.workbuddy.ai"})
	p := grantPanel(t, pl, nil)
	p.rememberCreditSnapshot("uid-1234567890", "甲", "cn", []upstream.CreditPackage{{Name: "包", Size: 1, Remain: 1}})
	p.rememberCreditSnapshot("uid-removed-0000", "旧号", "cn", []upstream.CreditPackage{{Name: "旧包", Size: 9, Remain: 9}})

	_, out := grantJSON(t, p, "/panel/api/credits/grants")
	rows := out["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["nickname"] != "甲" {
		t.Fatalf("removed account must not remain visible: %+v", rows)
	}
}

func TestCreditGrantsLimitAndPaging(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "uid-1234567890", Nickname: "甲", Domain: "www.workbuddy.ai"})
	p := grantPanel(t, pl, nil)
	packs := make([]upstream.CreditPackage, 0, 5)
	for i := 0; i < 5; i++ {
		packs = append(packs, upstream.CreditPackage{Name: "包" + strconv.Itoa(i), Size: 1, Remain: 1})
	}
	p.rememberCreditSnapshot("uid-1234567890", "甲", "cn", packs)
	rr, out := grantJSON(t, p, "/panel/api/credits/grants?limit=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	if out["count"].(float64) != 5 || out["returned"].(float64) != 2 || out["has_more"] != true || out["limit"].(float64) != 2 {
		t.Fatalf("paging=%+v", out)
	}
	if len(out["rows"].([]any)) != 2 {
		t.Fatalf("rows must be truncated after sorting: %+v", out["rows"])
	}
	_, capped := grantJSON(t, p, "/panel/api/credits/grants?limit=999999")
	if capped["limit"].(float64) != creditGrantMaxLimit || len(capped["rows"].([]any)) != 5 {
		t.Fatalf("limit cap=%+v", capped)
	}
}

// top-k 截断必须与「全量 SliceStable 排序后取前 limit」逐行一致，尤其是
// 同创建时间跨账号、同名包的稳定次序；规则改变会让面板最近记录跳动。
func TestCreditGrantTopKMatchesFullStableSort(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	pkg := func(name string, created time.Time) upstream.CreditPackage {
		return upstream.CreditPackage{Name: name, Size: 1, Remain: 1, CreatedAt: created.Format(time.RFC3339)}
	}
	pkgs := []upstream.CreditPackage{
		pkg("same-time", base),
		pkg("same-time", base),
		pkg("old", base.Add(-48*time.Hour)),
		pkg("new", base.Add(time.Hour)),
		pkg("middle", base.Add(-time.Hour)),
	}
	for _, limit := range []int{1, 2, 3, 4, 5, 8} {
		top := make([]creditGrantRow, 0, limit)
		all := make([]creditGrantRow, 0, len(pkgs))
		for i, p := range pkgs {
			row := creditGrantRow{Name: p.Name, Nickname: "甲", createdAt: parseGrantTime(p.CreatedAt), seq: i}
			insertCreditGrantTop(&top, row, limit)
			all = append(all, row)
		}
		// 旧实现每轮请求按 map 迭代顺序收集快照（账号顺序本来不稳定），随后用
		// SliceStable 在 createdAt/nickname/name 上排序；同键行保留的是本轮
		// 收集顺序。比较集合内容即可，不能假设输入顺序等于旧实现的收集顺序。
		sort.SliceStable(all, func(i, j int) bool {
			return creditGrantBefore(creditGrantRow{
				Name: all[i].Name, Nickname: all[i].Nickname, createdAt: all[i].createdAt,
			}, creditGrantRow{Name: all[j].Name, Nickname: all[j].Nickname, createdAt: all[j].createdAt})
		})
		if len(all) > limit {
			all = all[:limit]
		}
		sort.SliceStable(top, func(i, j int) bool { return creditGrantBefore(top[i], top[j]) })
		if len(top) != len(all) {
			t.Fatalf("limit=%d len=%d want %d", limit, len(top), len(all))
		}
		for i := range top {
			if top[i].Name != all[i].Name || top[i].Nickname != all[i].Nickname || !top[i].createdAt.Equal(all[i].createdAt) {
				t.Fatalf("limit=%d row %d top=%+v want %+v", limit, i, top[i], all[i])
			}
		}
	}
}

// 池为空时不清理快照：启动早期账号尚未导入完成，不能把刚写入的快照当成
// 已移除账号删除；一旦池中确有账号，才过滤并清理已移除的 UID。
func TestCreditGrantsEmptyPoolKeepsSnapshotUntilAccountsLoad(t *testing.T) {
	pl := pool.New("")
	p := grantPanel(t, pl, nil)
	p.rememberCreditSnapshot("uid-1234567890", "甲", "cn", []upstream.CreditPackage{{Name: "包", Size: 1, Remain: 1}})
	if _, out := grantJSON(t, p, "/panel/api/credits/grants"); out["count"].(float64) != 1 {
		t.Fatalf("empty pool must not drop snapshot: %+v", out)
	}
	pl.Add(&auth.Auth{UID: "uid-9999999999", Nickname: "乙", Domain: "www.workbuddy.ai"})
	if _, out := grantJSON(t, p, "/panel/api/credits/grants"); out["count"].(float64) != 0 {
		t.Fatalf("removed uid should be filtered once pool has accounts: %+v", out)
	}
}

// EndTime 只有上游显式标 ExpiryCycle 时才是周期边界；旧数据/兼容分支里
// 非零 ExpiresAt 缺失但 EndTime 是真实失效时间，不能被误标成 cycle。
func TestCreditGrantExpiryCycleComesFromUpstreamFlag(t *testing.T) {
	end := time.Now().Add(24 * time.Hour)
	iso := end.Format(time.RFC3339)
	if _, _, _, kind := grantExpiry(upstream.CreditPackage{EndTime: iso}); kind != "" {
		t.Fatalf("plain end time kind=%q want empty", kind)
	}
	if _, _, _, kind := grantExpiry(upstream.CreditPackage{EndTime: iso, ExpiryCycle: true}); kind != "cycle" {
		t.Fatalf("cycle flag kind=%q want cycle", kind)
	}
}

package panel

import (
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const creditGrantsRoutePattern = "GET /panel/api/credits/grants"

const (
	grantActionBefore = 2 * time.Hour
	grantActionAfter  = 10 * time.Minute
	// 470 账号 × 每号最多 100 个包可达数万行；面板只需要最近的一屏，
	// 服务端先排序再截断，避免把整份快照序列化进浏览器。
	creditGrantDefaultLimit = 200
	creditGrantMaxLimit     = 1000
)

type creditAccountSnapshot struct {
	UID      string
	Nickname string
	Realm    string
	Packages []upstream.CreditPackage
	// created 与 Packages 同下标，保存预解析的创建时间；oldest/newest 是本号
	// 包的时间边界。积分包在 remember 后不再修改，读路径因此无需重复解析
	// CreatedAt，也不需要在每次请求中先遍历全部包求时间范围。
	created   []time.Time
	oldest    time.Time
	newest    time.Time
	FetchedAt time.Time
}

// rememberCreditSnapshot 更新单个账号的积分包只读快照。只有 /packages 成功
// 查询后才调用；失败分支保留旧快照，读取侧通过 fetched_at 明示新鲜度。
func (p *Panel) rememberCreditSnapshot(uid, nickname, realm string, packages []upstream.CreditPackage) {
	uid = strings.TrimSpace(uid)
	if p == nil || uid == "" {
		return
	}
	p.creditSnapMu.Lock()
	defer p.creditSnapMu.Unlock()
	if p.creditSnap == nil {
		p.creditSnap = make(map[string]creditAccountSnapshot)
	}
	created := make([]time.Time, len(packages))
	var oldest, newest time.Time
	for i := range packages {
		created[i] = parseGrantTime(packages[i].CreatedAt)
		if created[i].IsZero() {
			continue
		}
		if oldest.IsZero() || created[i].Before(oldest) {
			oldest = created[i]
		}
		if newest.IsZero() || created[i].After(newest) {
			newest = created[i]
		}
	}
	p.creditSnap[uid] = creditAccountSnapshot{
		UID:       uid,
		Nickname:  nickname,
		Realm:     realm,
		Packages:  append([]upstream.CreditPackage(nil), packages...),
		created:   created,
		oldest:    oldest,
		newest:    newest,
		FetchedAt: time.Now(),
	}
}

// forgetCreditSnapshot 在账号移除时丢弃对应快照，避免历史账号长期占用内存。
func (p *Panel) forgetCreditSnapshot(uid string) {
	uid = strings.TrimSpace(uid)
	if p == nil || uid == "" {
		return
	}
	p.creditSnapMu.Lock()
	delete(p.creditSnap, uid)
	p.creditSnapMu.Unlock()
}

func (p *Panel) creditGrantsRoutes(mux *http.ServeMux) {
	mux.HandleFunc(creditGrantsRoutePattern, p.withAuth(p.creditGrants))
}

type creditGrantAction struct {
	Task         string `json:"task"`
	TS           string `json:"ts"`
	OK           bool   `json:"ok"`
	DeltaSeconds int64  `json:"delta_seconds"`
}

type creditGrantRow struct {
	// UID 只是脱敏展示键（uid8 或短 UID 的掩码），不是完整账号标识。
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	Realm       string `json:"realm"`
	Name        string `json:"name"`
	Product     string `json:"product"`
	GrantReason string `json:"grant_reason"`
	Size        int64  `json:"size"`
	Remain      int64  `json:"remain"`
	Used        int64  `json:"used"`
	CreatedAt   string `json:"created_at,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	ExpireISO   string `json:"expire_iso,omitempty"`
	// ExpiryKind 区分真失效时间与周期边界：上游 EndTime 在 DeductionEndTime
	// 缺失时会回落到 CycleEndTime，后者只是额度重置点，不是“用不完就没了”。
	// cycle 时前端只显示边界，不把它标成到期。
	ExpiryKind string             `json:"expiry_kind,omitempty"`
	NoExpiry   bool               `json:"no_expiry"`
	DaysLeft   int                `json:"days_left"`
	Status     string             `json:"status"`
	Action     *creditGrantAction `json:"action,omitempty"`
	createdAt  time.Time
	// seq 是快照内稳定插入序号，仅用于 createdAt 相同时保持原 SliceStable 语义
	// （同账号同名的包先出现者在前）；不进入 JSON。
	seq int
}

// creditGrants 返回积分包发放明细。数据完全来自 /packages 成功后留下的内存
// 快照，不逐账号访问上游；没有快照时返回 200 空数组，并在 note 里提示先刷新。
func (p *Panel) creditGrants(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	snaps := p.creditSnapshots()
	if len(snaps) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"generated_at": now.Format(time.RFC3339),
			"rows":         []creditGrantRow{},
			"count":        0,
			"total_size":   int64(0),
			"total_remain": int64(0),
			"total_used":   int64(0),
			"note":         "尚未刷新积分包：请先在积分构成页执行一次刷新",
		})
		return
	}

	rows := make([]creditGrantRow, 0, 64)
	var fetchedAt, oldest, newest time.Time
	for si := range snaps {
		snap := snaps[si]
		if snap.FetchedAt.After(fetchedAt) {
			fetchedAt = snap.FetchedAt
		}
		if !snap.oldest.IsZero() && (oldest.IsZero() || snap.oldest.Before(oldest)) {
			oldest = snap.oldest
		}
		if !snap.newest.IsZero() && (newest.IsZero() || snap.newest.After(newest)) {
			newest = snap.newest
		}
	}
	actions := p.grantActionIndex(oldest, newest)

	var totalSize, totalRemain, totalUsed int64
	limit := creditGrantLimit(r)
	// 只保留排序后的前 limit 行。旧实现先收集全部包、整表排序再截断，数万行时
	// 排序成本与快照体积同阶；这里用固定大小 top-k（插入/替换最差 limit 次），
	// 输出仍按 createdAt desc + nickname + name 的唯一顺序，与整表排序截断一致。
	top := make([]creditGrantRow, 0, limit)
	total := 0
	for si := range snaps {
		snap := snaps[si]
		for pi, pkg := range snap.Packages {
			var created time.Time
			if pi < len(snap.created) {
				created = snap.created[pi]
			} else {
				created = parseGrantTime(pkg.CreatedAt)
			}
			expires, expireISO, noExpiry, expiryKind := grantExpiry(pkg)
			status, daysLeft := grantStatus(pkg, expires, noExpiry, now)
			row := creditGrantRow{
				UID:        grantUID(snap.UID),
				Nickname:   snap.Nickname,
				Realm:      snap.Realm,
				Name:       firstNonEmpty(pkg.Name, pkg.PackageCode, "Package"),
				Product:    firstNonEmpty(pkg.SubProductName, pkg.SubProductCode),
				Size:       pkg.Size,
				Remain:     pkg.Remain,
				Used:       pkg.Used,
				CreatedAt:  pkg.CreatedAt,
				ExpiresAt:  pkg.ExpiresAt,
				ExpireISO:  expireISO,
				ExpiryKind: expiryKind,
				NoExpiry:   noExpiry,
				DaysLeft:   daysLeft,
				Status:     status,
			}
			if !created.IsZero() {
				// 活动历史落盘时 UID 已收敛为 uid8，索引也按同一口径查；
				// 完整 UID 只存在于本次内存快照里，不参与查询键拼接。
				row.Action = grantActionAt(actions[logfmt.UID8(snap.UID)], created)
			}
			row.createdAt = created
			row.seq = total
			insertCreditGrantTop(&top, row, limit)
			total++
			totalSize += pkg.Size
			totalRemain += pkg.Remain
			totalUsed += pkg.Used
		}
	}
	rows = top
	// 堆只保证前 limit 集合正确；输出顺序需按展示比较器还原。这里只排序
	// limit 行，不再对全部积分包排序。
	sort.SliceStable(rows, func(i, j int) bool { return creditGrantBefore(rows[i], rows[j]) })
	out := map[string]any{
		"ok":           true,
		"generated_at": now.Format(time.RFC3339),
		"rows":         rows,
		"count":        total,
		"returned":     len(rows),
		"limit":        limit,
		"has_more":     total > len(rows),
		"total_size":   totalSize,
		"total_remain": totalRemain,
		"total_used":   totalUsed,
	}
	if !fetchedAt.IsZero() {
		out["fetched_at"] = fetchedAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// creditGrantLimit 解析 limit；非法值回落到默认值，超上限钳到上限。解析错误
// 不在读路径上返回 400：快照视图的价值是“立即可看”，非法参数只做安全收窄。
func creditGrantLimit(r *http.Request) int {
	limit := creditGrantDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > creditGrantMaxLimit {
		limit = creditGrantMaxLimit
	}
	return limit
}

// creditGrantBefore 是展示排序：创建时间新的在前，同时间按昵称/包名稳定排序。
func creditGrantBefore(a, b creditGrantRow) bool {
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.After(b.createdAt)
	}
	if a.Nickname != b.Nickname {
		return a.Nickname < b.Nickname
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.seq < b.seq
}

// insertCreditGrantTop 维护按展示顺序前 limit 行的小顶堆（堆顶是当前最差行）。
// 只有比堆顶更靠前的行才替换，整体 O(行数 × log limit)，避免把所有积分包
// 完整排序后再截断。
func insertCreditGrantTop(top *[]creditGrantRow, row creditGrantRow, limit int) {
	if limit <= 0 {
		return
	}
	h := *top
	if len(h) < limit {
		h = append(h, row)
		*top = h
		upCreditGrantHeap(h, len(h)-1)
		return
	}
	if !creditGrantHeapBetter(row, h[0]) {
		return
	}
	h[0] = row
	downCreditGrantHeap(h, 0)
}

// creditGrantHeapBetter 与展示排序 creditGrantBefore 同口径：创建时间新的
// 在前，同时间按昵称/包名稳定排序。seq 只作最后兜底，避免完全相同的行
// 在堆里互相替换造成集合抖动。
func creditGrantHeapBetter(a, b creditGrantRow) bool {
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.After(b.createdAt)
	}
	if a.Nickname != b.Nickname {
		return a.Nickname < b.Nickname
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.seq < b.seq
}

func upCreditGrantHeap(h []creditGrantRow, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		// 堆顶必须是最差行：父节点比子节点更好时，把更差的子节点上浮。
		if !creditGrantHeapBetter(h[parent], h[i]) {
			break
		}
		h[i], h[parent] = h[parent], h[i]
		i = parent
	}
}

func downCreditGrantHeap(h []creditGrantRow, i int) {
	for {
		left := 2*i + 1
		if left >= len(h) {
			return
		}
		worst := left
		right := left + 1
		// 选更差的子节点与当前节点比较。
		if right < len(h) && creditGrantHeapBetter(h[worst], h[right]) {
			worst = right
		}
		// 当前节点比更差的子节点更好时必须下沉（堆顶保持最差）。
		if !creditGrantHeapBetter(h[i], h[worst]) {
			return
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}

func (p *Panel) creditSnapshots() []creditAccountSnapshot {
	p.creditSnapMu.RLock()
	if len(p.creditSnap) == 0 {
		p.creditSnapMu.RUnlock()
		return nil
	}
	snaps := make([]creditAccountSnapshot, 0, len(p.creditSnap))
	for _, snap := range p.creditSnap {
		snaps = append(snaps, snap)
	}
	p.creditSnapMu.RUnlock()

	allowed := make(map[string]struct{})
	poolReady := false
	if p.cfg.Pool != nil {
		for _, row := range p.cfg.Pool.UsageRows() {
			allowed[row.UID] = struct{}{}
		}
		poolReady = len(allowed) > 0
	}
	out := make([]creditAccountSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		// 池为空通常表示导入/启动尚未完成，此时不能把所有快照都当成“已移除”；
		// 只有确实加载到账号后才启用清理，避免误删刚写入的快照。
		if poolReady {
			if _, ok := allowed[snap.UID]; !ok {
				// 只在读取时过滤不够：账号通过 SyncToDir 等路径移除后，map 会一直
				// 留着它的包切片。读取侧顺手清掉；锁顺序是先释放快照读锁，再查池，
				// 最后短暂拿写锁删除，避免和 rememberCreditSnapshot 交叉等待。
				p.creditSnapMu.Lock()
				if cur, exists := p.creditSnap[snap.UID]; exists && cur.UID == snap.UID {
					delete(p.creditSnap, snap.UID)
				}
				p.creditSnapMu.Unlock()
				continue
			}
		}
		// Packages/created 在 rememberCreditSnapshot 中写入后视为不可变；
		// 读路径只返回浅拷贝，避免每次请求再复制整份包切片。
		out = append(out, snap)
	}
	return out
}

func grantUID(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "-"
	}
	if len(uid) <= 8 {
		if len(uid) <= 2 {
			return "***"
		}
		return uid[:2] + "****"
	}
	return logfmt.UID8(uid)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseGrantTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", v, upstream.SoftRateResetLoc()); err == nil {
		return t
	}
	return time.Time{}
}

func grantExpiry(pkg upstream.CreditPackage) (time.Time, string, bool, string) {
	if pkg.ExpiresAt > 0 {
		t := time.UnixMilli(pkg.ExpiresAt)
		return t, pkg.EndTime, false, ""
	}
	if t := parseGrantTime(pkg.EndTime); !t.IsZero() {
		// EndTime 的字段来源由上游显式标记：只有 ExpiryCycle 才是周期边界，
		// 不能仅凭 ExpiresAt 缺失推断——旧快照或兼容数据可能只有墙钟 EndTime。
		kind := ""
		if pkg.ExpiryCycle {
			kind = "cycle"
		}
		return t, pkg.EndTime, false, kind
	}
	return time.Time{}, pkg.EndTime, pkg.EndTime == "", ""
}

func grantStatus(pkg upstream.CreditPackage, expires time.Time, noExpiry bool, now time.Time) (string, int) {
	if !noExpiry && !expires.IsZero() && !expires.After(now) {
		return "expired", 0
	}
	if pkg.Remain <= 0 {
		return "used", 0
	}
	if noExpiry || expires.IsZero() {
		return "active", -1
	}
	days := int(math.Ceil(expires.Sub(now).Hours() / 24))
	if days < 0 {
		days = 0
	}
	return "active", days
}

type grantActionEntry struct {
	at   time.Time
	task activity.Task
	ok   bool
}

// grantActionIndex 读取活动历史中签到/活跃上报的真实尝试，按 uid 建时间索引。
// 读取失败只退化成“没有关联动作”，不影响积分明细本身。
func (p *Panel) grantActionIndex(oldest, newest time.Time) map[string][]grantActionEntry {
	if p.cfg.Activity == nil || oldest.IsZero() {
		return nil
	}
	until := newest.Add(grantActionAfter)
	start := oldest.Add(-grantActionBefore)
	if until.Before(start) {
		return nil
	}
	size, mod := p.cfg.Activity.Signature()
	key := start.Format(time.RFC3339Nano) + "|" + until.Format(time.RFC3339Nano) + "|" +
		strconv.FormatInt(size, 10) + "|" + strconv.FormatInt(mod.UnixNano(), 10)
	p.grantActionMu.Lock()
	if p.grantActionCache != nil && p.grantActionKey == key && time.Since(p.grantActionAt) < grantActionCacheTTL {
		index := p.grantActionCache
		p.grantActionMu.Unlock()
		return index
	}
	p.grantActionMu.Unlock()

	res, err := p.cfg.Activity.Query(activity.Filter{Since: start, Until: until, Limit: 1000})
	if err != nil {
		log.Printf("panel: 读取积分关联动作失败: %v", err)
		return nil
	}
	index := make(map[string][]grantActionEntry)
	for _, rec := range res.Records {
		if rec.Task != activity.TaskCheckin && rec.Task != activity.TaskActivity {
			continue
		}
		index[rec.UID] = append(index[rec.UID], grantActionEntry{at: rec.Time, task: rec.Task, ok: rec.OK})
	}
	for uid := range index {
		sort.SliceStable(index[uid], func(i, j int) bool { return index[uid][i].at.Before(index[uid][j].at) })
	}
	p.grantActionMu.Lock()
	p.grantActionKey, p.grantActionAt, p.grantActionCache = key, time.Now(), index
	p.grantActionMu.Unlock()
	return index
}

// grantActionCacheTTL 是派生索引的兜底有效期。活动文件签名变化会立即失效；
// TTL 只防“mtime 精度不足/时钟回拨”等极端情况下的陈旧缓存。
const grantActionCacheTTL = 30 * time.Second

func grantActionAt(items []grantActionEntry, at time.Time) *creditGrantAction {
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.at.After(at.Add(grantActionAfter)) {
			continue
		}
		if item.at.Before(at.Add(-grantActionBefore)) {
			break
		}
		return &creditGrantAction{
			Task:         string(item.task),
			TS:           item.at.Format(time.RFC3339),
			OK:           item.ok,
			DeltaSeconds: int64(math.Round(at.Sub(item.at).Seconds())),
		}
	}
	return nil
}

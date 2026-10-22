// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	// enterpriseToken 记录上次解析 EnterpriseID 所用的 accessToken。refresh 后
	// token 可能切换到个人空间或另一企业空间；若只按「EnterpriseID 非空优先」
	// 就会一直沿用旧租户，导致企业额度端点、X-Enterprise-Id 与任务门控错位。
	enterpriseToken string
	// enterpriseFromClaim 记录当前 EnterpriseID 是否来自 JWT claim。token 变化且
	// 新 token 没有企业 claim 时，只有 claim 来源的旧值必须清空；文件/API 显式
	// 给出的企业 ID（旧版凭据常见）在初次加载时仍可作为回退。
	enterpriseFromClaim bool
	Nickname            string
	FilePath            string // 来源文件；refresh 后原子写回此处

	// Weight 手动调度权重（1-100，1 = 不干预）。与 hub 语义一致：>1 的账号优先
	// 接单，多个高权重按权重比分流；落盘在 auth 文件的 weight 键，导出/导入照常
	// 携带。非法/越界值在 Parse 时收敛到 [1,100]，坏数据不会让账号加载失败。
	Weight int `json:"-"`
	// Note 账号备注（面板自填的「批量」「自用」等标记）。纯展示，不参与任何
	// 路由判断；随凭证档持久化，重启后保留。超长截断到 200 字符。
	Note string `json:"-"`

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争（go test -race 实证）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// ExpiresAtValue 加锁读取 token 过期时刻（Unix 秒）。池侧的「凭证新鲜度」
// 判定（见 pool.upsertLocked）需要它：refresh 在锁内改写 ExpiresAt，直读是竞争。
func (a *Auth) ExpiresAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExpiresAt
}

// SupersededBy 报告 other 是否携带比 a **更新**的凭证。
//
// 用途：同一个 UID 被再次 upsert 进池时（导入 cockpit 导出、重复登录），新对象
// 可能来自一份更旧的导出文件。若无条件用新对象替换池内对象，会把运行期已经
// refresh 出来的新 token 覆盖回旧 token——下一次请求直接 401，且旧 refresh token
// 可能已被 rotate 失效。这里只做纯比较，不修改任何状态（调用方仍持池锁：
// 两把锁的嵌套顺序恒为 pool.mu → auth.mu，与其它路径一致）。
//
// 判据（保守，只在证据充分时判定「新对象更旧」）：
//   - 两者 token 相同 → 无差别，返回 false；
//   - other 的过期时间严格早于 a，且 a 已过期时间晚于现在（a 的 token 还活着）
//     → other 是旧凭证，返回 true。
const credentialClockSkew int64 = 60 // 秒

func (a *Auth) SupersededBy(other *Auth) bool {
	if a == nil || other == nil {
		return false
	}
	// 固定加锁顺序避免 ABBA：先取两边的快照值，再比较。
	a.mu.Lock()
	aAT, aExp := a.AccessToken, a.ExpiresAt
	a.mu.Unlock()
	other.mu.Lock()
	bAT, bExp := other.AccessToken, other.ExpiresAt
	other.mu.Unlock()
	if aAT == "" || bAT == "" || aAT == bAT {
		return false
	}
	if aExp <= 0 || bExp <= 0 {
		return false
	}
	// 留 1 分钟余量：同一批导出的 token 之间可能有秒级差异，不值得判成旧凭证；
	// 真正「更旧的导出」通常差数小时到数天。
	return bExp+credentialClockSkew < aExp && aExp > time.Now().Unix()
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// EnterpriseIDValue 加锁读取企业 ID。契据文件里的 enterpriseId 可能为空，而企业
// 空间只在 accessToken 的 JWT claim 里（登录时切到企业空间换来的 token）。这里在
// 首次读取时懒回填，并按需重新解析新 token，保证所有调用方看到同一身份。
func (a *Auth) EnterpriseIDValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enterpriseIDLocked()
}

// ReparseEnterpriseID 在 refresh 写回新 token 后重算企业身份。调用方必须
// 已持有 a.mu（RefreshToken 的写回段就是这样）；不会再次加锁。
//
// 不能在锁内调用 EnterpriseIDValue：sync.Mutex 不可重入。refresh 的写回段
// 用本函数把「新 token 是否还有企业 claim」与 token 写入放在同一临界区，
// 避免出站头/额度端点在下一次请求里读到旧租户。
func (a *Auth) ReparseEnterpriseID() {
	if a == nil {
		return
	}
	// refresh 响应是身份权威来源：新 token 没有企业 claim 就说明已切到个人空间，
	// 即使旧文件里留有 explicit enterpriseId 也必须清空，避免串租户。
	claim := jwtEnterpriseID(a.AccessToken)
	a.EnterpriseID = claim
	a.enterpriseFromClaim = claim != ""
	a.enterpriseToken = a.AccessToken
}

// InitEnterpriseIdentity 在构造 Auth 后、首次入池前按当前 token 初始化企业身份。
// 与 Parse 同规则：JWT 有 claim 时 claim 权威；无 claim 时保留调用方给出的显式值
// （兼容旧版凭据文件/登录 API 只提供 enterpriseId 的形态）。不需要持锁（对象尚未
// 发布），导入与登录路径应在 Pool.Add 之前调用。
func (a *Auth) InitEnterpriseIdentity() {
	if a == nil {
		return
	}
	a.applyTokenEnterpriseIDLocked(false)
	a.enterpriseToken = a.AccessToken
}

// enterpriseIDLocked 企业 ID 的无锁实现：显式字段优先；为空时从 accessToken 的
// JWT payload 读取 enterprise_id/enterpriseId。只解码不验签——该值用于选中正确的
// 出站端点和任务门控，不是信任边界；鉴权仍由上游校验签名。
func (a *Auth) enterpriseIDLocked() string {
	// accessToken 变化即重新解析。有 claim 时 claim 权威（覆盖文件/API 的显式值）；
	// 无 claim 时，若旧值来自 claim 则清空（企业→个人），否则保留显式值（旧版
	// 凭据文件/登录 API 只给 enterpriseId 的形态）。
	if a.enterpriseToken != a.AccessToken {
		a.applyTokenEnterpriseIDLocked(false)
		a.enterpriseToken = a.AccessToken
	}
	return strings.TrimSpace(a.EnterpriseID)
}

// applyTokenEnterpriseIDLocked 按当前 accessToken 的企业 claim 更新身份。
// authoritative=false 用于首次加载：无 claim 时保留显式值；authoritative=true
// 用于 refresh：无 claim 即清空。调用方必须已持有 a.mu。
func (a *Auth) applyTokenEnterpriseIDLocked(authoritative bool) {
	claim := jwtEnterpriseID(a.AccessToken)
	switch {
	case claim != "":
		a.EnterpriseID = claim
		a.enterpriseFromClaim = true
	case authoritative || a.enterpriseFromClaim:
		a.EnterpriseID = ""
		a.enterpriseFromClaim = false
	default:
		// 首次加载且没有 claim：保留文件/登录 API 的显式 EnterpriseID。
		a.enterpriseFromClaim = false
	}
}

// jwtEnterpriseID 从 JWT payload 提取企业 ID。畸形 token/缺 claim 返回空串。
func jwtEnterpriseID(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var claims struct {
		EnterpriseID      string `json:"enterprise_id"`
		EnterpriseIDCamel string `json:"enterpriseId"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if id := strings.TrimSpace(claims.EnterpriseID); id != "" {
		return id
	}
	return strings.TrimSpace(claims.EnterpriseIDCamel)
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled 报告 global realm 路由开关当前状态（测试/运维观测）。
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// SaveRealmPatch 只把 realm 键补进现有文件，保留插件写入的其余字段
// （version / account.email / account.avatar / extra 等）。
//
// 为什么不能复用 SaveAtomic：saveLocked 用白名单重建 auth/account/device_token
// 三个对象，任何未知键都会被永久删除，扁平形文件还会被整体改成嵌套形。存量
// 迁移（realm 是后加字段，老文件普遍缺失）如果走白名单重建，等于在升级时静默
// 丢数据。这里改为「解码成 map → 只写 realm → 原子写回」。
func (a *Auth) SaveRealmPatch() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", a.FilePath, err)
	}
	// 嵌套形（插件输出）：写 auth.realm；扁平形：写顶层 realm。两种形态都写时
	// 以嵌套形为准（解析端同口径）。
	if authObj, ok := doc["auth"].(map[string]any); ok {
		authObj["realm"] = a.realm
		doc["auth"] = authObj
	} else {
		doc["realm"] = a.realm
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor 显式写入 realm 标识（包外登录路径使用：panel login 已知用户选了
// global，直接落盘 realm=global，不依赖 domain 后缀推断）。realm 需为 cn/global，
// 非法值报错（防写脏）。返回是否发生变更。
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// IsEnterprise 报告账号是否为企业版（auth 文件带非空 enterpriseId）。
//
// 企业版**没有个人成长体系**：签到 / 成长任务 / 连登管家 / 猫猫旅行 / 夜猫子
// 在上游一律被拒——实测（2026-10-07，同一时刻 A/B 对照）：
//   - POST /v2/billing/meter/daily-checkin                → 400 code 10001「企业账号不支持该操作」
//   - POST /billing/meter/claim-gift / claim-compensation → 400 code 10001 同上
//   - GET  /activity/growth/{streak,buddy/info,heatmap,lottery/summary}
//     与 GET /v2/activity/growth/tasks                    → 403「growth system is only available for personal users」
//
// 因此引用处（scheduler 五类任务 + panel 成长任务/签到入口）必须与 IsGlobal 门控
// 并列跳过，避免每日对注定失败的端点空发请求、面板按钮必然报错。
//
// 不受影响的能力：选号派发、保活（token 刷新）、额度查询（企业额度改走
// /v2/billing/meter/get-enterprise-user-usage，见 upstream.enterpriseResource）。
//
// EnterpriseID 会在 RefreshToken 换到新 token 时回填，故读取必须加锁。
func (a *Auth) IsEnterprise() bool { return a.EnterpriseIDValue() != "" }

// WeightValue 返回归一化后的手动调度权重：1 = 不干预，2-100 = 更高优先级。
func (a *Auth) WeightValue() int { return normalizeWeight(a.Weight) }

// SetWeight 设置手动调度权重（1-100），越界值收敛而非报错。
func (a *Auth) SetWeight(weight int) { a.Weight = normalizeWeight(weight) }

// SetNote 设置账号备注（纯展示，截断到 200 字符）。
func (a *Auth) SetNote(note string) { a.Note = normalizeNote(note) }

// NicknameValue 加锁读取昵称。池侧的状态投影（Status/Nicknames/UsageRows/
// RealmLabels 及日志标签）都在持池锁的情况下读昵称，而 SetNickname 在 a.mu
// 内改写它——不加锁直读是真实的数据竞争（Go 字符串是两个字的非原子写，
// 读到半写值会得到错乱字符串，go test -race 可稳定复现）。
func (a *Auth) NicknameValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Nickname
}

// ProfileSnapshot 返回面板展示需要的账号资料快照（weight/note + 持久化标签）。
// 读取 weight/note 也走 a.mu：SetProfile 会在锁内改写它们，避免数据竞争。
func (a *Auth) ProfileSnapshot() (weight int, note, nickname, realm string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return normalizeWeight(a.Weight), a.Note, a.Nickname, a.realmLocked()
}

// normalizeWeight 把任意权重收敛到 [1,100]；0/负数/异常值回落 1（不干预）。
func normalizeWeight(w int) int {
	if w < 1 {
		return 1
	}
	if w > 100 {
		return 100
	}
	return w
}

// normalizeNote 去首尾空白并截断到 200 字符（按 rune，避免切坏中文）。
func normalizeNote(note string) string {
	note = strings.TrimSpace(note)
	if len(note) <= 200 {
		return note
	}
	r := []rune(note)
	if len(r) > 200 {
		r = r[:200]
	}
	return string(r)
}

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
				Weight       int    `json:"weight"`
				Note         string `json:"note"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用；手写时无需嵌进 auth 对象）。
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			realm:        n.Auth.Realm,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
			DeviceToken:  n.DeviceToken,
			Weight:       normalizeWeight(n.Account.Weight),
			Note:         normalizeNote(n.Account.Note),
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			DeviceToken  string `json:"device_token"`
			Weight       int    `json:"weight"`
			Note         string `json:"note"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
			DeviceToken:  f.DeviceToken,
			Weight:       normalizeWeight(f.Weight),
			Note:         normalizeNote(f.Note),
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	// 企业号凭据文件的 enterpriseId 常为空，真实标识只在 JWT claim 里；Parse 时
	// 一并补齐，让面板、调度器和出站头从加载后的第一刻就是同一口径。JWT 有企业
	// claim 时以 claim 为准（显式值可能是旧租户）；无 claim 时保留显式值，兼容
	// 旧版凭据文件。refresh 换 token 后的规则见 ReparseEnterpriseID。
	a.InitEnterpriseIdentity()
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveLocked()
}

// saveLocked 是 SaveAtomic 的持锁主体。调用方必须已持有 a.mu；新增的
// SetProfile 需要「改字段 + 立即落盘」在同一把锁里完成，避免与 RefreshToken
// 并发时把半更新状态写回。
func (a *Auth) saveLocked() error {
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
			"weight":       normalizeWeight(a.Weight),
			"note":         normalizeNote(a.Note),
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount 权限问题的典型现场：容器内 app 用户（uid 10001）
		// 对宿主机挂载目录无写权限。给出可操作指引而不是裸 syscall 错误。
		msg := fmt.Sprintf("写入 %s 失败: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选：" +
				"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose 设 user: \"0:0\" 以 root 运行）"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// SetProfile 原子更新面板可编辑的账号资料（nil 表示该字段不变）并落盘。
// 只允许改 weight/note：这两个字段不参与鉴权，但必须与 token 刷新共用 a.mu，
// 否则 refresh 写回时可能用旧值覆盖刚保存的备注/权重。
func (a *Auth) SetProfile(weight *int, note *string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if weight != nil {
		a.Weight = normalizeWeight(*weight)
	}
	if note != nil {
		a.Note = normalizeNote(*note)
	}
	return a.saveLocked()
}

// NormalizeProfileInput 返回面板写入前应采用的 weight/note 规范值。SetProfile
// 在池镜像与 auth 文件中必须使用同一规则：仅 TrimSpace 会让 state.json/概览
// 短暂保存超长备注，而 auth 文件与 UI 已按 200 字符截断。
func NormalizeProfileInput(weight *int, note *string) (*int, *string) {
	var w *int
	if weight != nil {
		v := normalizeWeight(*weight)
		w = &v
	}
	var n *string
	if note != nil {
		v := normalizeNote(*note)
		n = &v
	}
	return w, n
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				// 用 patch 写回而不是 SaveAtomic：后者按白名单重建整份文件，
				// 会把插件写入的未知字段（version/email/extra…）静默删掉。
				if err := a.SaveRealmPatch(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

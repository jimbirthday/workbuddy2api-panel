package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// cockpit 导出可能在 account/扁平行任一位置携带企业标识；两种命名都接受，
	// 否则企业号导入后会退化成个人号（签到、成长任务、个人额度端点都会走错）。
	EnterpriseID      string `json:"enterpriseId"`
	EnterpriseIDSnake string `json:"enterprise_id"`
	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	TokenType         string `json:"token_type"`
	ExpiresAt         int64  `json:"expires_at"`
	Domain            string `json:"domain"`
	DosageNotify      string `json:"dosage_notify_code"`
	PaymentType       string `json:"payment_type"`
	Status            string `json:"status"`
	UsageUpdatedAt    int64  `json:"usage_updated_at"`
	LastCheckin       int64  `json:"last_checkin_time"`
	CheckinStreak     int    `json:"checkin_streak"`
	CreatedAt         int64  `json:"created_at"`
	LastUsed          int64  `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, dry_run, overwrite, total, added, updated, skipped, invalid, errors}。
// 同 UID 文件默认跳过，只有 overwrite=true 才替换；dryRun=true 只做解析和
// 分类，不写盘、不入池、不触发上游签到/激活。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, fileHeader, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	// importMaxFileSize 上传文件总量上限（8MiB）：cockpit tools 导出为纯 JSON
	// 文本，账号量个位/十位级远小于此值。先用 fileHeader.Size 预判（multipart
	// 已缓冲到内存/临时文件），再对实际读取套 LimitReader 兜底，避免畸形请求
	// 用超大文件撑爆内存。超限时给出可操作文案。
	const importMaxFileSize = 8 << 20
	if size := fileHeader.Size; size > importMaxFileSize {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("file too large: %d bytes (max %d bytes / 8MiB)", size, importMaxFileSize))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(file, importMaxFileSize))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}
	if int64(len(raw)) >= importMaxFileSize {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("file too large: max %d bytes / 8MiB", importMaxFileSize))
		return
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	dryRun := parseImportBool(r.FormValue("dryRun"), r.FormValue("dry_run"))
	overwrite := parseImportBool(r.FormValue("overwrite"), "")
	result := p.importCockpitAccounts(accounts, dryRun, overwrite)

	log.Printf("panel: cockpit import finished dry_run=%t overwrite=%t total=%d added=%d updated=%d skipped=%d invalid=%d",
		dryRun, overwrite, result.Total, result.Added, result.Updated, result.Skipped, result.Invalid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"dry_run":   dryRun,
		"overwrite": overwrite,
		"total":     result.Total,
		"added":     result.Added,
		"updated":   result.Updated,
		"skipped":   result.Skipped,
		"invalid":   result.Invalid,
		"imported":  result.Added + result.Updated,
		"errors":    result.Errors,
	})
}

type cockpitImportJob struct {
	a         *auth.Auth
	uid       string
	realm     string
	overwrite bool
}

type cockpitImportResult struct {
	Total   int
	Added   int
	Updated int
	Skipped int
	Invalid int
	Errors  []string
}

// importCockpitAccounts 是导入的纯业务实现：先完整分类，再决定是否执行。
// dryRun 只返回分类结果；写入失败单独计为 skipped，invalid 只表示解析期
// 拒绝（缺字段/非法 UID）。errors 只包含 id/uid 与原因，绝不拼接凭证。
func (p *Panel) importCockpitAccounts(accounts []cockpitAccount, dryRun, overwrite bool) cockpitImportResult {
	res := cockpitImportResult{Total: len(accounts), Errors: []string{}}
	jobs := make([]cockpitImportJob, 0, len(accounts))
	jobsByUID := make(map[string]int, len(accounts))
	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			res.Invalid++
			res.Errors = append(res.Errors, fmt.Sprintf("missing required fields (id=%s)", safeImportID(acc.ID)))
			continue
		}
		if !validImportUID(uid) {
			res.Invalid++
			res.Errors = append(res.Errors, fmt.Sprintf("invalid uid (id=%s)", safeImportID(acc.ID)))
			continue
		}
		// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
		realm := auth.ResolveRealm("", acc.Domain)

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		expiresAt := acc.ExpiresAt / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}
		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}
		filePath := filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid))
		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			EnterpriseID: firstNonBlank(acc.EnterpriseID, acc.EnterpriseIDSnake),
			Nickname:     nickname,
			FilePath:     filePath,
		}
		// 导入可能带来显式 enterpriseId 与 token 不一致的旧导出：claim 有企业身份
		// 时以 token 为准，避免新 token 被旧租户字段压住。
		a.InitEnterpriseIdentity()

		exists := p.importTargetExists(uid, filePath)
		if exists && !overwrite {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("uid=%s: already exists (set overwrite=true to replace)", safeImportUID(uid)))
			continue
		}
		// 同一文件内重复 UID：默认保留第一条；overwrite 时后一条替换待写任务，
		// 仍只产生一次写盘，避免并发写同一 auth 文件。
		if idx, seen := jobsByUID[uid]; seen {
			if !overwrite {
				res.Skipped++
				res.Errors = append(res.Errors, fmt.Sprintf("uid=%s: duplicate uid in import", safeImportUID(uid)))
				continue
			}
			jobs[idx] = cockpitImportJob{a: a, uid: uid, realm: realm, overwrite: exists}
			continue
		}
		jobsByUID[uid] = len(jobs)
		jobs = append(jobs, cockpitImportJob{a: a, uid: uid, realm: realm, overwrite: exists})
	}
	if dryRun {
		for _, j := range jobs {
			if j.overwrite {
				res.Updated++
			} else {
				res.Added++
			}
		}
		return res
	}

	if len(jobs) > 0 {
		// runBackgroundBounded 会并发执行回调，共享结果必须由互斥锁保护：
		// 否则多个成功/失败分支会丢计数，甚至并发 append 破坏 Errors 切片。
		var mu sync.Mutex
		if !p.runBackgroundBounded(3, len(jobs), func(k int) {
			j := jobs[k]
			err := saveAndActivateImportedAccount(p, j.a, j.realm, j.overwrite)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Skipped++
				res.Errors = append(res.Errors, fmt.Sprintf("uid=%s: %s", safeImportUID(j.uid), err.Error()))
				return
			}
			if j.overwrite {
				res.Updated++
			} else {
				res.Added++
			}
		}) {
			for _, j := range jobs {
				res.Skipped++
				res.Errors = append(res.Errors, fmt.Sprintf("uid=%s: server shutting down", safeImportUID(j.uid)))
			}
		}
	}
	p.invalidateOverview()
	return res
}

// saveAndActivateImportedAccount 写入单个已校验账号并完成入池后的幂等激活动作。
// 返回的错误只含固定文案与 uid，不会把 auth.Auth 的 token 拼进响应。
func saveAndActivateImportedAccount(p *Panel, a *auth.Auth, realm string, overwrite bool) error {
	if realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			return fmt.Errorf("set realm failed: %v", err)
		}
	} else {
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("save auth failed: %v", err)
	}

	p.cfg.Pool.Add(a)
	// 新号一律解除人工禁用/暂停。覆盖已有凭证时，保留人工暂停/禁用决定，
	// 但清掉旧凭证导致的自动禁用或限流冷却——否则用户用新 token 覆盖后仍
	// 无法路由。人工禁用由面板固定缘由标识，不会被这里误复活。
	if overwrite {
		p.cfg.Pool.ReviveAfterCredentialReplacement(a.UID)
	} else {
		p.cfg.Pool.Revive(a.UID)
	}

	if p.cfg.Upstream != nil {
		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", logfmt.UID8(a.UID), err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", logfmt.UID8(a.UID))
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", logfmt.UID8(a.UID), err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", logfmt.UID8(a.UID))
			}
		} else if !a.IsEnterprise() {
			// 企业账号没有个人签到体系（上游 400 code 10001）。导入时跳过，
			// 但下方额度查询照常走企业端点，不能把「签到失败」误报成导入异常。
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", logfmt.UID8(a.UID), err)
			}
		}
		// 企业额度与个人积分走不同端点；UserResource 已按 IsEnterprise 分流。
		// 必须写回完整快照（总额/到期），否则导入后的账号要等下一次后台刷新
		// 才有正确额度，面板会短暂显示 0 或被误判为余额不足。
		if rm, tt, expiring, at, er, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow()); err == nil {
			p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
			p.cfg.Pool.SetCreditsDetailed(a.UID, rm, tt, expiring, at, er)
		}
	}
	return nil
}

// importTargetExists 检查同一 UID 是否已有账号。池内存在即视为重复；同时检查
// 目标 auth 文件，覆盖「文件已在但尚未热加载/已从池移除」的边界，避免静默覆盖。
func (p *Panel) importTargetExists(uid, filePath string) bool {
	if p.cfg.Pool != nil && p.cfg.Pool.AuthByUID(uid) != nil {
		return true
	}
	if filePath == "" || p.cfg.AuthDir == "" {
		return false
	}
	_, err := os.Stat(filePath)
	return err == nil
}

func parseImportBool(values ...string) bool {
	for _, v := range values {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

// firstNonBlank 返回第一个去空白后非空的字符串，用于兼容同一语义的两种
// 字段命名（如 enterpriseId / enterprise_id）。
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// safeImportID 只输出导入行自带的短 id，并剔除控制字符；缺失时用 "-"。
// 这样错误响应可定位行，又不会把 token 等字段回显出去。
func safeImportID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "-"
	}
	if len(id) > 32 {
		id = id[:32]
	}
	var b strings.Builder
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "-"
	}
	return b.String()
}

// safeImportUID 在导入错误/日志中只暴露 uid 前 8 位，规则与运行日志一致。
// 错误响应可能被浏览器缓存或转贴，不能回显完整账号标识。
func safeImportUID(uid string) string {
	return logfmt.UID8(strings.TrimSpace(uid))
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

package panel

// accounts_bulk.go 账号池批量移除。
//
// 单号移除（panel.go 的 accountRemove）走的是「隔离改名 → 出池 → 清理」三步，
// 每一步都有必须保留的不变量：先改名再出池（否则删文件失败会让账号在下次
// LoadDir 复活）、失败回滚只在「自己拥有源文件」时做（否则并发请求会互相把
// 凭证恢复）、出池后立即落盘。批量版本逐条复用这些语义，只把「落盘」合并成
// 一次：循环调用单号移除 = N 次整份 state 序列化 + N 次 fsync + N 次 Redis
// 镜像，470 号必然超过面板 API 的 15s 超时。

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// accountsRemoveAllLimit 批量移除单次上限，与 accounts/set_all 的同名上限对齐。
// 上限存在的意义不是性能（RemoveMany 是一次写盘），而是防止一次误调用把整池
// 清空；全量清理应分多次显式发起。
const accountsRemoveAllLimit = 2000

type bulkRemoveItem struct {
	UID    string `json:"uid"`
	Status string `json:"status"` // removed | skipped | failed
	Reason string `json:"reason,omitempty"`
}

// accountsRemoveAll 批量移除选中的账号。
//
// 请求体：{"uids":[...], "confirm":<len(uids)>}
//   - uids 必须显式（空数组 → 400）。与 set_all 同一作用域红线：不能把缺失
//     解释成整池——旧前端或脚本漏传时会把一次点击放大成清池。
//   - confirm 必须等于 uids 的条数：删除不可恢复，多一道显式意图；纯后端校验，
//     前端另有一次二次确认。
//
// 响应恒为 200 + 逐 UID 结果（removed/skipped/failed）。整批 4xx/5xx 只用于
// 「请求本身不合法」；单个 UID 失败不影响其余账号，客户端据 results 重试。
func (p *Panel) accountsRemoveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	var body struct {
		UIDs    []string `json:"uids"`
		Confirm *int     `json:"confirm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.UIDs) == 0 {
		writeErr(w, http.StatusBadRequest, "uids is required: batch removal only applies to explicitly selected accounts")
		return
	}
	if len(body.UIDs) > accountsRemoveAllLimit {
		writeErr(w, http.StatusBadRequest, "too many uids (max "+strconv.Itoa(accountsRemoveAllLimit)+")")
		return
	}
	if body.Confirm == nil || *body.Confirm != len(body.UIDs) {
		// 数量必须逐字匹配：这是「我确认要删这么多」的显式承诺，不能只信前端。
		writeErr(w, http.StatusBadRequest, "confirm must equal the number of uids")
		return
	}

	seen := make(map[string]bool, len(body.UIDs))
	targets := make([]string, 0, len(body.UIDs))
	for _, uid := range body.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		targets = append(targets, uid)
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "uids must contain at least one non-empty account id")
		return
	}

	results := make([]bulkRemoveItem, 0, len(targets))
	// locked 记录已拿到 per-account 锁的 UID：整批持有到文件清理完成再释放，
	// 避免「批量移除」与「单号移除 / 签到 / 任务」在同一账号上交错。
	locked := make([]string, 0, len(targets))
	defer func() {
		for _, uid := range locked {
			p.unlockAccount(uid)
		}
	}()

	// quarantined 记录本请求把 auth 文件改名成功的 UID → 隔离路径。
	quarantined := make(map[string]string, len(targets))
	// poolTargets 只包含「文件已隔离（或本来就没有文件）且账号仍在池中」的 UID。
	// 改名失败的账号不进入出池步骤，账号保持原状。
	poolTargets := make([]string, 0, len(targets))

	for _, uid := range targets {
		if !p.tryLockAccount(uid) {
			// 批量里不能因为一个号正在签到就整批失败：跳过并让用户稍后重试。
			results = append(results, bulkRemoveItem{UID: uid, Status: "skipped", Reason: "账号有操作正在执行"})
			continue
		}
		locked = append(locked, uid)
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil {
			results = append(results, bulkRemoveItem{UID: uid, Status: "skipped", Reason: "account not found"})
			continue
		}
		if a.FilePath != "" {
			trashPath := a.FilePath + ".removed-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + logfmt.UID8(uid)
			if err := os.Rename(a.FilePath, trashPath); err != nil {
				if !os.IsNotExist(err) {
					// 与单号移除同口径：文件移不出去就不出池，否则重启会复活。
					log.Printf("panel: remove_all uid=%s（auth 文件隔离失败: %v）", logfmt.UID8(uid), err)
					results = append(results, bulkRemoveItem{UID: uid, Status: "failed", Reason: "凭证文件无法移出 auths 目录，账号未移除"})
					continue
				}
			} else {
				quarantined[uid] = trashPath
			}
		}
		poolTargets = append(poolTargets, uid)
	}

	// 一次出池 + 一次落盘。
	removedUIDs, missing := p.cfg.Pool.RemoveMany(poolTargets)
	removedSet := make(map[string]bool, len(removedUIDs))
	for _, uid := range removedUIDs {
		removedSet[uid] = true
	}

	for _, uid := range missing {
		// 预检时还在、出池时不见了：说明会话期间被并发移除。文件已被本请求
		// 隔离，账号也确实不该存在，因此隔离文件照常清理；但要明确回报 skipped
		// 而不是静默算成功。
		if trash := quarantined[uid]; trash != "" {
			_ = os.Remove(trash)
			delete(quarantined, uid)
		}
		results = append(results, bulkRemoveItem{UID: uid, Status: "skipped", Reason: "account not found"})
	}

	warnings := make([]bulkRemoveItem, 0)
	for _, uid := range poolTargets {
		if !removedSet[uid] || containsString(missing, uid) {
			continue
		}
		p.forgetCreditSnapshot(uid)
		if trash := quarantined[uid]; trash != "" {
			if err := os.Remove(trash); err != nil && !os.IsNotExist(err) {
				// 账号已出池、文件已不在 auths/，清理失败只作 warning：
				// 把一次成功移除报成失败会让用户重复删除已不存在的账号。
				warnings = append(warnings, bulkRemoveItem{UID: uid, Status: "warning", Reason: "隔离文件清理失败（不影响移除结果）"})
			}
		}
		results = append(results, bulkRemoveItem{UID: uid, Status: "removed"})
	}

	p.invalidateOverview()
	p.invalidateBenefitsGrowth()
	p.invalidateScanCache()

	failed, skipped := 0, 0
	for _, it := range results {
		switch it.Status {
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}
	log.Printf("panel: accounts remove_all requested=%d removed=%d skipped=%d failed=%d warnings=%d",
		len(targets), len(removedUIDs), skipped, failed, len(warnings))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"scope":        "selected",
		"requested":    len(targets),
		"removed":      len(removedUIDs),
		"removed_uids": removedUIDs,
		"skipped":      skipped,
		"failed":       failed,
		"results":      results,
		"warnings":     warnings,
	})
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

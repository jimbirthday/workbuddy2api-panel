package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	TokenType      string `json:"token_type"`
	ExpiresAt      int64  `json:"expires_at"`
	Domain         string `json:"domain"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
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

	var (
		total, imported, skipped int
		errs                     []string
		mu                       sync.Mutex
		wg                       sync.WaitGroup
		// sem 限制导入内逐账号的上游调用并发（口径与面板其它路径一致，
		// concurrency=3）：账号多时串行太慢，但无界并发会瞬间打满上游触发风控。
		sem = make(chan struct{}, 3)
	)

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			mu.Lock()
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			mu.Unlock()
			continue
		}
		if !validImportUID(uid) {
			mu.Lock()
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			mu.Unlock()
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

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if realm == "global" {
				if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
					mu.Lock()
					skipped++
					errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
					mu.Unlock()
					return
				}
			} else {
				_, _ = a.BackfillRealm()
			}

			if err := a.SaveAtomic(); err != nil {
				mu.Lock()
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
				mu.Unlock()
				return
			}

			p.cfg.Pool.Add(a)
			p.cfg.Pool.Revive(uid)

			// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
			if realm == "global" {
				if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
					log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
				} else if activated {
					log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
				}
				if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
					log.Printf("panel: import global trial uid=%s: %v", uid, err)
				} else if claimed {
					log.Printf("panel: import global trial uid=%s 已领", uid)
				}
			} else {
				if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
					log.Printf("panel: import checkin uid=%s: %v", uid, err)
				}
			}
			if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
				p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
			}

			mu.Lock()
			imported++
			mu.Unlock()
		}()
	}
	wg.Wait()

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
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

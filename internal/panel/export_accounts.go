package panel

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// exportAccounts 导出账号运维清单（CSV）。只读取 pool.Status 的展示字段，
// 不触碰 auth.Auth 的 AccessToken / RefreshToken / DeviceToken；账号列输出
// 脱敏展示标签（昵称(uid8)）而非原始 UID/邮箱，自由文本经 csvText 防公式
// 注入。调用方必须挂 withAuth，与其它面板 API 使用同一 Bearer 鉴权。
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "account pool not available")
		return
	}
	header := []string{
		"account", "realm", "enterprise", "disabled", "paused", "cooling",
		"reason", "credits", "credits_total", "credits_expiring",
		"credits_earliest_expiry", "credits_earliest_remaining",
		"success_count", "err_total", "last_success", "last_err",
		"checkin_done", "in_flight", "token_requests", "token_prompt", "token_completion", "token_total",
	}
	writeCSVRows(w, exportFilename("accounts", "csv"), header, func(yield func([]string) bool) {
		for _, a := range p.cfg.Pool.List() {
			row := accountExportRow(a)
			if !yield(row) {
				return
			}
		}
	})
}

// maskExportUID 在导出中完全隐藏 uid：短 UID 仍是完整账号标识（如手机号），
// 不能按“截前 8 位”处理。账号定位用行号 + 脱敏昵称，需要精确 UID 时仍应走
// 面板管理接口（已鉴权且不落 CSV）。
func maskExportUID(uid string) string {
	if strings.TrimSpace(uid) == "" {
		return "-"
	}
	return "***"
}

// safeExportNickname 只保留可辨认的昵称，不把导入时回填的邮箱/手机号原样
// 导出。含 @ 的自由文本视为邮箱，整体打码为 "***"；其余昵称原样（CSV 公式
// 防护由 csvText 负责）。
func safeExportNickname(nick string) string {
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return ""
	}
	if strings.Contains(nick, "@") {
		return "***"
	}
	return nick
}

// exportAccountLabel 用昵称作为可读定位；昵称为空时回落到固定占位，绝不回显
// 完整/短 UID。邮箱昵称先打码，避免导入回填的邮箱随 CSV 外流。
func exportAccountLabel(nick string) string {
	nick = safeExportNickname(nick)
	if nick == "" {
		return "***"
	}
	return nick
}

func accountExportRow(a pool.Status) []string {
	expiry := ""
	if !a.CreditsEarliestExpiry.IsZero() {
		expiry = a.CreditsEarliestExpiry.Local().Format("2006-01-02 15:04:05")
	}
	return []string{
		csvText(exportAccountLabel(a.Nickname)), csvText(a.Realm),
		strconv.FormatBool(a.Enterprise), strconv.FormatBool(a.Disabled), strconv.FormatBool(a.Paused),
		strconv.FormatBool(a.Cooling), csvText(a.Reason),
		strconv.FormatInt(a.Credits, 10), strconv.FormatInt(a.CreditsTotal, 10), strconv.FormatInt(a.CreditsExpiring, 10),
		expiry, strconv.FormatInt(a.CreditsEarliestRemaining, 10),
		strconv.FormatInt(a.SuccessCount, 10), strconv.FormatInt(a.ErrTotal, 10),
		formatExportTime(a.LastSuccessTime), formatExportTime(a.LastErrTime),
		strconv.FormatBool(a.CheckinDone), strconv.Itoa(a.InFlight),
		strconv.FormatInt(a.TokenUsage.RequestCount, 10), strconv.FormatInt(a.TokenUsage.PromptTokens, 10),
		strconv.FormatInt(a.TokenUsage.CompletionTokens, 10), strconv.FormatInt(a.TokenUsage.TotalTokens, 10),
	}
}

func formatExportTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

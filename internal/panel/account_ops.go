// account_ops.go 单账号连通性测试与资料同步。
//
// 两者都只读上游、不写账单：连通性测试用一次最小 chat 请求判断账号能否
// 真正产出正文；资料同步只取昵称（上游响应中的手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）。失败一律以 ok=false 回报，
// 绝不把空回复/错误帧包装成成功。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// accountTestDefaultModel 连通性测试默认模型：目录里稳定存在、成本低且
	// 支持工具面较全的 flash 档。
	accountTestDefaultModel = "deepseek-v4.1-flash"
	// accountTestTimeout 单次连通性测试上限。面板同步等待结果，超时必须
	// 明确报失败，不能挂着管理页。
	accountTestTimeout = 30 * time.Second
	// accountTestSoftCooldown 6004 且上游未给重置时刻时的模型级退避基数。
	// 只锁被测模型，不冻结账号。
	accountTestSoftCooldown = 10 * time.Minute
)

// bareAccountModel 去掉网关路由前缀，返回域与裸模型名。测试接口只接受
// 当前账号所属域，避免把 cn: 前缀原样发给上游。
func bareAccountModel(model string) (realm, bare string) {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, "global:") {
		return "global", strings.TrimSpace(strings.TrimPrefix(model, "global:"))
	}
	if strings.HasPrefix(model, "cn:") {
		return "cn", strings.TrimSpace(strings.TrimPrefix(model, "cn:"))
	}
	return "", model
}

// accountTest 对单账号做一次真实的最小对话，只有拿到非空正文才返回成功。
//
// 错误处理边界（红线）：
//   - 上游明确 6004：只给该模型写模型级软冷却，账号与其他模型仍可用；
//   - 其他失败：不写账号级冷却、不喂熔断/连败计数，仅把真实错误返回给面板；
//   - 成功：清该模型的 11102 负缓存并 NoteSuccess，但不写 usage/请求日志/成本账本，
//     因为这是面板运维探测而不是客户流量。
func (p *Panel) accountTest(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	// per-account 互斥：避免与签到/任务/保活并发打同一账号。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)

	var body struct {
		Model string `json:"model"`
	}
	// 请求体可选；空体或非法 JSON 都退回默认模型（测试入口不应因表单细节失败）。
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	}
	realm, model := bareAccountModel(body.Model)
	if model == "" {
		model = accountTestDefaultModel
	}
	if realm != "" && realm != a.Realm() {
		writeErr(w, http.StatusBadRequest, "model realm does not match account realm")
		return
	}

	payload, err := json.Marshal(map[string]any{
		"model":  model,
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "请只回复：pong"},
		},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode test request failed")
		return
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), accountTestTimeout)
	defer cancel()
	meta := upstream.ChatMeta{ConversationRequestID: reqlog.NewRequestID()}
	rc, status, respBody, err := p.cfg.Upstream.ChatStreamContext(ctx, a, payload, "", meta)
	if err != nil {
		elapsed := time.Since(started).Milliseconds()
		var uerr *upstream.Error
		if errors.As(err, &uerr) {
			status = uerr.Status
			if isAccountTestModelRateLimit(uerr, respBody, model) {
				p.coolTestModel(uid, model, uerr, string(respBody))
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":         false,
				"model":      model,
				"status":     status,
				"elapsed_ms": elapsed,
				"error":      testErrorMessage(uerr, respBody),
			})
			return
		}
		// 超时/传输层错误：如实失败。不写任何冷却，避免网络抖动误伤账号。
		msg := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "连通性测试超时"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"model":      model,
			"status":     status,
			"elapsed_ms": elapsed,
			"error":      truncateStr(msg, 240),
		})
		return
	}
	// ChatStreamContext 对 ≥400 且分类为 ErrNone 的防御分支会返回
	// (nil, status, raw, nil)。这里不能假设 err==nil 就一定有 body，否则
	// defer rc.Close() 会 nil-panic 并让面板请求 500。
	if rc == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"model":      model,
			"status":     status,
			"elapsed_ms": time.Since(started).Milliseconds(),
			"error":      truncateStr("上游空响应: "+testErrorMessage(nil, respBody), 240),
		})
		return
	}
	defer rc.Close()

	agg, aggErr := upstream.Aggregate(rc)
	elapsed := time.Since(started).Milliseconds()
	if aggErr != nil {
		if frame, ok := upstream.IsErrorFrameError(aggErr); ok {
			if upstream.IsModelRateLimit(frame) {
				p.coolTestModelPayload(uid, model, frame)
			}
			kind := upstream.FrameKind(frame)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":         false,
				"model":      model,
				"status":     status,
				"elapsed_ms": elapsed,
				"error":      truncateStr("上游错误帧: "+frame, 240),
				"kind":       kind.String(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"model":      model,
			"status":     status,
			"elapsed_ms": elapsed,
			"error":      truncateStr(aggErr.Error(), 240),
		})
		return
	}
	reply := aggregateReply(agg)
	if strings.TrimSpace(reply) == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"model":      model,
			"status":     status,
			"elapsed_ms": elapsed,
			"error":      "上游返回空回复（未拿到有效正文）",
		})
		return
	}
	p.cfg.Pool.NoteSuccess(uid)
	p.cfg.Pool.BlockModelClear(uid, model)
	p.invalidateOverview()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"model":      model,
		"status":     status,
		"elapsed_ms": elapsed,
		"reply":      truncateStr(reply, 240),
	})
}

// isAccountTestModelRateLimit 判断错误是否明确为 6004 模型级限流。只有
// 明确 6004 才走模型级冷却；普通 429/11140 不在这里罚号。
func isAccountTestModelRateLimit(uerr *upstream.Error, respBody []byte, model string) bool {
	if uerr == nil || model == "" {
		return false
	}
	return upstream.IsModelRateLimit(string(respBody)) || upstream.IsModelRateLimit(uerr.Msg)
}

// coolTestModel 按上游重置时刻写模型级冷却；没有权威时间时使用短基数，
// 仍然只锁该模型。
func (p *Panel) coolTestModel(uid, model string, uerr *upstream.Error, payload string) {
	if resetAt, ok := upstream.ParseRateReset(payload); ok {
		p.cfg.Pool.CooldownSoftForModel(uid, accountTestSoftCooldown, resetAt, model, "6004 model rate limit (panel test)")
		return
	}
	if uerr != nil && uerr.RetryAfter > 0 {
		p.cfg.Pool.CooldownSoftForModel(uid, accountTestSoftCooldown, time.Now().Add(uerr.RetryAfter), model, "6004 model rate limit (panel test, retry-after)")
		return
	}
	p.cfg.Pool.CooldownSoftForModelNoReset(uid, model, accountTestSoftCooldown, "6004 model rate limit (panel test, reset unknown)")
}

// coolTestModelPayload 处理 SSE error 帧里的 6004（HTTP 200 形态）。
func (p *Panel) coolTestModelPayload(uid, model, payload string) {
	if resetAt, ok := upstream.ParseRateReset(payload); ok {
		p.cfg.Pool.CooldownSoftForModel(uid, accountTestSoftCooldown, resetAt, model, "6004 model rate limit (panel test)")
		return
	}
	p.cfg.Pool.CooldownSoftForModelNoReset(uid, model, accountTestSoftCooldown, "6004 model rate limit (panel test, reset unknown)")
}

// testErrorMessage 提取可展示的错误文本，不回显 Authorization 等敏感头。
func testErrorMessage(uerr *upstream.Error, respBody []byte) string {
	if len(respBody) > 0 {
		return truncateStr(string(respBody), 240)
	}
	if uerr != nil {
		return truncateStr(uerr.Msg, 240)
	}
	return "upstream request failed"
}

// aggregateReply 从聚合后的 chat.completion 里取首条正文。
func aggregateReply(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	text, _ := msg["content"].(string)
	return text
}

// accountSyncProfile 拉取单账号最新昵称并落盘。只同步昵称，不解析/透传
// 手机号等敏感字段；失败返回 502，不部分成功。
func (p *Panel) accountSyncProfile(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "account operation already running")
		return
	}
	defer p.unlockAccount(uid)

	nick, err := p.cfg.Upstream.FetchAccountProfile(a)
	if err != nil {
		log.Printf("WARN: [panel] sync profile uid=%s: %v", logfmt.UID8(uid), err)
		writeErr(w, http.StatusBadGateway, "fetch profile: "+truncateStr(err.Error(), 200))
		return
	}
	if strings.TrimSpace(nick) == "" {
		writeErr(w, http.StatusBadGateway, "上游未返回昵称")
		return
	}
	updated := p.cfg.Pool.SetNickname(uid, nick)
	p.invalidateOverview()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"nickname": nick,
		"updated":  updated,
	})
}

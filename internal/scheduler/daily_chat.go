package scheduler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// dailyChatWeb 是网页通道的注入点：生产走 upstream.DailyChatWeb，测试可替换为
// 无网络的桩，避免调度器单测意外请求真实域名。桌面腿仍走上游客户端，测试用
// httptest base 覆盖。
var dailyChatWeb = func(s *Scheduler, a *auth.Auth) upstream.WebDailyResult {
	return s.cfg.Upstream.DailyChatWeb(a, dailyChatModel)
}

// dailyChatModel 与 Hub 的内置免费模型一致。每日活跃只发一句 Hi、输出上限 10，
// 用免费模型既满足官方活跃判定，又不会挪用真实流量的模型窗口额度。
const dailyChatModel = "deepseek-v4.1-flash"

// RunDailyChatNow 对全部国际版个人账号执行一次每日活跃打卡。
//
// 与 Hub 一致，一次打卡包含两条腿：
//   - 桌面身份的最小流式对话（兼容旧路径）；
//   - 网页通道的 agent 会话（真正计入官方 Bonus Pack）。
//
// 只处理 realm=global 且非企业的账号：国内号由签到/连登体系负责，企业号没有
// 个人每日活跃福利。网页腿失败时整次记失败——桌面腿成功不等于今天到账，
// 否则活动记录会把「今天没拿到奖励」显示成成功。
func (s *Scheduler) RunDailyChatNow() {
	s.runDailyChat(activity.TriggerManual)
}

func (s *Scheduler) runDailyChat(trigger activity.Trigger) {
	for _, acct := range s.cfg.Pool.TaskAccounts(s.includeDisabledInTasks()) {
		a := acct.Auth
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if !a.IsGlobal() || a.IsEnterprise() {
			continue
		}
		s.dailyChatOne(a, trigger)
	}
}

func (s *Scheduler) dailyChatOne(a *auth.Auth, trigger activity.Trigger) {
	if s.cfg.Upstream == nil {
		s.recordActivity(a, activity.TaskDailyChat, trigger, false, "upstream client unavailable")
		return
	}
	// 网页腿是官方每日活跃的计数依据；先跑它，失败时仍保留桌面腿尝试的
	// 可观测信息，但最终结果必须是失败。
	web := dailyChatWeb(s, a)
	webNote := ""
	if web.OK {
		webNote = fmt.Sprintf("；网页通道 %s，%d 段输出，%d ms", web.Status, web.Chunks, web.ElapsedMS)
	} else {
		webNote = "；网页通道失败：" + web.Error
	}

	body := []byte(fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"Hi"}],"stream":true,"max_tokens":10,"reasoning_effort":"none"}`,
		dailyChatModel))
	rc, status, respBody, err := s.cfg.Upstream.ChatStream(a, body, "", upstream.ChatMeta{
		ConversationID: a.UID + "-daily-chat-" + time.Now().Format("20060102"),
	})
	if err != nil {
		msg := err.Error()
		if len(respBody) > 0 {
			msg = strings.TrimSpace(string(respBody))
		}
		log.Printf("daily-chat %s: desktop leg failed: %v", logfmt.Label(a.UID, a.Nickname), err)
		s.recordActivity(a, activity.TaskDailyChat, trigger, false, "桌面通道失败："+msg+webNote)
		return
	}
	if rc != nil {
		// 只读空流，避免大响应占内存；关闭后交给连接池回收。
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		_ = rc.Close()
	}
	if status != http.StatusOK {
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = fmt.Sprintf("upstream HTTP %d", status)
		}
		log.Printf("daily-chat %s: desktop leg HTTP %d", logfmt.Label(a.UID, a.Nickname), status)
		s.recordActivity(a, activity.TaskDailyChat, trigger, false, "桌面通道失败："+msg+webNote)
		return
	}
	if !web.OK {
		log.Printf("daily-chat %s: web leg failed: %s", logfmt.Label(a.UID, a.Nickname), web.Error)
		s.recordActivity(a, activity.TaskDailyChat, trigger, false, "每日活跃未完成"+webNote)
		return
	}
	log.Printf("daily-chat %s: ok (desktop+web)", logfmt.Label(a.UID, a.Nickname))
	s.recordActivity(a, activity.TaskDailyChat, trigger, true, "每日活跃完成"+webNote)
}

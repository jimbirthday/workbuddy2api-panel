package scheduler

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/activity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunDailyChatNowOnlyGlobalPersonal 每日活跃只打国际版个人号：国内号、企业号
// 与无 token 账号都必须零请求。它复用 recorderUpstream，断言路径而不是响应体。
func TestRunDailyChatNowOnlyGlobalPersonal(t *testing.T) {
	origWeb := dailyChatWeb
	dailyChatWeb = func(s *Scheduler, a *auth.Auth) upstream.WebDailyResult {
		return upstream.WebDailyResult{OK: true, Conversation: "web-ok", Status: "completed", Chunks: 1}
	}
	t.Cleanup(func() { dailyChatWeb = origWeb })

	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "global-ok", AccessToken: "at", Domain: "www.workbuddy.ai"})
	pl.Add(&auth.Auth{UID: "cn-skip", AccessToken: "at"})
	pl.Add(&auth.Auth{UID: "ent-skip", AccessToken: "at", Domain: "www.workbuddy.ai", EnterpriseID: "ent"})
	pl.Add(&auth.Auth{UID: "no-token", Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL}
	store := activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})
	s := New(Config{Pool: pl, Upstream: up, Activity: store})

	s.RunDailyChatNow()

	var n atomic.Int32
	for _, p := range r.snapshot() {
		if p == "/v2/chat/completions" || p == "/chat/completions" {
			n.Add(1)
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("daily chat calls=%d paths=%v want 1 (only global personal)", got, r.snapshot())
	}
	// 活动历史必须留下一条 daily_chat 记录，且只有国际版个人号这一条。
	deadline := time.Now().Add(2 * time.Second)
	for {
		res, err := store.Query(activity.Filter{Task: activity.TaskDailyChat})
		if err == nil && len(res.Records) == 1 {
			if res.Records[0].UID != "global-o" || !res.Records[0].OK || res.Records[0].Realm != "global" {
				t.Fatalf("daily_chat record=%+v", res.Records[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daily_chat history not recorded: records=%+v err=%v", res.Records, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDailyChatWebFailureIsNotReportedAsSuccess 网页腿失败时即使桌面腿成功，
// 活动记录也必须是失败：官方 Bonus Pack 由网页通道计数，桌面调用不能冒充到账。
func TestDailyChatWebFailureIsNotReportedAsSuccess(t *testing.T) {
	origWeb := dailyChatWeb
	dailyChatWeb = func(s *Scheduler, a *auth.Auth) upstream.WebDailyResult {
		return upstream.WebDailyResult{Error: "sandbox not ready"}
	}
	t.Cleanup(func() { dailyChatWeb = origWeb })

	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "global-fail", AccessToken: "at", Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, ChatBaseGlobal: srv.URL}
	store := activity.New(activity.Config{Path: filepath.Join(t.TempDir(), activity.FileName())})
	s := New(Config{Pool: pl, Upstream: up, Activity: store})
	s.RunDailyChatNow()

	deadline := time.Now().Add(2 * time.Second)
	for {
		res, err := store.Query(activity.Filter{Task: activity.TaskDailyChat})
		if err == nil && len(res.Records) == 1 {
			if res.Records[0].OK || !strings.Contains(res.Records[0].Message, "网页通道失败") {
				t.Fatalf("web failure recorded as success: %+v", res.Records[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daily_chat history not recorded: records=%+v err=%v", res.Records, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

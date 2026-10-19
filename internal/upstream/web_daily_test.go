package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDailyChatWebACPSteps 钉住网页通道的协议顺序：建会话 → 取沙箱 →
// SSE 连接 → initialize/load/set_model/prompt → 状态 completed。所有请求都
// 打到 httptest，不触网；webPollEvery 暂置 0 让状态轮询立即完成。
func TestDailyChatWebACPSteps(t *testing.T) {
	origPoll := webPollEvery
	webPollEvery = 0
	t.Cleanup(func() { webPollEvery = origPoll })

	var mu sync.Mutex
	var methods []string
	var queryCount int
	acpCtx, acpCancel := context.WithCancel(context.Background())
	t.Cleanup(acpCancel)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/console/as/conversations/":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"code":0,"data":{"id":"conv-1"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv-1/session":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":{"link":"`+srv.URL+`/acp","token":"tok","sessionId":"sid","cwd":"/workspace"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/acp":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Acp-Connection-Id", "conn-1")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-acpCtx.Done():
			case <-r.Context().Done():
			}
		case r.Method == http.MethodPost && r.URL.Path == "/acp":
			var req struct {
				Method string `json:"method"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			methods = append(methods, req.Method)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv-1":
			mu.Lock()
			queryCount++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":{"status":"completed"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client(), ChatBaseGlobal: srv.URL, GlobalEnabled: true}
	a := &auth.Auth{UID: "u1", AccessToken: "at", Domain: "www.workbuddy.ai"}
	res := c.DailyChatWeb(a, "deepseek-v4.1-flash")
	if !res.OK || res.Conversation != "conv-1" || res.Status != "completed" {
		t.Fatalf("daily chat result=%+v", res)
	}
	mu.Lock()
	got := append([]string(nil), methods...)
	qn := queryCount
	mu.Unlock()
	want := []string{"initialize", "session/load", "session/set_model", "session/prompt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ACP methods=%v want %v", got, want)
	}
	if qn == 0 {
		t.Fatal("conversation status was never polled")
	}
	acpCancel()
}

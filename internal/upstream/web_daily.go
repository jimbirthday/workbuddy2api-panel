// web_daily.go 国际版每日活跃的网页通道（Web Agent ACP over streamable HTTP）。
//
// 背景（对齐 Hub issue #75/#59/#90）：POST /console/as/conversations/ 只把会话
// 排队；真正计入官方「每日活跃」的是客户端接上沙箱并跑完一轮 agent 会话。只发
// 桌面端 chat/completions 不会点亮奖励。流程与 Hub wb_webagent.py 一致：
//
//  1. POST {webBase}/console/as/conversations/ 建会话；
//  2. GET  /console/as/conversations/{id}/session 取沙箱 link/token；
//  3. GET link（SSE）拿 Acp-Connection-Id；
//  4. POST link initialize → session/load → session/set_model → session/prompt；
//  5. 轮询会话状态直到 completed / failed / 超时。
//
// 不接文件系统、终端或工具；这条链路只为每日活跃服务。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// webDailyUserAgent 网页控制台 UA（与 Hub 保持一致）。
	webDailyUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"
	webDailyPrompt = "Hi"
	webTurnTimeout = 120 * time.Second
)

// webPollEvery 是会话状态轮询间隔。变量而非常量，便于协议测试把等待降到 0；
// 生产值初始化为 3s。
var webPollEvery = 3 * time.Second

// WebDailyResult 网页通道每日活跃结果。
type WebDailyResult struct {
	OK           bool   `json:"ok"`
	Conversation string `json:"conversation,omitempty"`
	Status       string `json:"status,omitempty"`
	Chunks       int64  `json:"chunks,omitempty"`
	ElapsedMS    int64  `json:"elapsed_ms,omitempty"`
	Error        string `json:"error,omitempty"`
}

// DailyChatWeb 走网页通道完成一轮 agent 会话（国际版个人号）。
func (c *Client) DailyChatWeb(a *auth.Auth, model string) WebDailyResult {
	started := time.Now()
	if a == nil || !a.IsGlobal() {
		return WebDailyResult{Error: "web daily chat is only for international accounts"}
	}
	if model == "" {
		model = dailyChatDefaultModel
	}
	conv, err := c.webCreateConversation(a, model)
	if err != nil {
		return WebDailyResult{Error: err.Error(), ElapsedMS: time.Since(started).Milliseconds()}
	}
	res := c.webRunConversation(a, conv, model)
	res.ElapsedMS = time.Since(started).Milliseconds()
	return res
}

const dailyChatDefaultModel = "deepseek-v4.1-flash"

// webOrigin 网页控制台根地址：global 固定 workbuddy.ai；CN 走官网域。
func (c *Client) webOrigin(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		// 测试/私有部署可注入 ChatBaseGlobal；未注入时保持生产默认 workbuddy.ai。
		if c.ChatBaseGlobal != "" {
			return c.ChatBaseGlobal
		}
		return defaultGlobalBase
	}
	return c.webBase(a)
}

func (c *Client) webCreateConversation(a *auth.Auth, model string) (string, error) {
	body := map[string]any{
		"prompt":             webDailyPrompt,
		"model":              model,
		"conversationOrigin": "workbuddy-app",
		"plugins": []map[string]any{{
			"name": "weixinpay", "marketplace": "codebuddy-builtin",
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.webOrigin(a)+"/console/as/conversations/", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	c.webDailyHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("create conversation HTTP %d: %s", resp.StatusCode, truncate(string(payload), 160))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return "", fmt.Errorf("parse conversation: %w", err)
	}
	if env.Code != 0 || strings.TrimSpace(env.Data.ID) == "" {
		return "", fmt.Errorf("create conversation code=%d", env.Code)
	}
	return env.Data.ID, nil
}

func (c *Client) webConversationGet(a *auth.Auth, conversation, suffix string) (json.RawMessage, error) {
	u := c.webOrigin(a) + "/console/as/conversations/" + url.PathEscape(conversation) + suffix
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.webDailyHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("conversation GET HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	return raw, nil
}

func (c *Client) webConversationStatus(a *auth.Auth, conversation string) string {
	raw, err := c.webConversationGet(a, conversation, "")
	if err != nil {
		return ""
	}
	var env struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return ""
	}
	return strings.TrimSpace(env.Data.Status)
}

func (c *Client) webDailyHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("X-User-Id", a.UID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", c.webOrigin(a))
	req.Header.Set("Referer", c.webOrigin(a)+"/app")
	req.Header.Set("User-Agent", webDailyUserAgent)
}

// webRunConversation 接上沙箱并执行一轮，然后轮询到 completed。
func (c *Client) webRunConversation(a *auth.Auth, conversation, model string) WebDailyResult {
	raw, err := c.webConversationGet(a, conversation, "/session")
	if err != nil {
		return WebDailyResult{Conversation: conversation, Error: "session: " + err.Error()}
	}
	var env struct {
		Data struct {
			Link       string `json:"link"`
			Endpoint   string `json:"endpoint"`
			Token      string `json:"token"`
			SessionID  string `json:"sessionId"`
			SessionID2 string `json:"session_id"`
			Cwd        string `json:"cwd"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return WebDailyResult{Conversation: conversation, Error: "parse session"}
	}
	link := strings.TrimSpace(env.Data.Link)
	if link == "" {
		link = strings.TrimSpace(env.Data.Endpoint)
	}
	token := strings.TrimSpace(env.Data.Token)
	sessionID := strings.TrimSpace(env.Data.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(env.Data.SessionID2)
	}
	if sessionID == "" {
		sessionID = conversation
	}
	if link == "" || token == "" {
		return WebDailyResult{Conversation: conversation, Error: "sandbox not ready (missing link/token)"}
	}
	acp, err := newACPChannel(link, token, webDailyUserAgent, c.HTTP)
	if err != nil {
		return WebDailyResult{Conversation: conversation, Error: err.Error()}
	}
	defer acp.close()
	if err := acp.open(); err != nil {
		return WebDailyResult{Conversation: conversation, Error: err.Error()}
	}
	if err := acp.request("initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}, 1); err != nil {
		return WebDailyResult{Conversation: conversation, Status: acp.status, Error: err.Error()}
	}
	cwd := strings.TrimSpace(env.Data.Cwd)
	if cwd == "" {
		cwd = "/workspace"
	}
	if err := acp.request("session/load", map[string]any{
		"sessionId": sessionID, "cwd": cwd, "mcpServers": []any{},
	}, 2); err != nil {
		return WebDailyResult{Conversation: conversation, Status: acp.status, Error: err.Error()}
	}
	// 模型切换失败不致命：与 Hub 一样继续本轮，只是可能落回会话默认模型。
	_ = acp.request("session/set_model", map[string]any{
		"sessionId": sessionID, "modelId": model,
	}, 3)
	if err := acp.request("session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": webDailyPrompt}},
	}, 4); err != nil {
		return WebDailyResult{Conversation: conversation, Status: acp.status, Error: err.Error()}
	}

	deadline := time.Now().Add(webTurnTimeout)
	for time.Now().Before(deadline) {
		st := c.webConversationStatus(a, conversation)
		acp.setStatus(st)
		if st == "completed" {
			return WebDailyResult{
				OK: true, Conversation: conversation, Status: st,
				Chunks: acp.chunks.Load(),
			}
		}
		if st == "failed" || st == "error" {
			return WebDailyResult{Conversation: conversation, Status: st, Error: "conversation status=" + st}
		}
		time.Sleep(webPollEvery)
	}
	return WebDailyResult{
		Conversation: conversation, Status: acp.getStatus(), Chunks: acp.chunks.Load(),
		Error: fmt.Sprintf("conversation not completed within %s", webTurnTimeout),
	}
}

// acpChannel ACP streamable HTTP：SSE 收通知、独立 POST 发 JSON-RPC。
type acpChannel struct {
	link      *url.URL
	token     string
	ua        string
	http      *http.Client
	connID    string
	stream    io.ReadCloser
	statusMu  sync.Mutex
	status    string
	chunks    atomic.Int64
	events    atomic.Int64
	closeOnce atomic.Bool
}

func newACPChannel(link, token, ua string, hc *http.Client) (*acpChannel, error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid sandbox link")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &acpChannel{link: u, token: token, ua: ua, http: hc}, nil
}

func (c *acpChannel) open() error {
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.link.String(), nil)
	if err != nil {
		cancel()
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("ACP SSE HTTP %d", resp.StatusCode)
	}
	c.connID = strings.TrimSpace(resp.Header.Get("Acp-Connection-Id"))
	if c.connID == "" {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("ACP SSE missing Acp-Connection-Id")
	}
	c.stream = resp.Body
	go func() {
		defer cancel()
		c.readEvents(resp.Body)
	}()
	return nil
}

func (c *acpChannel) readEvents(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
				} `json:"update"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(payload), &msg) != nil {
			continue
		}
		c.events.Add(1)
		if msg.Method == "session/update" && msg.Params.Update.SessionUpdate == "agent_message_chunk" {
			c.chunks.Add(1)
		}
	}
}

func (c *acpChannel) request(method string, params any, id int) error {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.link.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Acp-Connection-Id", c.connID)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%s HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (c *acpChannel) setStatus(s string) {
	c.statusMu.Lock()
	c.status = s
	c.statusMu.Unlock()
}

func (c *acpChannel) getStatus() string {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status
}

func (c *acpChannel) close() {
	if !c.closeOnce.CompareAndSwap(false, true) {
		return
	}
	if c.stream != nil {
		_ = c.stream.Close()
	}
}

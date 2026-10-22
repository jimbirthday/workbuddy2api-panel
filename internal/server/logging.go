// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxUserAgentLen 归档与面板展示保留的 UA 字节上限。UA 是客户端完全可控的
// 自由文本（浏览器动辄 150+ 字符，恶意客户端可以塞几 KB），落盘前必须截断，
// 否则一条请求就能把归档行撑大。截断只影响展示，不影响请求处理。
const maxUserAgentLen = 200

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start            time.Time
	model            string
	mode             string // "stream" | "sync"
	uid              string // 完整 uid，展示时只取前 8 位
	nick             string // 账号昵称（随选号同步），流水行经 logfmt.Label 拼成 "昵称(uid8)"
	ttfb             time.Duration
	toks             int // <0 表示 usage 缺失 → 显示 "-"
	status           int
	requestID        string
	keyID            string // 命中的 API key id（不含密钥）；空 = 未鉴权/旧兼容
	outcome          string
	attempts         int
	credit           float64
	hasCredit        bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	// 缓存命中观测（usage.prompt_cache_hit_tokens / miss）：上游给到才有效。
	// 供 usage 桶命中率维度与 reqlog 逐次记录；缺失时 hasCache=false 不参与统计。
	cacheHit  int64
	cacheMiss int64
	hasCache  bool

	// 调用来源（客户端 IP / User-Agent）。空 = 未采集（logging.request_client_info
	// 关闭，或非 chat 路径），展示层一律以 "-" 兜底。
	clientIP  string
	userAgent string

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// newChatStatWithModel 用入口已解析出的 model 构造统计对象，避免为日志再完整
// 解析一遍 body。model 为空时与 parseModelFromBody 的缺省口径一致显示 "-"。
func newChatStatWithModel(now time.Time, model string, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	if model == "" {
		model = "-"
	}
	return &chatStat{start: now, model: model, mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRowEx(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit, s.clientIP, s.userAgent, s.keyID)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit  bool
	credit     float64
	errorFrame bool
	// cacheHit/cacheMiss 上游末帧 usage.prompt_cache_hit_tokens / miss_tokens，
	// 供用量桶的命中率维度与 reqlog 逐次记录（issue #92）。
	hasCacheHit  bool
	cacheHit     int
	cacheMiss    int
	hasCacheMiss bool
	pend         []byte // 已读未返回的行缓存
	lineBuf      []byte // 超长 SSE 行的拼接缓冲（仅 ErrBufferFull 时复用）
	readBuf      []byte // 批量 Read 的暂存缓冲（仅单次无法填满调用方 p 时使用）
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	s.parseSSELineBytes([]byte(line))
}

// parseSSELineBytes 与 parseSSELine 语义一致，但直接消费 bufio 的临时缓冲，
// 避免流式透传时每行都把 []byte 转成 string（ReadString）再转回 []byte（解码）。
func (s *chatStatsReader) parseSSELineBytes(line []byte) {
	payload := upstream.SSEDataPayload(line)
	if payload == nil {
		return
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	// 绝大多数 delta 帧只有 content/tool_calls，完整解码整帧只为找 usage/error
	// 是流式路径上的固定浪费。先做字节预过滤：只有出现这两个键才解码。
	// 键名在 JSON 里仍可能出现在字符串内容中（如用户消息包含 "usage"），
	// 因此预过滤只作为“快速跳过”，命中后仍走完整解码并按结构判字段。
	// 本轮同时把「末帧 usage」和「error 帧」的识别合并，避免为同一 payload
	// 分别判断两次；坏帧保持静默跳过。
	// 两个键共用一次扫描；绝大多数 delta 帧在这里直接返回。
	hasUsage := bytes.Contains(payload, []byte(`"usage"`))
	// 错误既可能是 OpenAI 的顶层 error，也可能是上游业务信封
	// {"code":6004,"msg":...}。后者没有 error 键，只看 error 会漏记限流帧。
	hasError := bytes.Contains(payload, []byte(`"error"`)) || bytes.Contains(payload, []byte(`"code"`))
	if !hasUsage && !hasError {
		return
	}
	var chunk struct {
		Code    json.RawMessage   `json:"code"`
		Error   json.RawMessage   `json:"error"`
		Choices []json.RawMessage `json:"choices"`
		Usage   *struct {
			PromptTokens         *int     `json:"prompt_tokens"`
			CompletionTokens     *int     `json:"completion_tokens"`
			TotalTokens          *int     `json:"total_tokens"`
			Credit               *float64 `json:"credit"`
			PromptCacheHitTokens *int     `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTok   *int     `json:"prompt_cache_miss_tokens"`
			CachedTokens         *int     `json:"cached_tokens"`
			// 真实命中在嵌套 details 里（上游同时给顶层 0 值兼容别名），必须
			// 与 upstream.bestUsageCacheHitTokens 同一优先级解析，否则流式请求
			// 的缓存命中会被记成 0 或整体缺失，与非流式口径不一致。
			PromptTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputTokensDetails *struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	// error:null / error:{} 不是错误帧：前者是正常心跳/元信息占位，后者为空对象
	// 也无任何错误信息。与 upstream.frameIsError 同口径，避免流尾统计把 null 当
	// 成上游错误、误触发账号处置或错误指标。
	if len(chunk.Error) > 0 {
		var errValue any
		if json.Unmarshal(chunk.Error, &errValue) == nil {
			switch v := errValue.(type) {
			case nil:
				// error:null 是正常收尾/元信息占位。
			case map[string]any:
				// error:{} 没有任何错误信息；与 upstream.frameIsError 同口径。
				if len(v) > 0 {
					s.errorFrame = true
				}
			default:
				s.errorFrame = true
			}
		}
	}
	// 业务信封与 upstream.frameIsError 同口径：code 存在且非 0，同时没有可用
	// choices。带 choices 的帧即便夹带 code 仍是回答正文。错误标记只置位、不回落，
	// 避免真实错误帧后再来 error:{} 把整条流误判为成功。
	if !s.errorFrame && len(chunk.Code) > 0 && len(chunk.Choices) == 0 {
		var code any
		if json.Unmarshal(chunk.Code, &code) == nil && code != nil {
			if strings.TrimSpace(fmt.Sprint(code)) != "0" {
				s.errorFrame = true
			}
		}
	}
	if chunk.Usage == nil {
		return
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
	// 缓存命中按「嵌套优先、只取正值」解析：上游真实命中在
	// prompt_tokens_details.cached_tokens / input_tokens_details.cached_tokens，
	// 顶层 prompt_cache_hit_tokens / cached_tokens 常是 0 值兼容别名。只读顶层
	// 会得到 hit=0（命中率恒 0%）或整体缺失（ok=false 不参与统计）。
	nested := []*int{chunk.Usage.PromptCacheHitTokens, chunk.Usage.CachedTokens}
	if d := chunk.Usage.PromptTokensDetails; d != nil {
		nested = append([]*int{d.CachedTokens}, nested...)
	}
	if d := chunk.Usage.InputTokensDetails; d != nil {
		nested = append(nested, d.CachedTokens)
	}
	for _, v := range nested {
		if v != nil && *v > 0 {
			s.hasCacheHit = true
			s.cacheHit = *v
			break
		}
	}
	if chunk.Usage.PromptCacheMissTok != nil {
		s.hasCacheMiss = true
		s.cacheMiss = *chunk.Usage.PromptCacheMissTok
	}
}

// CacheTokens 返回末帧 usage 的缓存命中 / 未命中 token 数。miss 缺失时按
// prompt - hit 推导；hit 与 miss 均不可得时 ok=false（不参与命中率统计）。
func (s *chatStatsReader) CacheTokens() (hit, miss int64, ok bool) {
	if !s.hasCacheHit {
		return 0, 0, false
	}
	hit = int64(s.cacheHit)
	miss = int64(s.cacheMiss)
	if !s.hasCacheMiss {
		if !s.hasPromptTokens || s.promptTokens < s.cacheHit {
			return hit, 0, true
		}
		miss = int64(s.promptTokens - s.cacheHit)
	}
	return hit, miss, true
}

// SawErrorFrame 报告流中是否透传过 SSE error 帧。
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// readLine 读取一条完整 SSE 行。ReadSlice 在单行超过 bufio 缓冲时返回
// bufio.ErrBufferFull 和半行；必须继续拼接，否则 usage/error 位于行尾时会被
// 截断，且透传给客户端的内容也会被拆错。普通短行仍走零分配路径。
func (s *chatStatsReader) readLine() ([]byte, error) {
	line, err := s.br.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return line, err
	}
	s.lineBuf = append(s.lineBuf[:0], line...)
	for {
		line, err = s.br.ReadSlice('\n')
		s.lineBuf = append(s.lineBuf, line...)
		if err != bufio.ErrBufferFull {
			return s.lineBuf, err
		}
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	// io.Copy 给的是 32KB 级缓冲；逐行返回会让 500 帧的流产生 500 次 Read。
	// 这里把连续完整行一次性填进 p：bufio.ReadSlice 的切片在下一次读取前有效，
	// 因此必须在同一轮循环内既解析该行、又把它复制到 p（不能先缓存切片再统一解析）。
	// 任何跨 Read 的残行才落到 readBuf/pend，语义与逐行实现逐字节一致。
	n := 0
	for {
		line, err := s.readLine()
		if len(line) > 0 {
			s.parseSSELineBytes(line)
			if n+len(line) <= len(p) {
				copy(p[n:], line)
				n += len(line)
				if err != nil {
					return n, err
				}
				continue
			}
			// 剩余空间不足一行：先把这一行完整复制到自有缓冲，再按 p 的容量返回，
			// 剩余部分由下一次 Read 从 pend 续传，绝不截断行内的 usage/error。
			s.stashPending(line, p[n:], &n)
			return n, nil
		}
		if n > 0 {
			return n, err
		}
		return 0, err
	}
}

// stashPending 把一行写入调用方剩余空间，并把未返回部分保存到 pend。
// readBuf 独立于 bufio 内部缓冲：ReadSlice 返回的切片在下一次读取后失效。
func (s *chatStatsReader) stashPending(line, dst []byte, n *int) {
	need := len(line)
	if cap(s.readBuf) < need {
		s.readBuf = make([]byte, need)
	} else {
		s.readBuf = s.readBuf[:need]
	}
	copy(s.readBuf, line)
	c := copy(dst, s.readBuf)
	*n += c
	s.pend = append(s.pend[:0], s.readBuf[c:]...)
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 实现委托 logfmt.UID8，避免 "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
type requestTrace struct {
	id    string
	path  string
	start time.Time
	stat  *chatStat
	// 调用来源，进入 handler 时一次性采集（见 ServeHTTP / captureClientInfo）。
	clientIP  string
	userAgent string
}

// captureClientInfo 采集调用来源（客户端 IP + 截断后的 UA）。开关关闭时保持空串：
// 来源信息比 token 计数敏感，是否落盘由 logging.request_client_info 决定。
func (t *requestTrace) captureClientInfo(r *http.Request) {
	if t == nil || r == nil {
		return
	}
	t.clientIP = clientIPForLog(r)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// clientIPForLog 提取用于日志展示的客户端 IP。
//
// 与 upstream.ExtractClientIP 的差别：后者只认代理头（X-Forwarded-For 首段 →
// X-Real-IP），因为它的用途是把客户端 IP **透传给上游**，回落到网关自身地址会
// 污染上游风控判据；日志场景相反——直连（无反代）时 RemoteAddr 就是唯一线索，
// 必须回落，否则面板里所有来源都显示 "-"。代理头优先保证反代后拿到真实客户端。
func clientIPForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// dashIfEmpty 空串统一显示 "-"（来源字段未采集时不留空白列）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	path := t.path
	if path == "" {
		path = "/v1/chat/completions"
	}
	e := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      path,
		Status:    status,
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.KeyID = s.keyID
		e.Account = logfmt.Label(s.uid, s.nick)
		e.Model = s.model
		e.Outcome = s.outcome
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
		e.CacheHitTokens = s.cacheHit
		e.CacheMissTokens = s.cacheMiss
	}
	e.ClientIP = t.clientIP
	e.UserAgent = t.userAgent
	// Protocol conversion happens after chatCompletions has set its outcome.
	// A late conversion failure must override that preliminary success while
	// retaining specific stream-error/interrupted outcomes.
	if e.Outcome == "" || (e.Outcome == reqlog.OutcomeSuccess && (status < 200 || status >= 300)) {
		if status >= 200 && status < 300 {
			e.Outcome = reqlog.OutcomeSuccess
		} else {
			e.Outcome = reqlog.OutcomeHTTPError
		}
	}
	e.OK = status >= 200 && status < 300 && e.Outcome == reqlog.OutcomeSuccess
	return e
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态，同时保留 Flusher/Unwrap，
// 避免破坏 SSE 逐帧刷新。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
)

// logChatRow 打印一行请求级表格日志（输出 chatLogOut，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	logChatRowEx(ttfb, total, model, mode, uid, nick, status, toks, "", "", 0, 0, false, "", "", "")
}

// rowBufPool 复用流水行构建缓冲，避免每请求分配一次 strings.Builder。
var rowBufPool = sync.Pool{New: func() any { b := strings.Builder{}; return &b }}

// padInt 把 n 右对齐补零写进 buf（与 fmt "%03d" 逐字节一致，负数不作特殊处理，
// 与既有格式语义一致：seq 恒正）。
func padInt(buf *strings.Builder, n int64, width int) {
	digits := strconv.FormatInt(n, 10)
	if d := width - len(digits); d > 0 {
		for i := 0; i < d; i++ {
			buf.WriteByte('0')
		}
	}
	buf.WriteString(digits)
}

// logChatRowEx 是带请求 ID、结果、重试、积分与调用来源字段的扩展流水行。旧调用保持
// 原格式；requestID 非空时才追加扩展字段；来源两参均为空时不追加来源段。
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent, keyID string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = strconv.Itoa(toks)
		// 速率与用量账本走同一个函数（扣掉 TTFB）。此前这里自己除 total，漏扣首
		// token 等待，于是控制台流水行的 tok/s 与面板数字对不上（issue #34）——
		// 本函数本来就收到了 ttfb，只是没拿它算速率。
		if tps, ok := tokensPerSecond(int64(toks), total, ttfb); ok {
			tokpsField = strconv.FormatFloat(tps, 'f', 1, 64) + "tok/s"
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = strconv.FormatInt(ttfb.Milliseconds(), 10) + "ms"
	}
	extra := ""
	if requestID != "" {
		if outcome == "" {
			outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				outcome = reqlog.OutcomeSuccess
			}
		}
		creditField := "-"
		if hasCredit {
			creditField = strconv.FormatFloat(credit, 'f', 4, 64)
		}
		extra = " rid=" + requestID + " | out=" + outcome + " | try=" + strconv.Itoa(attempts) + " | credit=" + creditField + " |"
		if keyID != "" {
			extra += " key=" + keyID + " |"
		}
	}
	// 调用来源：IP 用可解析的裸值（便于 grep），UA 用 ShortUA 压缩后的客户端标签
	// 并加引号（标签内可能含空格，如 `OpenAI/Python 1.30.0` 只会取到 OpenAI/Python）。
	// 两者都未采集时不追加，旧行格式保持不变。
	src := ""
	if clientIP != "" || userAgent != "" {
		ua := "-"
		if s := logfmt.ShortUA(userAgent); s != "" {
			ua = `"` + s + `"`
		}
		src = " src=" + dashIfEmpty(clientIP) + " ua=" + ua + " |"
	}
	// 用 strings.Builder + strconv 逐段拼装：每请求行此前有约 7 次 fmt 反射分配，
	// 生产环境 chatLog 常开，这是每请求固定支出。输出字节与旧 fmt 逐字节一致。
	buf := rowBufPool.Get().(*strings.Builder)
	buf.Reset()
	buf.WriteString("| #")
	padInt(buf, seq, 3)
	buf.WriteString(" | ")
	buf.WriteString(time.Now().Format("15:04:05"))
	buf.WriteString(" | ")
	buf.WriteString(model)
	buf.WriteString(" | ")
	buf.WriteString(mode)
	buf.WriteString(" | ")
	buf.WriteString(strconv.Itoa(status))
	buf.WriteString(" | ")
	buf.WriteString(acct)
	buf.WriteString(" | TTFB=")
	buf.WriteString(logfmt.Pad(ttfbMS, chatTTFBWidth))
	buf.WriteString(" | tok=")
	buf.WriteString(logfmt.Pad(tokField, chatTokWidth))
	buf.WriteString(" | ")
	buf.WriteString(logfmt.Pad(tokpsField, chatRateWidth))
	buf.WriteString(" | total=")
	buf.WriteString(strconv.FormatFloat(total.Seconds(), 'f', 1, 64))
	buf.WriteString("s |")
	buf.WriteString(extra)
	buf.WriteString(src)
	buf.WriteByte('\n')
	_, _ = io.WriteString(chatLogOut, buf.String())
	rowBufPool.Put(buf)
}

// cacheMissSignal 低命中率告警信号（issue #92 P1 轻量化）：同一模型此前观测到过
// 命中、本次大前缀（≥1000 tok）整段未命中（命中率 <10%）时打一条 WARN，每模型
// 10 分钟冷却。只报「本可命中却重算」的可行动信号——新模型/新会话的首请求天然
// 全 miss，不在此列。
type cacheMissSignal struct {
	mu      sync.Mutex
	everHit map[string]bool
	last    map[string]time.Time
}

var cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}

// cacheMissWarnEvery 冷却间隔；变量供测试缩短。
var cacheMissWarnEvery = 10 * time.Minute

// noteCacheTokens 记录一次观测：有命中 → 标记该模型可命中；整段未命中且此前
// 命中过 → 触发 WARN（冷却内不重复）。
func (c *cacheMissSignal) noteCacheTokens(model string, prompt, hit, miss int64) {
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit > 0 {
		c.everHit[model] = true
		return
	}
	// hit==0 才可能是整段重算。小前缀（<1000 tok）的 miss 无告警价值；上游未回
	// miss 字段时按 prompt 全量视为未命中。
	if prompt < 1000 {
		return
	}
	if miss <= 0 {
		miss = prompt
	}
	if !c.everHit[model] {
		return
	}
	now := time.Now()
	if t, ok := c.last[model]; ok && now.Sub(t) < cacheMissWarnEvery {
		return
	}
	c.last[model] = now
	log.Printf("WARN: [server] cache miss: model=%s 本次大前缀未命中（prompt=%d miss=%d，此前观测到过命中）；上游前缀缓存重算，费用会显著升高", model, prompt, miss)
}

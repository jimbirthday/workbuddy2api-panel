// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const sseDataPrefix = "data:"

// sseLineReader 读取一条完整 SSE 行。ReadSlice 的正常路径直接返回 bufio
// 内部切片（零分配）；只有单行超过缓冲容量时才把分片拼进 lineBuf。
// 返回的切片仅在下一次调用前有效，调用方必须在同一轮循环内消费完。
type sseLineReader struct {
	br      *bufio.Reader
	lineBuf []byte
}

func newSSELineReader(r io.Reader) *sseLineReader {
	return &sseLineReader{br: bufio.NewReaderSize(r, 64*1024)}
}

func (s *sseLineReader) readLine() ([]byte, error) {
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

// sseDataPayload 返回 SSE data 字段的内容（不含行尾 \r\n）。按规范接受
// "data:" 后零个或多个空格，也接受 JSON 紧贴冒号的 "data:{...}"；上游确实
// 存在这两种合法变体。非 data 行（含 "database:" 这类同前缀字段）返回 nil。
func sseDataPayload(line []byte) []byte {
	return SSEDataPayload(line)
}

// SSEDataPayload extracts a data field using the same rules for forwarding and
// usage accounting. It accepts data: with or without whitespace after the colon.
func SSEDataPayload(line []byte) []byte {
	line = bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(line, []byte(sseDataPrefix)) {
		return nil
	}
	payload := line[len(sseDataPrefix):]
	if len(payload) > 0 && payload[0] != ' ' && payload[0] != '\t' {
		// "data:" 必须正好是字段名；"data:{"x"}" 合法，"datax..." 不合法。
		return payload
	}
	return bytes.TrimLeft(payload, " \t")
}

// errEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
// 用哨兵错误替代裸 fmt.Errorf：StreamHint 的调用方（handler 流式路径）需要区分
// 「上游空流」与「客户端断连写失败」——空流是上游缺陷，应记 502 观测；写失败是
// 客户端已走，日志口径不同。Aggregate 与 StreamHint 共用同一哨兵（errors.Is 判定）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为「上游空流」（无有效 SSE 帧）——供 handler
// 在流式路径把空流记为失败观测并返回真实 502。StreamHint 在空流时不写任何
// 字节（200 成功头延迟到首个正文帧），因此调用方仍可自由选择错误状态码；
// 与客户端断连类错误区分。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// ErrFrame 承载「上游以 SSE error 帧报错」的原始 payload（非 HTTP >=400 形态）。
// 上游真实形态：HTTP 200 已开流，但第一帧就是 error（6004 模型级限流 / 内容拦截 /
// 审核）。此前非流式 Aggregate 只看 id/model/created/usage/choices，把 error 帧当成
// 有效事件计入 validEvents，最终合成 content:"" + finish_reason:"stop" 的 200 假成功，
// 且账号不被处置——限流号被当健康号继续选中。用独立错误类型携带原文，让 handler
// 走与流式完全一致的 FrameKind/Classify 分类与冷却分支（单一事实来源）。
type ErrFrame struct{ Payload string }

func (e *ErrFrame) Error() string {
	return "upstream error frame: " + truncate(e.Payload, 200)
}

// IsErrorFrameError 报告 err 是否由上游 error 帧触发，并返回帧原文。
func IsErrorFrameError(err error) (string, bool) {
	var ef *ErrFrame
	if errors.As(err, &ef) {
		return ef.Payload, true
	}
	return "", false
}

// StreamReadError 包装「读上游 SSE 流失败」（连接中断 / 空闲看门狗掐流 / 读超时），
// 与「写客户端失败」区分开。此前 StreamHint 把两类错误用同一个 error 返回，handler
// 只能一律按「客户端写失败 → 账号健康」处理：上游半途断流/停滞时账号被记成功并清
// 11102 负缓存、绑粘性，坏号长期留在池内。用独立类型让调用方按来源分别处置。
type StreamReadError struct{ Err error }

func (e *StreamReadError) Error() string { return "upstream stream read: " + e.Err.Error() }
func (e *StreamReadError) Unwrap() error { return e.Err }

// IsStreamReadError 报告 err 是否为读上游流失败（读侧），与写客户端失败（写侧）区分。
func IsStreamReadError(err error) bool {
	var re *StreamReadError
	return errors.As(err, &re)
}

// aggChoice 单个 choice（按 index 区分）的聚合缓冲。
//
// 为什么要分桶：OpenAI 的 n>1 让上游在同一流里并行下发 choices[].index=0..n-1，
// 每路各有自己的 content、tool_calls 与 finish_reason。旧实现把所有 choice 的
// delta 拼进同一份 content，返回时又只合成一个 index=0 的 choice——客户端要
// n=2 却拿到两个回答交错的一段正文，且 finish_reason 只剩最后一路的值，
// 属于静默数据损坏。分桶后每路独立聚合，输出仍是合法的 n 条 choices。
type aggChoice struct {
	content       strings.Builder
	reasoning     strings.Builder
	refusal       strings.Builder
	role          string
	finishReason  string
	sawTerminal   bool // 本 choice 收到过非空 finish_reason
	gotAnyContent bool // 本 choice 已取到正文（message 回退的 latch）
	toolCalls     map[int]map[string]any
	toolOrder     []int
	toolSeq       int
	idIndex       map[string]int
	legacyFC      map[string]any
}

// appendContent 是本 choice「已取到正文」latch 的唯一写入点：
//   - 空串不算已取到正文（role-only 首帧 / 整条空 message 是常态帧，
//     置位后会把后续真正文挡在 message 回退分支的 !gotAnyContent 守卫外）；
//   - 空串也无字节可追加，跳过写入。
func (c *aggChoice) appendContent(txt string) {
	if txt == "" {
		return
	}
	c.content.WriteString(txt)
	c.gotAnyContent = true
}

// nextToolIndex 分配下一个不冲突的缺 index 序号：从 toolSeq 起递增跳过既有
// index（合规流的 index 是 0..N-1，缺 index 的补位不能覆盖它们）。
func (c *aggChoice) nextToolIndex() int {
	for {
		idx := c.toolSeq
		c.toolSeq++
		if _, used := c.toolCalls[idx]; !used {
			return idx
		}
	}
}

// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进本 choice。
// 兼容缺 index 的上游：id 优先、最近槽兜底（见下方调用点注释）。
func (c *aggChoice) mergeToolCallsChunk(tcs []any) {
	if c.toolCalls == nil {
		c.toolCalls = map[int]map[string]any{}
	}
	if c.idIndex == nil {
		c.idIndex = map[string]int{}
	}
	for _, tc := range tcs {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		idx := -1
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		} else if cid, _ := call["id"].(string); cid != "" {
			if mid, seen := c.idIndex[cid]; seen {
				idx = mid // 该 id 已归位过：延续既有调用（跨帧有效）
			} else {
				idx = c.nextToolIndex()
			}
		} else if len(c.toolOrder) > 0 {
			idx = c.toolOrder[len(c.toolOrder)-1] // 无 id 碎片：延续最近调用
		} else {
			idx = c.nextToolIndex()
		}
		merged, seen := c.toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			c.toolCalls[idx] = merged
			c.toolOrder = append(c.toolOrder, idx)
		}
		if cid, _ := call["id"].(string); cid != "" {
			c.idIndex[cid] = idx
		}
		if cid, _ := merged["id"].(string); cid != "" {
			c.idIndex[cid] = idx
		}
		mergeToolCallDelta(merged, call)
	}
}

// mergeMessageFields 把非 delta 的完整 message 内容并入本 choice（message 是整条
// 下发，非流式拼接，content 只取一次）。
func (c *aggChoice) mergeMessageFields(msg map[string]any) {
	if r2, ok := msg["role"].(string); ok && r2 != "" {
		c.role = r2
	}
	if txt, ok := msg["content"].(string); ok {
		c.appendContent(txt)
	}
	if rc, ok := msg["reasoning_content"].(string); ok {
		c.reasoning.WriteString(rc)
	}
	if tcs, ok := msg["tool_calls"].([]any); ok {
		c.mergeToolCallsChunk(tcs)
	}
	if fc, ok := msg["function_call"].(map[string]any); ok {
		mergeLegacyFunctionCall(&c.legacyFC, fc)
	}
}

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 sseLineReader 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
// n>1 的多路 choice 按 index 分桶聚合，各自保留正文与 finish_reason。
func Aggregate(r io.Reader) (map[string]any, error) {
	lr := newSSELineReader(r)
	var (
		id, model   string
		created     float64
		usage       map[string]any
		gotAnswer   bool // 本次流出现过客户端可见内容（role-only/元信息不算）
		validEvents int
		// sawChoice 标记是否真的见过非空 choices。Aggregate 会自行合成 choices，
		// 若只有正文却从未出现可解析 choice，说明帧结构不可交付，不能 200 放行。
		sawChoice bool
		sawDone   bool // 上游显式发过 data: [DONE]（正常收尾）
		// choicesByIndex 按 index 分桶；choiceOrder 保留首次出现顺序，输出前排序。
		choicesByIndex = map[int]*aggChoice{}
		choiceOrder    []int
	)
	choiceAt := func(idx int) *aggChoice {
		c := choicesByIndex[idx]
		if c == nil {
			c = &aggChoice{}
			choicesByIndex[idx] = c
			choiceOrder = append(choiceOrder, idx)
		}
		return c
	}
	for {
		line, err := lr.readLine()
		if err != nil && err != io.EOF {
			return nil, err
		}
		if payload := sseDataPayload(line); payload != nil {
			if bytes.Equal(payload, []byte("[DONE]")) {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				sawDone = true
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal(payload, &chunk) == nil {
					// 上游 error 帧（HTTP 200 已开流 + 首帧 error，真实形态）：
					// 不是回答正文。若继续走下面的 validEvents++，会合成出
					// content:"" + finish_reason:"stop" 的 200 假成功，客户端误判成
					// 「模型返回空」，且账号不被处置（限流号被当健康号继续选中）。
					// 原样带出帧原文，交给 handler 复用流式同一套 FrameKind/Classify
					// 分类与冷却分支（6004 只锁该模型，账号对其他模型仍可选）。
					if frameIsError(chunk) {
						return nil, &ErrFrame{Payload: string(payload)}
					}
					// 有效事件计数：只有真正可用的回答/收尾事件才算，不能用「JSON
					// 解析成功」代替。上游常见 {"choices":[]} 控制帧、纯元信息帧
					// 可解析但没有回答；若计入，最终会合成 200 空内容并记账成功。
					hasContent := frameHasUsableChoice(chunk)
					// 遗留 function_call 也会被下面并入聚合 message；它本身是
					// 客户端可见的工具调用，必须算有效事件，否则合法的
					// function_call-only 流会在 validEvents==0 处被误判为空流。
					if !hasContent && chunkHasLegacyFunctionCall(chunk) {
						hasContent = true
					}
					hasFinish := frameHasFinishReason(chunk)
					if hasContent || (hasFinish && gotAnswer) {
						validEvents++
					}
					if hasContent {
						gotAnswer = true
					}
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					// 只在非空对象时覆盖：上游常在正文流中间插一帧 `"usage":{}`
					// 占位，整体覆盖会把已收到的 prompt/completion/credit 抹掉，
					// 导致该次调用 token/credit 不入账、成本台账断档。
					if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						if len(ch) > 0 {
							sawChoice = true
						}
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							// 缺 index 的帧按 0 处理（单路流是绝大多数形态）。
							idx := 0
							if v, ok := c["index"].(float64); ok {
								idx = int(v)
							}
							cc := choiceAt(idx)
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								cc.finishReason = fr
								cc.sawTerminal = true
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									cc.role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									cc.appendContent(txt)
								}
								// 内容拒绝也属于回答正文：既要在最终闸门里可交付，也要
								// 出现在聚合 message 里，否则非流式响应会丢掉拒绝文本。
								if rc, ok := delta["refusal"].(string); ok && rc != "" {
									cc.refusal.WriteString(rc)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									cc.reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									cc.mergeToolCallsChunk(tcs)
								}
								if fc, ok := delta["function_call"].(map[string]any); ok {
									mergeLegacyFunctionCall(&cc.legacyFC, fc)
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）：
							// 整条并入（role/content/reasoning_content/tool_calls），
							// 与 delta 分支同构。本 choice 已取过正文则跳过
							// （避免与 delta 路径重复拼接——PR #134 的 latch 语义）。
							if msg, ok := c["message"].(map[string]any); ok && !cc.gotAnyContent {
								cc.mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, errEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	sort.Ints(choiceOrder)
	outChoices := make([]any, 0, len(choiceOrder))
	deliverable := false
	for _, idx := range choiceOrder {
		c := choicesByIndex[idx]
		rawFinish := c.finishReason
		if rawFinish == "" {
			rawFinish = "stop"
		}
		roleOut := c.role
		if roleOut == "" {
			roleOut = "assistant"
		}
		message := map[string]any{
			"role":    roleOut,
			"content": c.content.String(),
		}
		if c.refusal.Len() > 0 {
			message["refusal"] = c.refusal.String()
		}
		if c.reasoning.Len() > 0 {
			message["reasoning_content"] = c.reasoning.String()
		}
		if len(c.toolOrder) > 0 {
			sort.Ints(c.toolOrder)
			calls := make([]map[string]any, 0, len(c.toolOrder))
			for _, ti := range c.toolOrder {
				calls = append(calls, c.toolCalls[ti])
			}
			// P1b：流被截断时 tool_call 的 arguments 是残缺 JSON（解析失败），不把脏参数
			// 交给客户端——残留分片会被客户端解析成非法 JSON 卡死会话。截断的两个来源：
			//   - finish_reason=="length"（模型因 max_tokens 提前中止）；
			//   - 上游连接中断（EOF 收尾但未发 data: [DONE]，sawDone=false）。
			// 完整参数原样保留（正例零改动）；空参数（无参工具）不是截断，同样保留。
			if rawFinish == "length" || !sawDone {
				calls = dropTruncatedToolCalls(calls)
			}
			if len(calls) > 0 {
				message["tool_calls"] = calls
			}
		}
		if len(c.legacyFC) > 0 {
			name, _ := c.legacyFC["name"].(string)
			args, _ := c.legacyFC["arguments"].(string)
			// 遗留 function_call 与标准 tool_calls 同口径：截断时残缺参数不得交付。
			// 否则 finish_reason=length / 缺 [DONE] 的半截 JSON 会以 200 返回，
			// 客户端解析失败后卡死会话；清空后由下方最终闸门返回 errEmptyStream。
			if (rawFinish == "length" || !sawDone) && isTruncatedArguments(args) {
				args = ""
				name = ""
			}
			if name != "" || args != "" {
				message["function_call"] = c.legacyFC
			}
		}
		// 截断识别：只有「既没有 [DONE]、也没有任何显式收尾」才算连接中断——此时
		// 正文可能被截断，而 finish_reason 仍是默认 "stop"，客户端无法察觉。
		// 上游显式给了 finish_reason（含 stop）就是正常收尾，与流式路径的
		// !sawTerminal 判据保持同一口径；否则同一条上游流在流式/非流式两条路径上
		// 会给出互相矛盾的 finish_reason，非流式客户端据此误判截断并重复续写。
		finishOut := rawFinish
		if !sawDone && !c.sawTerminal && finishOut == "stop" && c.gotAnyContent {
			finishOut = "length"
		}
		// 聚合后的最终有效性校验：event 层看起来「有内容」不等于能交付给客户端。
		// 截断流里的 tool_calls 参数会被 dropTruncatedToolCalls 全部丢弃；上游也可能
		// 只发 {"index":0} 的空壳 tool_call。此时若直接构造响应，就会得到 content:""
		// 的空消息——正是非流式假成功。
		if aggregateHasDeliverableAnswer(c.content.String(), c.reasoning.String(), message) {
			deliverable = true
		}
		outChoices = append(outChoices, map[string]any{
			"index":         idx,
			"message":       message,
			"finish_reason": finishOut,
		})
	}
	if !deliverable || !sawChoice {
		return nil, errEmptyStream
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": outChoices,
	}
	if usage != nil {
		// OpenAI 非流式 usage 必含 total_tokens。上游若只发 prompt_tokens +
		// completion_tokens（部分上游末帧缺 total），网关合成补齐——否则严格按
		// schema 校验的客户端收不到 total_tokens。已有 total 或二者缺一不补
		// （不臆造：单边有值无法合成可信的 total）。
		resp["usage"] = normalizeUsageCacheAliases(ensureUsageTotal(usage))
	}
	return resp, nil
}

// aggregateHasDeliverableAnswer 报告聚合结果是否包含客户端实际可消费的回答。
// 非空 content/reasoning 或至少一个带名称/参数的 tool_call 才算成功；仅有
// role/finish_reason、被截断过滤后为空的 tool_calls、或上游只发 {"index":0} /
// 仅 id/type 的空壳调用，都不是回答。
func aggregateHasDeliverableAnswer(content, reasoning string, message map[string]any) bool {
	if content != "" || reasoning != "" {
		return true
	}
	// refusal 与正文同权：纯拒绝回答是有效交付物（见 deltaHasClientContent）。
	if txt, ok := message["refusal"].(string); ok && txt != "" {
		return true
	}
	for _, call := range aggregateToolCalls(message) {
		if toolCallHasPayload(call) {
			return true
		}
	}
	if fc, ok := message["function_call"].(map[string]any); ok && toolCallHasPayload(fc) {
		return true
	}
	return false
}

// aggregateToolCalls 把聚合 message 中的 tool_calls 归一成 []map[string]any。
// 正常路径写入的是 []map[string]any；保留 []any 分支兼容测试/调用方手工构造。
func aggregateToolCalls(message map[string]any) []map[string]any {
	switch calls := message["tool_calls"].(type) {
	case []map[string]any:
		return calls
	case []any:
		out := make([]map[string]any, 0, len(calls))
		for _, item := range calls {
			if call, ok := item.(map[string]any); ok {
				out = append(out, call)
			}
		}
		return out
	}
	return nil
}

// toolCallHasPayload 判断一个工具调用是否带可交付信息。id/type 单独存在不算
// 回答：上游可能只发 {"index":0} 的空壳分片，客户端无法据此执行任何工具。
// 名称或参数非空才算有效内容（空参数是合法的无参工具）。
func toolCallHasPayload(call map[string]any) bool {
	// 顶层 name/arguments 用于遗留 function_call；标准 tool_call 则放在
	// nested function 对象下。两种形态都必须识别，否则合法工具调用会被误判为空壳。
	if name, _ := call["name"].(string); name != "" {
		return true
	}
	if args, _ := call["arguments"].(string); args != "" {
		return true
	}
	if fn, ok := call["function"].(map[string]any); ok {
		if name, _ := fn["name"].(string); name != "" {
			return true
		}
		if args, _ := fn["arguments"].(string); args != "" {
			return true
		}
	}
	return false
}

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens 都在时
// 补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// num64 把 JSON number（float64/int64 均可）归一为 float64；非数字返回 ok=false。
//
// 注意与 payload.go 的类型 switch 语义不同：那边是翻译请求别名字段（非数字拒绝
// 整个请求），这边是聚合响应求和（非数字只跳过合成）。防后人合并两处。
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// mergeLegacyFunctionCall 合并遗留 function_call 分片：name 直覆盖（后续片通常
// 缺省），arguments 拼接（与 tool_calls 的 function.arguments 同语义）。
func mergeLegacyFunctionCall(dst *map[string]any, fc map[string]any) {
	if len(fc) == 0 {
		return
	}
	if *dst == nil {
		*dst = map[string]any{}
	}
	if v, ok := fc["name"].(string); ok && v != "" {
		(*dst)["name"] = v
	}
	if v, ok := fc["arguments"].(string); ok && v != "" {
		if prev, _ := (*dst)["arguments"].(string); prev != "" {
			(*dst)["arguments"] = prev + v
		} else {
			(*dst)["arguments"] = v
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已发过首片（与 name 是否非空无关）；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	stripToolCallNamesState(obj, &toolNameState{seenName: seen})
}

// toolNameState 跨帧记录 tool_call 的槽位归属与「该槽是否已发过非空 name」。
//
// 为什么不能用裸 map[int]bool：上游常有「tool_call 不带 index」的分片。此前缺
// index 一律按 0 处理，第二个并行调用首现时 seen[0] 已置位 → 它的 function.name
// 被当成重复分片删掉，客户端拿到空工具名、工具调用直接失败。这里按与
// mergeToolCallsChunk 相同的归属规则（index → id → 最近槽）解析槽位，并只在该槽
// 已发过**非空** name 之后才删后续 name 键（首片 name 为空、后续补 name 的兼容
// 形态也不会被误删）。
type toolNameState struct {
	seenName map[int]bool   // 槽位 → 已发过非空 name
	idSlot   map[string]int // id → 槽位
	last     int            // 最近一次用过的槽位（跨帧保留）
	hasLast  bool
	next     int  // 缺 index 且 id 为新时的分配游标
	base     bool // next 是否已初始化
}

// slot 解析一条 tool_call 分片应归属的槽位。
func (st *toolNameState) slot(tc map[string]any) int {
	cid, _ := tc["id"].(string)
	if v, ok := tc["index"].(float64); ok {
		idx := int(v)
		if cid != "" {
			if st.idSlot == nil {
				st.idSlot = map[string]int{}
			}
			st.idSlot[cid] = idx
		}
		st.last, st.hasLast = idx, true
		if !st.base || idx >= st.next {
			st.next, st.base = idx+1, true
		}
		return idx
	}
	if st.idSlot == nil {
		st.idSlot = map[string]int{}
	}
	if cid != "" {
		if idx, ok := st.idSlot[cid]; ok {
			st.last, st.hasLast = idx, true
			return idx
		}
		if !st.base {
			// 已知槽位数为 0..len(seenName)-1，新调用接在其后。
			st.next, st.base = len(st.seenName), true
		}
		idx := st.next
		st.next++
		st.idSlot[cid] = idx
		st.last, st.hasLast = idx, true
		return idx
	}
	if st.hasLast {
		return st.last
	}
	idx := st.next
	st.next++
	st.last, st.hasLast = idx, true
	return idx
}

func stripToolCallNamesState(obj map[string]any, st *toolNameState) {
	if st.seenName == nil {
		st.seenName = map[int]bool{}
	}
	// last 跨帧保留：无 id 的后续分片要延续「最近收到的调用」槽位。
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := st.slot(tc)
			fn, _ := tc["function"].(map[string]any)
			name := ""
			if fn != nil {
				if n, ok := fn["name"].(string); ok {
					name = n
				}
			}
			if name != "" && !st.seenName[idx] {
				// 该槽的首个非空 name：保留，记已发。
				st.seenName[idx] = true
				continue
			}
			if st.seenName[idx] {
				// 已发过 name：删除本分片的 name 键（存在即删，幂等）。
				if fn != nil {
					delete(fn, "name")
				}
			}
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			d, _ := c["delta"].(map[string]any)
			if d == nil {
				d, _ = c["message"].(map[string]any)
			}
			if d != nil {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if rawUsage, ok := obj["usage"]; ok {
		if u, ok := rawUsage.(map[string]any); ok {
			out["usage"] = normalizeUsageCacheAliases(u)
		} else {
			out["usage"] = rawUsage
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 成功头与 SSE headers 在首个有效内容帧写出时才提交；空流返回 errEmptyStream
// 且不写任何字节，调用方仍可返回真实错误状态码。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
//
// StreamHint 变体（gateway_hint 任务）：上游 error 帧透传时附加
// error.gateway_hint 字段——message 原文不动，hint 并列补充；hintFn 返回空串
// 或 nil 时与 Stream 行为逐字节一致。
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}

// fastFrameDisabled 仅供测试强制关闭快速路径，用同一输入对比新旧出站字节。
// 生产默认开启；fastFrame 只直出与 normalizeFrame+json.Marshal 逐字节等价的
// 规范键序帧，其余形态（含上游常见非规范键序）全部回退旧路径。
var fastFrameDisabled bool

// StreamOption StreamHint 的可选行为开关。
// WithErrorFrameObserver 只做旁路观测、不改透传字节；WithMarkupRepair 会改写出站
// 帧（把正文里的原生工具调用标记还原成 delta.tool_calls，见 dsml.go）。
type StreamOption func(*streamOptions)

type streamOptions struct {
	onErrorFrame func(payload string)
	// holdErrorUntilContent 为 true 时，在写出任何正文之前把上游 error 帧留在
	// 缓冲里，不写给客户端。调用方据此换号重试（客户端还没看到失败）。
	// 已经写出过正文后再遇到 error 帧，仍立即透传——半截回答不能换成另一个号接着写。
	holdErrorUntilContent bool
	markup                *MarkupRepair
}

// WithMarkupRepair 注册「模型原生工具调用标记」修复器（见 dsml.go）。
//
// 上游在没有 tools 的请求里会把工具调用吐成标记文本，Codex 这类只执行结构化
// function_call 的客户端于是把它当普通正文落进会话历史，回合直接结束、工具一次
// 都没跑。挂上修复器后，这类标记在透传前被还原成 delta.tool_calls。
//
// 传 nil、或传入未启用的修复器（客户端没声明过 tools）→ 与 Stream 行为逐字节一致。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = r }
}

// HoldErrorUntilContent 让 StreamHint 在首个正文帧之前扣住上游 error 帧。
// 返回的 ErrStreamHeld 携带该帧原文；响应头与 body 都还没写。
func HoldErrorUntilContent() StreamOption {
	return func(o *streamOptions) { o.holdErrorUntilContent = true }
}

// ErrStreamHeld 表示上游 error 帧出现在任何正文之前，且调用方要求先扣住。
// Payload 是 data 行原文（不含 "data: " 前缀）。
type ErrStreamHeld struct{ Payload string }

func (e *ErrStreamHeld) Error() string { return "upstream error frame held before content" }

// WithErrorFrameObserver 注册"上游以 error 帧报错"的观察者：透传该帧之前先用
// payload（原始 data 内容）回调一次。给调用方一个**账号处置**的挂载点——
// 上游「200 + error 帧」是真实形态（6004 限流、内容拦截、审核），此前网关在读
// 第一帧之前就把账号记成功，限流号被当成健康号；有观察者后可在流尾按帧分类处置。
func WithErrorFrameObserver(fn func(payload string)) StreamOption {
	return func(o *streamOptions) { o.onErrorFrame = fn }
}

// maxHeldStreamBytes 是「首个正文帧之前」允许缓冲的上游帧字节上限。缓冲的帧
// 不含正文（role-only/心跳/注释），1 MiB 足够容纳正常流的元信息，超出即丢弃。
const maxHeldStreamBytes = 1 << 20

// StreamHint 同 Stream，但上游 error 帧透出前把 hintFn(payload) 的返回值写入
// error.gateway_hint。hintFn 为 nil 或返回空串 → 原样透传（零改写）。
// 空流不写任何字节并返回 errEmptyStream：成功头只在首个有效内容写出时提交，
// 调用方因此可以返回真实 502/native 错误信封，而不是 200 + 空回答。
// opts：旁路观测开关（见 WithErrorFrameObserver），不改变任何透传字节。
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string, opts ...StreamOption) error {
	var o streamOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	fl, _ := w.(http.Flusher)
	// 成功头（200 + SSE）延迟到第一个真正写给客户端的字节。上游 200 但只有
	// role-only/心跳/空 choices 时一个字节都不写，调用方才能回真实 502（协议层
	// 还要能返回 JSON native 错误信封）；一旦写出正文，后续 error 帧必须原样
	// 透传，不能再换号拼流。
	started := false
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}

	// toolNames 跨帧记录 delta.tool_calls 的槽位归属与已发过的 name（见 toolNameState），
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolNames := &toolNameState{seenName: map[int]bool{}}
	// clientSawContent 一旦为 true，后续 error 帧必须透传：客户端已经在读回答，
	// 换号会把两个账号的输出拼进同一条流。
	clientSawContent := false
	// gotAnswer 跨帧记录本流是否出现过可用回答内容（正文/思考/tool_calls）。
	// 收尾帧只有在此为 true 时才计入有效帧，避免 role-only + stop 空流假成功。
	gotAnswer := false
	sawTerminal := false // explicit finish_reason or an upstream error frame
	// pending 扣住正文之前的帧（role-only 首帧、心跳、注释）。这些帧都不是可用
	// 回答，先写出去只会提交 200 成功头，让空流/正文前错误无法回真实状态码。
	// 有界：只保留最早的 maxHeldStreamBytes 字节，超出部分丢弃——被缓冲的帧按
	// 定义不含正文，丢弃不影响回答正确性，可防止恶意上游用无限心跳撑爆内存。
	var pending []string
	pendingBytes := 0

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-wb2api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID := ""

	// 标记修复（见 dsml.go）：把上游正文里的原生工具调用标记还原成 delta.tool_calls。
	// 客户端没声明过 tools 时 repair.Enabled() 为 false → 整条流零改写零分配。
	//   - synthIdx：本流已还原的调用数（合成的 index 从上游最大 index 之后接续）；
	//   - maxToolIdx：上游已用过的最大 tool_call index，避免合成的 index 与真实调用撞车；
	//   - toolFinishSent：是否已发出过 finish_reason: tool_calls，供流尾兜底。
	repair := o.markup
	synthIdx := 0
	maxToolIdx := -1
	toolFinishSent := false

	// writeDirect 原样写出一行并 flush（低层写出；调用方负责补 "data: " 前缀与帧分隔）。
	writeDirect := func(line string) error {
		ensureStarted()
		if _, werr := io.WriteString(w, line); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	// emit 在客户端可见正文出现前只缓冲：role-only 首帧、心跳、空 choices 控制帧
	// 都不能证明这是一次有效回答，先写出去就会提交 200 成功头，空流/正文前 error
	// 帧便无法再回真实错误码。首个正文帧到达时按原顺序 flush。
	emit := func(line string) error {
		if !clientSawContent {
			if pendingBytes+len(line) <= maxHeldStreamBytes {
				pending = append(pending, line)
				pendingBytes += len(line)
			}
			return nil
		}
		return writeDirect(line)
	}
	// writeSSE 把 payload 作为一帧 data 行写出并 flush（经 emit → 扣住态下先缓冲）。
	writeSSE := func(payload string) error {
		return emit("data: " + payload + "\n\n")
	}
	flushPending := func() error {
		if len(pending) == 0 {
			return nil
		}
		buf := pending
		pending = nil
		pendingBytes = 0
		for _, line := range buf {
			if werr := writeDirect(line); werr != nil {
				return werr
			}
		}
		return nil
	}
	// writeRaw 原样写出一帧（绕过 normalizeFrame）并 flush。上游 error 帧（error-passthrough）
	// 与空流错误帧需保留 error 字段，不能被白名单剥掉，故经此写出。
	// gateway_hint：上游 error 帧透出前按 hintFn 附加 error.gateway_hint 字段
	// （error 对象上加一个键，message/code/requestId 等原文不动；hintFn 为
	// nil / 空串 / 非 JSON 帧 → 原样写出，零改写）。
	writeRaw := func(payload string) error {
		if hint := frameGatewayHint(hintFn, payload); hint != "" {
			payload = attachHintToErrorFrame(payload, hint)
		}
		return writeDirect("data: " + payload + "\n\n")
	}

	// writeChunk 把一个 chunk 对象按规范白名单重建后写出并 flush，同时维护三件跨帧
	// 状态：tool_calls name 收敛（每 index 仅首片带 name）、消息 id 续传、以及上游
	// 已用过的最大 tool_call index（供标记修复接续编号）。
	// fallback 是规范化失败时的兜底原文（与修复前「marshal 失败即原样透传」一致）。
	writeChunk := func(obj map[string]any, fallback string) error {
		// 先按 index 收敛 tool_calls name（每 index 仅首片保留，后续分片删 name 键），再规范化透传。
		stripToolCallNamesState(obj, toolNames)
		// id 续传：首帧非空真实 id 缓存；后续帧缺 id / 空 id 一律用缓存值，
		// 有自己 id 的帧保持原样（不同流分裂的帧允许各自 id）。
		if firstID == "" {
			if v, ok := obj["id"].(string); ok && v != "" {
				firstID = v
			}
		} else if v, ok := obj["id"].(string); !ok || v == "" {
			obj["id"] = firstID
		}
		if chs, ok := obj["choices"].([]any); ok {
			for _, ci := range chs {
				c, _ := ci.(map[string]any)
				if c == nil {
					continue
				}
				d, _ := c["delta"].(map[string]any)
				if d == nil {
					continue
				}
				tcs, _ := d["tool_calls"].([]any)
				for _, tci := range tcs {
					tc, _ := tci.(map[string]any)
					if tc == nil {
						continue
					}
					if v, ok := tc["index"].(float64); ok && int(v) > maxToolIdx {
						maxToolIdx = int(v)
					}
				}
			}
		}
		raw, err := json.Marshal(normalizeFrame(obj))
		if err != nil {
			return writeSSE(fallback)
		}
		return writeSSE(string(raw))
	}

	// repairFrame 在透传前跑标记修复，并把一帧展开成「可能多帧」：正文帧（标记被摘掉
	// 后的可见文本）+ N 个 delta.tool_calls 帧 + 收尾 finish_reason 帧。没有命中标记
	// 时原样返回入参帧（单元素），与修复前逐字节一致。
	repairFrame := func(obj map[string]any) []map[string]any {
		if !fastFrameDisabled && !repair.Enabled() {
			return []map[string]any{obj}
		}
		chs, ok := obj["choices"].([]any)
		if !ok {
			return []map[string]any{obj}
		}
		var calls []MarkupCall
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			d, ok := c["delta"].(map[string]any)
			if !ok {
				continue
			}
			txt, ok := d["content"].(string)
			if !ok || txt == "" {
				continue
			}
			emit, got := repair.Feed(txt)
			calls = append(calls, got...)
			if emit == "" {
				delete(d, "content")
			} else {
				d["content"] = emit
			}
		}
		if len(calls) == 0 {
			// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
			// tool_calls，客户端据此知道「回合以工具调用结束」而不是「模型只说了话」。
			if synthIdx > 0 && rewriteFinishReason(obj, "tool_calls") {
				toolFinishSent = true
			}
			return []map[string]any{obj}
		}
		// 命中：finish_reason 必须挪到工具调用帧之后，否则客户端在读到调用之前就
		// 认为回合已经结束。注意「本帧本来没有 finish_reason」时**不得**补一帧收尾
		// ——上游的收尾帧随后就到，补了会变成两个 finish_reason（后续那个由
		// rewriteFinishReason 改写）。
		fr := takeFinishReason(obj)
		out := []map[string]any{obj}
		for _, call := range calls {
			idx := maxToolIdx + 1 + synthIdx
			synthIdx++
			out = append(out, markupCallFrame(obj, idx, call))
		}
		if fr != "" {
			if fr == "stop" {
				fr = "tool_calls"
			}
			out = append(out, finishOnlyFrame(obj, fr))
			toolFinishSent = fr == "tool_calls"
		}
		return out
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 只有真正可用的回答/收尾帧才计数（JSON 解析失败照常降级原样写出，但不计数）。
	writeFrame := func(payload string) (int, error) {
		if !repair.Enabled() {
			payloadBytes := []byte(payload)
			if raw, kind, ok := fastFrame(payloadBytes, &firstID, gotAnswer, nil); ok {
				switch kind {
				case fastFrameContent:
					if werr := flushPending(); werr != nil {
						return 0, werr
					}
					clientSawContent = true
					gotAnswer = true
					if werr := writeSSE(string(raw)); werr != nil {
						return 0, werr
					}
					return 1, nil
				case fastFrameFinish:
					sawTerminal = true
					if werr := writeSSE(string(raw)); werr != nil {
						return 0, werr
					}
					return 1, nil
				default:
					if werr := writeSSE(string(raw)); werr != nil {
						return 0, werr
					}
					return 0, nil
				}
			}
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			// 非 JSON 帧：原样降级写出，不计数（与修复前一致）。扣住态下同样缓冲。
			if werr := emit("data: " + payload + "\n\n"); werr != nil {
				return 0, werr
			}
			return 0, nil
		}
		// Some providers send full message objects inside SSE. Promote them
		// before repair/name handling so detection and forwarded content agree.
		if choices, ok := obj["choices"].([]any); ok {
			for _, raw := range choices {
				choice, _ := raw.(map[string]any)
				if choice == nil {
					continue
				}
				if delta, _ := choice["delta"].(map[string]any); delta == nil {
					if msg, ok := choice["message"].(map[string]any); ok {
						choice["delta"] = msg
					}
				}
			}
		}
		if frameHasFinishReason(obj) {
			sawTerminal = true
		}
		// 上游错误帧透传（error-passthrough）：带 error 键的帧**原样写出**，不走
		// normalizeFrame 白名单——白名单会剥掉 error 字段，客户端就看不到上游
		// code/msg/requestId。error.message 即上游原文（如 6004 限流、审核拦截），
		// 计入有效帧（避免误判空流补写 "empty upstream stream"）。
		if frameIsError(obj) {
			sawTerminal = true
			// 正文还没写出：扣住这一帧，让调用方换号。不写响应头、不通知观察者
			// （观察者挂在「已经透传」之后；换号成功时客户端不该看到这次失败）。
			// OpenAI 形态（{"error":{...}}）与业务信封形态（{"code":<非0>,"msg":...}）
			// 都算；choices 里夹带的 error 不在此列。
			if o.holdErrorUntilContent && !clientSawContent {
				return 0, &ErrStreamHeld{Payload: payload}
			}
			// 非扣住态：先按原顺序写出此前缓冲的 role-only/注释，再透传 error 帧。
			if werr := flushPending(); werr != nil {
				return 0, werr
			}
			// 旁路通知（账号处置用）：原始 payload 交给观察者，透传字节不变。
			if o.onErrorFrame != nil {
				o.onErrorFrame(payload)
			}
			if werr := writeRaw(payload); werr != nil {
				return 0, werr
			}
			return 1, nil
		}
		// 正文首帧：先提交扣住的缓冲（限流换号场景下这些字节一直没写出去）。
		if frameHasClientContent(obj) {
			clientSawContent = true
			if werr := flushPending(); werr != nil {
				return 0, werr
			}
		}
		hasContent := frameHasUsableChoice(obj)
		if hasContent {
			gotAnswer = true
		}
		// 收尾帧只有在已经出现正文/思考/tool_calls 后才算有效；单独的空 finish
		// 帧与 role-only 首帧不能构成一次回答。
		usable := hasContent || (frameHasFinishReason(obj) && gotAnswer)
		for _, ch := range repairFrame(obj) {
			if werr := writeChunk(ch, payload); werr != nil {
				return 0, werr
			}
		}
		if usable {
			return 1, nil
		}
		return 0, nil
	}

	lr := newSSELineReader(r)
	validFrames := 0
	sawDone := false
readLoop:
	for {
		line, err := lr.readLine()
		trimmed := bytes.TrimRight(line, "\r\n")
		if payload := sseDataPayload(trimmed); payload != nil {
			if bytes.Equal(payload, []byte("[DONE]")) {
				sawDone = true
				// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
				// [DONE] 统一在循环结束后写出，保证恰好一个。
				break readLoop
			}
			n, werr := writeFrame(string(payload))
			validFrames += n
			if werr != nil {
				return werr
			}
			continue
		}
		if len(trimmed) != 0 {
			// 注释/其他行：正文前与 role 帧一起缓冲，避免限流帧出现前已提交响应。
			if werr := emit(string(line)); werr != nil {
				return werr
			}
		}
		// 空行（帧分隔）吞掉：本函数自产 "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			// 读上游失败（连接中断 / 空闲看门狗掐流 / 读超时）：用 StreamReadError
			// 包一层，与写客户端失败区分——handler 据此不把该次尝试记成账号成功。
			return &StreamReadError{Err: err}
		}
	}
	// 空流（0 有效帧）：一个字节都不写——不提交 200 SSE，也不补 error 帧/[DONE]。
	// 调用方据此回真实 502（协议层还要转成 native 错误信封）。此前先写 error 帧
	// 会提交 200，客户端把上游缺陷误读成一次成功的空回答。
	if validFrames == 0 {
		return errEmptyStream
	}
	// A connection closing after content is not proof of completion. Keep
	// compatibility with providers omitting DONE only when they sent an explicit
	// finish_reason (or error); otherwise expose a truncated upstream stream.
	if !sawDone && !sawTerminal {
		return &StreamReadError{Err: io.ErrUnexpectedEOF}
	}
	if werr := flushPending(); werr != nil {
		return werr
	}
	// 标记修复的尾部收尾：流结束时仍未判定的字节一律原文回吐（绝不吞字节，未闭合
	// 的块连起始标记一起交还）；若本流还原过调用、但上游从未给出收尾帧，补一帧
	// finish_reason: tool_calls，保证客户端不会把「工具调用回合」读成「模型只说了话」。
	if repair.Enabled() {
		if tail := repair.Flush(); tail != "" {
			tailObj := map[string]any{
				"object": "chat.completion.chunk",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": tail},
				}},
			}
			if werr := writeChunk(tailObj, ""); werr != nil {
				return werr
			}
			validFrames++
		}
		if synthIdx > 0 && !toolFinishSent {
			if werr := writeChunk(finishOnlyFrame(map[string]any{}, "tool_calls"), ""); werr != nil {
				return werr
			}
		}
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// frameIsError 报告该帧是否为「上游报错帧」，覆盖上游两种真实形态：
//
//	形态一（OpenAI 形态）：顶层 error 为非 nil 的 error 对象，如 {"error":{"code":6004,...}}。
//	  注意 "error": null 必须判为**非**错误帧——上游正常收尾帧会显式把 error 置空，
//	  此前用 `_, hasErr := obj["error"]` 只判键存在，把这种帧误判成错误帧 → 非流式
//	  Aggregate 直接返回 ErrFrame（把正常响应变成失败），流式 StreamHint 扣住并当作
//	  限流帧错误处置账号。
//
//	形态二（业务信封形态）：顶层 code 为**数值非 0**，如
//	  {"code":6004,"msg":"The model provider is rate-limiting requests."}。
//	  这类信封是上游网关/供应商的业务错误回执，**没有** error 对象，也不是模型回答正文。
//	  若当普通数据帧处理：非流式 Aggregate 会合成 content:"" + finish_reason:"stop" 的
//	  200 空内容假成功；流式 StreamHint 走 normalizeFrame 白名单把 code/msg 剥掉后转发
//	  一个空 chunk 假成功——两条路径都会把被限流的账号记成健康、粘性钉死，后续每轮都失败。
//
// 「业务信封」判定需**同时**满足两个条件，缺一不可：
//  1. 顶层 code 存在且数值非 0；code == 0 / code 缺失 → 非错误（{"code":0,...} 是
//     上游常见的心跳/元信息帧）。
//  2. 该帧**没有**可用 choices（obj["choices"] 缺失或为空数组）；带 choices 的帧即便
//     同时夹带 code 也仍是回答正文，不能误杀。
//
// code 编码兼容数字（6004）与字符串（"6004"）：统一 fmt.Sprint 后比较，并容忍前后空白
// （上游偶尔输出 " 6004 "）。
func frameIsError(obj map[string]any) bool {
	// 形态一：顶层 error 对象（"error":null 视为非错误，既有行为保持不变）。
	if v, ok := obj["error"]; ok && v != nil {
		if m, ok := v.(map[string]any); ok {
			return len(m) > 0
		}
		return true
	}
	// 带可用 choices 的帧是回答正文：即便夹带 code 也不判为错误。
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		return false
	}
	// 形态二：业务信封——code 存在且数值非 0。
	code, ok := obj["code"]
	if !ok || code == nil {
		return false
	}
	trimmed := strings.TrimSpace(fmt.Sprint(code))
	return trimmed != "" && trimmed != "0"
}

// chunkHasLegacyFunctionCall 报告帧的 delta/message 是否带非空遗留
// function_call。聚合器现在会合并该形态，因此它也必须计入有效事件。
func chunkHasLegacyFunctionCall(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if delta, ok := c["delta"].(map[string]any); ok {
			if fc, ok := delta["function_call"].(map[string]any); ok && len(fc) > 0 {
				return true
			}
		}
		if msg, ok := c["message"].(map[string]any); ok {
			if fc, ok := msg["function_call"].(map[string]any); ok && len(fc) > 0 {
				return true
			}
		}
	}
	return false
}

// frameHasUsableChoice 报告帧是否包含真正可用的 choice（正文、思考、
// tool_calls/function_call）。空 choices、纯元信息/心跳帧、业务信封（无 choices）
// 不算——它们不能证明上游完成了一次有效回答。
//
// role-only 首帧与单独的空收尾帧也不算：上游可能只发 {"delta":{"role":"assistant"}}
// 或 {"finish_reason":"stop"} 就结束，客户端实际没有任何回答。finish_reason 的
// 有效性由调用方结合「此前是否见过正文」判定（见 Aggregate/StreamHint）。
func frameHasUsableChoice(obj map[string]any) bool {
	if frameIsError(obj) {
		return false
	}
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if delta, ok := c["delta"].(map[string]any); ok && deltaHasUsableSignal(delta) {
			return true
		}
		if msg, ok := c["message"].(map[string]any); ok && deltaHasUsableSignal(msg) {
			return true
		}
	}
	return false
}

// deltaHasUsableSignal 是 delta/message 的「有可用内容」判据：正文、思考内容、
// tool_calls、遗留 function_call。空对象/空串/仅 role 不算。
func deltaHasUsableSignal(delta map[string]any) bool {
	return deltaHasClientContent(delta)
}

// frameHasFinishReason 报告帧是否携带非空 finish_reason（收尾语义）。
func frameHasFinishReason(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			return true
		}
	}
	return false
}

// frameHasClientContent 报告这一帧是否已经是客户端可见的回答。
// role-only 首帧（delta 只有 role，或 content 为空串）不算：上游常在限流 error
// 帧之前先发一帧空 role，此时客户端还没有正文，换号不会拼出半截回答。
func frameHasClientContent(obj map[string]any) bool {
	if frameIsError(obj) {
		return false
	}
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if delta, ok := c["delta"].(map[string]any); ok && deltaHasClientContent(delta) {
			return true
		}
		if msg, ok := c["message"].(map[string]any); ok && deltaHasClientContent(msg) {
			return true
		}
	}
	return false
}

func deltaHasClientContent(delta map[string]any) bool {
	if txt, ok := delta["content"].(string); ok && txt != "" {
		return true
	}
	if txt, ok := delta["reasoning_content"].(string); ok && txt != "" {
		return true
	}
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		return true
	}
	// 遗留 function_call（非 tool_calls 数组形态）：也是客户端可见的回答内容，
	// 此前漏判会让「首帧即 function_call」的流被当成还没有正文。
	if fc, ok := delta["function_call"].(map[string]any); ok && len(fc) > 0 {
		return true
	}
	// 内容拒绝（OpenAI refusal 字段）是合法的模型回答：normalizeFrame 明确保留
	// 该字段、Responses/Messages 转换也会把它映射成 stop_reason=refusal。若不计入
	// 客户端可见内容，纯 refusal 流会被当成空流回 502，客户端把「模型礼貌拒绝」
	// 误读成网关故障并重试，账号还会被记一次失败。
	if txt, ok := delta["refusal"].(string); ok && txt != "" {
		return true
	}
	return false
}

// frameGatewayHint 取 error 帧的 gateway_hint（hintFn 缺失/异常返回空串 → 不附加）。
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic 隔离：hint 判定是补充功能，任何实现缺陷不得击穿流透传主路径。
	defer func() { _ = recover() }()
	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame 在 error 帧的 error 对象上附加 gateway_hint 字段。
// message/code/requestId 等既有键原样保留（只加不改）；非 JSON / 无 error 对象 →
// payload 原样返回（宁可不加 hint 也不破坏原文透传）。
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}

// takeFinishReason 取出并删除所有 choice 的 finish_reason，返回第一个非空值。
// 标记修复把一帧拆成多帧时需要它：finish_reason 必须落在工具调用帧之后，否则
// 客户端在读到调用之前就认为回合结束了。
func takeFinishReason(obj map[string]any) string {
	fr := ""
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// rewriteFinishReason 把 choice 上「已有且为 stop」的 finish_reason 改写成 want。
// 本来就没有 finish_reason 的中间帧一律不动（绝不凭空造收尾），已有其他值
// （length / content_filter 等）时也不动——那些收尾语义更需要被客户端看到。
// 返回是否发生了改写。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v == "stop" {
			c["finish_reason"] = want
			changed = true
		}
	}
	return changed
}

// markupCallFrame 以 src 帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
// id 不在这里写：交给 writeChunk 的 id 续传逻辑补齐（src 帧先写出，firstID 已就位）。
func markupCallFrame(src map[string]any, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// finishOnlyFrame 构造一帧只带 finish_reason 的收尾 chunk（空 delta）。
func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

package upstream

import (
	"strings"
	"testing"
)

// TestStripToolCallNamesNoIndexParallel 上游省略 index 时，两个并行调用的
// function.name 都必须只出现一次且各自保留。此前缺 index 一律归 0，第二个
// 调用首现即被视为「重复分片」，name 被删 → 客户端拿到空工具名、调用失败。
func TestStripToolCallNamesNoIndexParallel(t *testing.T) {
	st := &toolNameState{seenName: map[int]bool{}}
	frame := func(calls ...map[string]any) map[string]any {
		arr := make([]any, 0, len(calls))
		for _, c := range calls {
			arr = append(arr, c)
		}
		return map[string]any{"choices": []any{
			map[string]any{"delta": map[string]any{"tool_calls": arr}},
		}}
	}
	nameOf := func(f map[string]any, i int) (string, bool) {
		tc := f["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[i].(map[string]any)
		fn, _ := tc["function"].(map[string]any)
		v, ok := fn["name"]
		s, _ := v.(string)
		return s, ok
	}

	// 两个并行调用：都不带 index，各自带 id。
	f1 := frame(map[string]any{"id": "call_1", "function": map[string]any{"name": "read_file", "arguments": ""}})
	stripToolCallNamesState(f1, st)
	if got, ok := nameOf(f1, 0); !ok || got != "read_file" {
		t.Fatalf("first call name=%q ok=%v want read_file", got, ok)
	}

	f2 := frame(map[string]any{"id": "call_2", "function": map[string]any{"name": "write_file", "arguments": ""}})
	stripToolCallNamesState(f2, st)
	if got, ok := nameOf(f2, 0); !ok || got != "write_file" {
		t.Fatalf("second call name=%q ok=%v want write_file（缺 index 不得归并到槽 0）", got, ok)
	}

	// 后续无 id 的参数分片延续最近槽位，name 键不重复下发。
	f3 := frame(map[string]any{"function": map[string]any{"arguments": `{"p":"x"}`}})
	stripToolCallNamesState(f3, st)
	if _, ok := nameOf(f3, 0); ok {
		t.Fatal("continuation fragment should not repeat function.name")
	}
}

// TestStripToolCallNamesNameArrivesLate 首片只给 id/arguments、name 在第二片：
// 该 name 必须保留（此前只看「该 index 是否见过分片」，会把迟到 name 删掉）。
func TestStripToolCallNamesNameArrivesLate(t *testing.T) {
	st := &toolNameState{seenName: map[int]bool{}}
	mk := func(tc map[string]any) map[string]any {
		return map[string]any{"choices": []any{
			map[string]any{"delta": map[string]any{"tool_calls": []any{tc}}},
		}}
	}
	first := mk(map[string]any{"index": float64(0), "id": "c1", "function": map[string]any{"arguments": ""}})
	stripToolCallNamesState(first, st)
	late := mk(map[string]any{"index": float64(0), "function": map[string]any{"name": "read_file"}})
	stripToolCallNamesState(late, st)
	fn := late["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read_file" {
		t.Fatalf("late name=%v want read_file", fn["name"])
	}
}

// TestAggregateExplicitStopWithoutDoneKeepsStop 上游显式 finish_reason=stop 但
// 省略 data: [DONE] 时不得改写成 length：流式路径按「有显式收尾即正常」处理，
// 非流式若改成 length，客户端会误判截断并重复续写。
func TestAggregateExplicitStopWithoutDoneKeepsStop(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"complete answer"}}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate err=%v", err)
	}
	fr := resp["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if fr != "stop" {
		t.Fatalf("finish_reason=%v want stop（显式收尾不得改写成 length）", fr)
	}
}

// TestAggregateNoDoneNoFinishIsLength 既无 [DONE] 也无显式收尾才是截断。
func TestAggregateNoDoneNoFinishIsLength(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		``,
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate err=%v", err)
	}
	fr := resp["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if fr != "length" {
		t.Fatalf("finish_reason=%v want length", fr)
	}
}

// TestAggregateRefusalOnlyIsDeliverable 纯内容拒绝（refusal 字段）是合法回答，
// 不能被判成空流。
func TestAggregateRefusalOnlyIsDeliverable(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"refusal":"我无法协助该请求。"}}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("refusal-only stream must not be empty: %v", err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["refusal"] != "我无法协助该请求。" {
		t.Fatalf("refusal=%v", msg["refusal"])
	}
}

// TestAggregateEmptyUsageDoesNotClobber 空 usage 对象不得覆盖已收到的完整用量，
// 否则该次调用 token/credit 不入账。
func TestAggregateEmptyUsageDoesNotClobber(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate err=%v", err)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["total_tokens"] != float64(12) {
		t.Fatalf("usage=%v want prompt_tokens=10 total_tokens=12", usage)
	}
}

// TestAggregateMultipleChoicesKeepSeparate 钉住 n>1：两路 choice 各带自己的
// content 与 finish_reason，必须原样分成两条 choices，不能拼成一段正文再只报
// 一个 index=0（旧行为会静默丢失一路回答，且 finish_reason 只保留最后一路）。
func TestAggregateMultipleChoicesKeepSeparate(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"甲回答"}}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":1,"delta":{"role":"assistant","content":"乙回答"}}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":1,"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate err=%v", err)
	}
	choices := resp["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("choices=%d want 2（n>1 必须分桶保留）", len(choices))
	}
	type row struct {
		content string
		finish  any
	}
	got := map[int]row{}
	for _, ci := range choices {
		c := ci.(map[string]any)
		msg := c["message"].(map[string]any)
		got[c["index"].(int)] = row{content: msg["content"].(string), finish: c["finish_reason"]}
	}
	if r := got[0]; r.content != "甲回答" || r.finish != "stop" {
		t.Fatalf("choice0=%+v want content=甲回答 finish=stop", r)
	}
	if r := got[1]; r.content != "乙回答" || r.finish != "length" {
		t.Fatalf("choice1=%+v want content=乙回答 finish=length", r)
	}
}

// TestAggregateMultipleChoicesPerChoiceToolCalls 两路 choice 各自的 tool_calls
// 不能串到对方身上（分桶后各 choice 有自己的 index 空间）。
func TestAggregateMultipleChoicesPerChoiceToolCalls(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a1","type":"function","function":{"name":"read_file","arguments":"{\"p\":\"1\"}"}}]}}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"b1","type":"function","function":{"name":"write_file","arguments":"{\"p\":\"2\"}"}}]}}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"c1","model":"glm-5.2","choices":[{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("aggregate err=%v", err)
	}
	for _, ci := range resp["choices"].([]any) {
		c := ci.(map[string]any)
		idx := c["index"].(int)
		msg := c["message"].(map[string]any)
		calls, _ := msg["tool_calls"].([]map[string]any)
		if len(calls) != 1 {
			t.Fatalf("choice%d tool_calls=%d want 1", idx, len(calls))
		}
		fn := calls[0]["function"].(map[string]any)
		want := map[int]string{0: "read_file", 1: "write_file"}[idx]
		if fn["name"] != want {
			t.Fatalf("choice%d tool name=%v want %s（两路 tool_calls 串了）", idx, fn["name"], want)
		}
	}
}

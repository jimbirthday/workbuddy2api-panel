package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"none defaults to high when supported",
			`{"model":"glm-5.2","reasoning_effort":"none"}`, efforts, "reasoning_effort", "high"},
		{"none passes through when high unsupported",
			`{"model":"glm-5.2-mini","reasoning_effort":"none"}`, efforts, "reasoning_effort", "none"},
		{"camelCase none defaults to high when supported",
			`{"model":"glm-5.2","reasoningEffort":"none"}`, efforts, "reasoningEffort", "high"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

// TestPrepareBodySingleDecodeEquivalent 验证 prepareBody 的单次解码/编码管线
// 与旧的两段字节管线和独立的 global system 步骤逐字节等价，覆盖全部关键语义。
func TestPrepareBodySingleDecodeEquivalent(t *testing.T) {
	c := &Client{}
	c.SanitizeFingerprints.Store(false)
	c.efforts = map[string]map[string][]string{
		"cn": {"glm-5.2": {"low", "high"}},
	}
	c.defaultEfforts = map[string]map[string]string{
		"cn": {"glm-5.2": "high"},
	}
	cases := []struct {
		name          string
		body          string
		conv          string
		consoleSystem bool
	}{
		{
			name: "normalization and generated cache key",
			body: `{"model":"glm-5.2","reasoning_effort":"max","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"f"}},"tools":[{"type":"function","function":{"name":"f"}}]}`,
			conv: "conv-42",
		},
		{
			name:          "global system injection",
			body:          `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`,
			conv:          "conv-42",
			consoleSystem: true,
		},
		{
			name:          "global system not duplicated",
			body:          `{"model":"glm-5.2","messages":[{"role":"system","content":"already"},{"role":"user","content":"hi"}]}`,
			conv:          "conv-42",
			consoleSystem: true,
		},
		{
			name:          "global empty messages unchanged",
			body:          `{"model":"glm-5.2","messages":[]}`,
			conv:          "conv-42",
			consoleSystem: true,
		},
		{
			name: "existing cache key preserved",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"client-key"}`,
			conv: "conv-42",
		},
		{
			name: "body conversation id precedence",
			body: `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"conversation_id":"body-conv"}`,
			conv: "header-conv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.prepareBody([]byte(tc.body), "cn", "user-abc-123", tc.conv, tc.consoleSystem)
			want := PrepareBodyOptWithEffortsAndDefault([]byte(tc.body), false, c.efforts["cn"], c.defaultEfforts["cn"])
			want = InjectPromptCacheKey(want, "user-abc-123", tc.conv)
			if tc.consoleSystem {
				want = ensureConsoleSystem(want)
			}
			if string(got) != string(want) {
				t.Fatalf("single-pass body differs from legacy pipeline:\ngot:  %s\nwant: %s", got, want)
			}
		})
	}
}

// TestPrepareBodyBadJSONPassthrough 坏 JSON 必须在统一入口原样回退，不因新管线被改写。
func TestPrepareBodyBadJSONPassthrough(t *testing.T) {
	c := &Client{}
	c.SanitizeFingerprints.Store(false)
	in := []byte(`{"model":"glm-5.2",`)
	if got := c.prepareBody(in, "global", "uid", "conv", true); string(got) != string(in) {
		t.Fatalf("bad JSON altered: got=%q want=%q", got, in)
	}
	if got := c.prepareBody(nil, "cn", "uid", "conv", false); len(got) != 0 {
		t.Fatalf("nil body altered: %q", got)
	}
}

// TestPrepareBodyGlobalEffortFallbackMatchesLegacy global 域远端桶缺该模型时，
// effortFor 单键查找必须与旧 globalEffortMap 合并结果一致（静态兜底优先补齐），
// 且远端桶有值时权威覆盖；同时确保不会整表复制。
func TestPrepareBodyGlobalEffortFallbackMatchesLegacy(t *testing.T) {
	c := &Client{}
	c.SanitizeFingerprints.Store(false)
	c.efforts = map[string]map[string][]string{
		"global": {"gpt-6-astra": {"low", "max"}},
	}
	c.defaultEfforts = map[string]map[string]string{
		"global": {"gpt-6-astra": "max"},
	}
	in := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	got := c.prepareBody(in, "global", "uid", "conv", false)
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "high" {
		t.Fatalf("global static fallback must downgrade low->high, got %v body=%s", obj["reasoning_effort"], got)
	}

	in = []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
	got = c.prepareBody(in, "global", "uid", "conv", false)
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["reasoning_effort"] != "low" {
		t.Fatalf("remote bucket must override static fallback, got %v body=%s", obj["reasoning_effort"], got)
	}
}

// TestChatStreamGlobalInjectsConsoleSystemOnce 覆盖 wire 级语义：global 请求
// 首条非 system 时注入且只注入一条；首条已是 system 时不重复注入。
func TestChatStreamGlobalInjectsConsoleSystemOnce(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantSystems  int
		wantFirstSys bool
	}{
		{"inject once", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`, 1, true},
		{"no duplicate", `{"model":"glm-5.2","messages":[{"role":"system","content":"already"},{"role":"user","content":"hi"}]}`, 1, true},
		{"no messages", `{"model":"glm-5.2","messages":[]}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire map[string]any
			c := testClient(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Fatalf("wire body: %v", err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
			})
			c.GlobalEnabled = true
			c.ChatBaseGlobal = "https://global.example"
			a := &auth.Auth{AccessToken: "at", UID: "u1"}
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				t.Fatalf("set realm: %v", err)
			}
			rc, status, respBody, err := c.ChatStream(a, []byte(tc.body), "", ChatMeta{ConversationID: "conv"})
			if err != nil || status != 200 {
				t.Fatalf("chat: status=%d body=%s err=%v", status, respBody, err)
			}
			rc.Close()
			msgs, _ := wire["messages"].([]any)
			systems := 0
			for _, raw := range msgs {
				msg, _ := raw.(map[string]any)
				if role, _ := msg["role"].(string); role == "system" {
					systems++
				}
			}
			if systems != tc.wantSystems {
				t.Fatalf("system messages=%d want %d: %v", systems, tc.wantSystems, msgs)
			}
			if tc.wantFirstSys && len(msgs) > 0 {
				first, _ := msgs[0].(map[string]any)
				if role, _ := first["role"].(string); role != "system" {
					t.Fatalf("first message role=%v want system", first["role"])
				}
			}
		})
	}
}

// TestPrepareBodyEmptyObject 空对象是合法 JSON，必须走完整管线而不是坏 JSON 回退。
func TestPrepareBodyEmptyObject(t *testing.T) {
	c := &Client{}
	c.SanitizeFingerprints.Store(false)
	out := c.prepareBody([]byte(`{}`), "cn", "uid", "conv", false)
	obj, err := decodeBody(out)
	if err != nil {
		t.Fatalf("out not json: %v", err)
	}
	if obj["stream"] != true {
		t.Fatalf("stream not forced: %v", obj)
	}
	if got, _ := obj["prompt_cache_key"].(string); got == "" {
		t.Fatalf("cache key missing: %v", obj)
	}
}

// TestPrepareBodyDeterministic 序列化稳定性：同输入跑多遍出站字节级一致
// （prompt_cache_key 前缀命中的前提——链中不得注入时间/随机/ID 类不确定源）。
func TestPrepareBodyDeterministic(t *testing.T) {
	inputs := []string{
		`{"model":"glm-5.2","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4","messages":[{"role":"user","content":"写个函数"}],"tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"glm-5.3","messages":[{"role":"developer","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for i, in := range inputs {
		var first []byte
		for round := 0; round < 5; round++ {
			out := PrepareBodyOptWithEfforts([]byte(in), true, map[string][]string{"glm-5.2": {"off", "low", "high"}})
			if round == 0 {
				first = out
				continue
			}
			if string(out) != string(first) {
				t.Fatalf("input #%d round %d differs from round 0:\n%s\n%s", i, round, first, out)
			}
		}
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// TestNormalizeToolPatterns 工具 schema pattern 的 `\_` 转义归一（11129 实案：
// exa agent_run 的 `^agent\_run\_` 被 deepseek 系确定性拒收，归一后上游 200）。
// 覆盖：function.parameters 与裸 parameters 两形态、嵌套 schema、patternProperties
// 键、消息正文不碰、无 tools no-op、sanitize 开关两态一致。
func TestNormalizeToolPatterns(t *testing.T) {
	lookupPattern := func(t *testing.T, body string, path ...any) string {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		cur := any(obj)
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("path %v: want object, got %T", path, cur)
				}
				cur = m[k]
			case int:
				l, ok := cur.([]any)
				if !ok || k >= len(l) {
					t.Fatalf("path %v: bad array at %T", path, cur)
				}
				cur = l[k]
			}
		}
		s, _ := cur.(string)
		return s
	}

	t.Run("exa agent_run pattern normalized", func(t *testing.T) {
		bs := string(byte(92))                                                                                                                                                                                                                                                                                                                // 反斜杠，测试体经工具链多层转义易被吞，运行时拼装保真
		body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"agent_run","parameters":{"type":"object","properties":{"runId":{"type":"string","pattern":"^agent` + bs + bs + `_run` + bs + bs + `_"}` + `,"query":{"type":"string"}},"required":["query"]}}}]}` //nolint:lll // 实案 body 原样
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "runId", "pattern"); got != `^agent_run_` {
			t.Fatalf("pattern = %q, want %q", got, `^agent_run_`)
		}
		// 同 schema 的兄弟字段不受影响
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "query", "type"); got != "string" {
			t.Fatalf("sibling field disturbed: %q", got)
		}
	})

	t.Run("nested anyOf items pattern normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"properties":{"code":{"pattern":"x` + bs + bs + `_y"}}},{"type":"string"}]}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "code", "pattern"); got != "x_y" {
			t.Fatalf("nested pattern = %q, want %q", got, "x_y")
		}
	})

	t.Run("bare tool parameters form normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"name":"t","parameters":{"properties":{"id":{"pattern":"p` + bs + bs + `_q"}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "parameters", "properties", "id", "pattern"); got != "p_q" {
			t.Fatalf("bare parameters pattern = %q, want %q", got, "p_q")
		}
	})

	t.Run("patternProperties key normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"patternProperties":{"^a` + bs + bs + `_b` + bs + bs + `_":{"type":"string"}}}}}]}` //nolint:lll
		var obj map[string]any
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		props := tool["patternProperties"].(map[string]any)
		if _, ok := props["^a_b_"]; !ok {
			t.Fatalf("normalized key missing: %v", props)
		}
		for k := range props {
			if strings.Contains(k, string(byte(92))) {
				t.Fatalf("patternProperties key not normalized: %q", k)
			}
		}
	})

	t.Run("message content backslash untouched", func(t *testing.T) {
		bs := string(byte(92))
		raw := `{"messages":[{"role":"user","content":"path C:` + bs + bs + `_dir and regex a` + bs + bs + `_b"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"q` + bs + bs + `_r"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(raw), false, nil))
		var obj map[string]any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		msg := obj["messages"].([]any)[0].(map[string]any)
		if got := msg["content"]; got != `path C:\_dir and regex a\_b` {
			t.Fatalf("message content mutated: %q", got)
		}
	})

	t.Run("clean pattern untouched", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"^[a-z]+` + bs + bs + `d_$"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != `^[a-z]+\d_$` {
			t.Fatalf("clean pattern mutated: %q", got)
		}
	})

	t.Run("no tools is no-op", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"model":"m","messages":[{"role":"user","content":"a` + bs + bs + `_b"}]}`
		var inObj, outObj map[string]any
		if err := json.Unmarshal([]byte(body), &inObj); err != nil {
			t.Fatal(err)
		}
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &outObj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		inMsg, _ := json.Marshal(inObj["messages"])
		outMsg, _ := json.Marshal(outObj["messages"])
		if string(inMsg) != string(outMsg) {
			t.Fatalf("messages changed without tools: %s -> %s", inMsg, outMsg)
		}
		if _, has := outObj["tools"]; has {
			t.Fatal("tools appeared out of nowhere")
		}
	})

	t.Run("sanitize on and off behave the same", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"a` + bs + bs + `_b"}}}}}]}` //nolint:lll
		for _, sanitize := range []bool{false, true} {
			out := string(PrepareBodyOptWithEfforts([]byte(body), sanitize, nil))
			if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != "a_b" {
				t.Fatalf("sanitize=%v: pattern = %q, want %q", sanitize, got, "a_b")
			}
		}
	})
}

// TestClampGPTMinMaxTokens GPT 系上游要求 max_tokens ≥ 16（实测 gpt-6-sol/gpt-6-luna/
// gpt-5.6-sol：15 → 400 code=11133 model_param_invalid，16 → 200）。Claude Code 切
// 模型时的探针请求 max_tokens 极小，全号轮转同样被拒 → 客户端 503。非 GPT 模型不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"gpt below floor", `{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"gpt global prefix", `{"model":"global:gpt-5.6-sol","max_tokens":15,"messages":[]}`, float64(16)},
		{"gpt at floor", `{"model":"gpt-6-luna","max_tokens":16,"messages":[]}`, float64(16)},
		{"gpt above floor", `{"model":"gpt-6-sol","max_tokens":32000,"messages":[]}`, float64(32000)},
		{"gpt alias translated then clamped", `{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[]}`, float64(16)},
		{"gpt absent untouched", `{"model":"gpt-6-sol","messages":[]}`, nil},
		{"non-gpt untouched", `{"model":"hy4-preview-f","max_tokens":1,"messages":[]}`, float64(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, c.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, has := obj["max_tokens"]
			if c.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("max_tokens = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNormalizeStop 钉住 OpenAI `stop: string | string[]` 的出站形态。上游把
// 字段定死为 []string，字符串会 400/11101；空值与空数组删除字段，非法类型
// 原样交给上游报错，避免把调用方错误静默改写成另一种语义。
func TestNormalizeStop(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示输出不应有 stop 字段
	}{
		{"string wraps", `{"model":"m","stop":"END"}`, []any{"END"}},
		{"blank separator string survives", `{"model":"m","stop":"\n\n"}`, []any{"\n\n"}},
		{"array stays ordered", `{"model":"m","stop":["END","STOP"]}`, []any{"END", "STOP"}},
		{"array filters empties", `{"model":"m","stop":["END","","STOP"]}`, []any{"END", "STOP"}},
		// 数组里混入非字符串时不能只转发合法前缀，否则调用方的 `["END",7]`
		// 会被静默改写成 `["END"]`。整体保留原值，交给上游明确 400。
		{"array with non-strings preserved", `{"model":"m","stop":["END",7,null]}`, []any{"END", float64(7), nil}},
		{"empty string removed", `{"model":"m","stop":""}`, nil},
		{"empty array removed", `{"model":"m","stop":[]}`, nil},
		{"null removed", `{"model":"m","stop":null}`, nil},
		{"number preserved", `{"model":"m","stop":7}`, float64(7)},
		{"object preserved", `{"model":"m","stop":{"a":1}}`, map[string]any{"a": float64(1)}},
		{"absent stays absent", `{"model":"m"}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(tc.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}
			got, present := obj["stop"]
			if tc.want == nil {
				if present {
					t.Fatalf("stop should be removed, got %#v", got)
				}
				return
			}
			if !present {
				t.Fatalf("stop missing, want %#v", tc.want)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stop=%#v want %#v", got, tc.want)
			}
		})
	}
}

// TestNormalizeStopOutboundJSON 全链路断言不是单测内部 map 自嗨：序列化后的
// JSON 必须是数组，否则上游仍然会在 unmarshal 阶段报 11101。
func TestNormalizeStopOutboundJSON(t *testing.T) {
	out := PrepareBodyOpt([]byte(`{"model":"m","stop":"\n\n","messages":[]}`), true)
	var probe struct {
		Stop []string `json:"stop"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("outbound stop is not []string: %v (out=%s)", err, out)
	}
	if !reflect.DeepEqual(probe.Stop, []string{"\n\n"}) {
		t.Fatalf("outbound stop=%#v want [\\n\\n]", probe.Stop)
	}
}

// TestPrepareBodyStopStringThroughClient 端到端：客户端组件发出的 stop 字符串
// 在真正写到上游请求体之前必须已是数组。走 prepareBody（含 cache key / effort
// 管线）而不是只测 normalizeBodyObject，避免将来调换阶段顺序时回归。
func TestPrepareBodyStopStringThroughClient(t *testing.T) {
	c := New()
	out := c.prepareBody([]byte(`{"model":"glm-5.2","stop":"END","messages":[{"role":"user","content":"hi"}]}`), "cn", "uid-1", "conv-1", false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, out)
	}
	got, ok := obj["stop"].([]any)
	if !ok || len(got) != 1 || got[0] != "END" {
		t.Fatalf("prepareBody stop=%#v want [END] (body=%s)", obj["stop"], out)
	}
}

// TestPrepareBodyImageSessionCacheKey 图片请求的会话源必须覆盖 body 里的旧
// conversation_id：否则客户端在同一会话换图后，X-Conversation-ID 虽被隔离，
// prompt_cache_key 仍哈希回旧会话，前缀缓存与上游图片状态继续串用（11135）。
// 无图请求不改变既有优先级（body conversation_id 仍优先）。
func TestPrepareBodyImageSessionCacheKey(t *testing.T) {
	c := New()
	for _, body := range [][]byte{
		[]byte(`{"model":"glm-5.2","conversation_id":"client-conv","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`),
		// 客户端显式固定 prompt_cache_key 时也必须被图片指纹派生值覆盖，
		// 否则换图后仍命中旧缓存会话，11135 继续复现。
		[]byte(`{"model":"glm-5.2","conversation_id":"client-conv","prompt_cache_key":"client-fixed","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`),
	} {
		out := c.prepareBodyForceCacheConv(body, "cn", "uid-1", "client-conv:img:abcd", false, true)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, _ := obj["prompt_cache_key"].(string)
		want := buildCacheKey("uid-1", "client-conv:img:abcd")
		if got != want {
			t.Fatalf("body=%s prompt_cache_key=%q want image-session derived %q", body, got, want)
		}
	}
}

// TestChatStreamImageSessionCacheKey 走真实 ChatStreamContext 入口，确认
// PromptCacheConversationID 不仅存在于单测 helper，而是实际支配出站缓存键。
func TestChatStreamImageSessionCacheKey(t *testing.T) {
	var wire map[string]any
	var wireConvID string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("wire body: %v", err)
		}
		wireConvID = r.Header.Get("X-Conversation-ID")
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	body := []byte(`{"model":"glm-5.2","conversation_id":"client-conv","messages":[]}`)
	rc, status, respBody, err := c.ChatStream(a, body, "", ChatMeta{
		ConversationID: "client-conv", PromptCacheConversationID: "client-conv:img:abcd",
	})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d body=%s err=%v", status, respBody, err)
	}
	rc.Close()
	got, _ := wire["prompt_cache_key"].(string)
	want := buildCacheKey("u1", "client-conv:img:abcd")
	if got != want {
		t.Fatalf("wire prompt_cache_key=%q want %q", got, want)
	}
	// 11135 隔离必须同时作用于 X-Conversation-ID：上游按该头绑定图片状态，
	// 只改缓存键仍会复用旧会话。图片请求用派生会话，普通请求保持原值透传。
	if wireConvID != "client-conv:img:abcd" {
		t.Fatalf("wire X-Conversation-ID=%q want image-derived session", wireConvID)
	}
}

// TestChatStreamEnterpriseImageSessionCarriesTenant 企业号的图片隔离会话必须
// 与租户头同时成立：换图后 X-Conversation-ID / body 会话切到派生值，但
// X-Enterprise-Id / X-Tenant-Id 仍指向该企业。若只覆盖个人号，回归时很容易
// 修好 11135 却把企业聊天落回默认租户（企业图片请求表现为“图片存在但上下文
// 丢失”或跨租户 11135）。
func TestChatStreamEnterpriseImageSessionCarriesTenant(t *testing.T) {
	var wire map[string]any
	var gotConv, gotEnt, gotTenant string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("wire body: %v", err)
		}
		gotConv = r.Header.Get("X-Conversation-ID")
		gotEnt = r.Header.Get("X-Enterprise-Id")
		gotTenant = r.Header.Get("X-Tenant-Id")
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u-ent", EnterpriseID: "ent-img"}
	body := []byte(`{"model":"glm-5.2","conversation_id":"client-conv","messages":[]}`)
	rc, status, respBody, err := c.ChatStream(a, body, "", ChatMeta{
		ConversationID: "client-conv", PromptCacheConversationID: "client-conv:img:abcd",
	})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d body=%s err=%v", status, respBody, err)
	}
	rc.Close()
	if gotConv != "client-conv:img:abcd" {
		t.Fatalf("enterprise image X-Conversation-ID=%q want derived session", gotConv)
	}
	if gotEnt != "ent-img" || gotTenant != "ent-img" {
		t.Fatalf("enterprise image tenant headers=%q/%q want ent-img/ent-img", gotEnt, gotTenant)
	}
	if got, _ := wire["conversation_id"].(string); got != "client-conv:img:abcd" {
		t.Fatalf("enterprise image body conversation_id=%q want derived session", got)
	}
	if got, _ := wire["prompt_cache_key"].(string); got != buildCacheKey("u-ent", "client-conv:img:abcd") {
		t.Fatalf("enterprise image prompt_cache_key=%q", got)
	}
}

// TestPrepareBodyContentPartNormalize 覆盖出站 content 数组里的 reasoning/thinking
// part 清理：这些 part 透传会让上游报 400「unsupported content type at index 0:
// reasoning」，必须在出站前移除；可提取的推理文本按条件回填 reasoning_content。
func TestPrepareBodyContentPartNormalize(t *testing.T) {
	msgsOf := func(t *testing.T, body string) []any {
		t.Helper()
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		obj, err := decodeBody(out)
		if err != nil {
			t.Fatalf("unmarshal out=%s: %v", out, err)
		}
		msgs, ok := obj["messages"].([]any)
		if !ok {
			t.Fatalf("messages not array: %v", obj["messages"])
		}
		return msgs
	}
	firstMsg := func(t *testing.T, msgs []any) map[string]any {
		t.Helper()
		if len(msgs) != 1 {
			t.Fatalf("want 1 message, got %d: %v", len(msgs), msgs)
		}
		msg, ok := msgs[0].(map[string]any)
		if !ok {
			t.Fatalf("message not object: %T", msgs[0])
		}
		return msg
	}
	partsOf := func(t *testing.T, msg map[string]any) []any {
		t.Helper()
		parts, ok := msg["content"].([]any)
		if !ok {
			t.Fatalf("content not array: %#v", msg["content"])
		}
		return parts
	}

	t.Run("assistant deepseek reasoning summary extracted and part removed", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":[{"type":"reasoning","summary":[{"type":"summary_text","text":"hidden"}]},{"type":"text","text":"visible"}]}]}`))
		parts := partsOf(t, msg)
		if len(parts) != 1 {
			t.Fatalf("content parts=%v want only text part", parts)
		}
		part, ok := parts[0].(map[string]any)
		if !ok || part["type"] != "text" || part["text"] != "visible" {
			t.Fatalf("text part mutated: %#v", parts[0])
		}
		if got, _ := msg["reasoning_content"].(string); got != "hidden" {
			t.Fatalf("reasoning_content=%q want hidden (full msg=%v)", got, msg)
		}
	})

	t.Run("assistant non deepseek no reasoning_content key stays absent", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"glm-5.2","messages":[{"role":"assistant","content":[{"type":"reasoning","summary":[{"type":"summary_text","text":"hidden"}]},{"type":"text","text":"visible"}]}]}`))
		parts := partsOf(t, msg)
		if len(parts) != 1 {
			t.Fatalf("content parts=%v want only text part", parts)
		}
		part, ok := parts[0].(map[string]any)
		if !ok || part["type"] != "text" || part["text"] != "visible" {
			t.Fatalf("text part mutated: %#v", parts[0])
		}
		if _, exists := msg["reasoning_content"]; exists {
			t.Fatalf("reasoning_content should not be added for non-deepseek: %#v", msg)
		}
	})

	t.Run("only reasoning part leaves empty string content", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"glm-5.2","messages":[{"role":"assistant","content":[{"type":"reasoning","text":"solo"}]}]}`))
		if got, ok := msg["content"].(string); !ok || got != "" {
			t.Fatalf("content=%#v want empty string", msg["content"])
		}
	})

	t.Run("thinking block text extracted on deepseek assistant", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"th"}]}]}`))
		if got, _ := msg["reasoning_content"].(string); got != "th" {
			t.Fatalf("reasoning_content=%q want th (full msg=%v)", got, msg)
		}
		if _, ok := msg["content"].(string); !ok {
			t.Fatalf("content should be empty string after removal: %#v", msg["content"])
		}
	})

	t.Run("string content and text plus image_url unchanged", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
			want any
		}{
			{"pure string", `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`, "hello"},
			{"text plus image_url object", `{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"pic"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
				[]any{
					map[string]any{"type": "text", "text": "pic"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/y.png"}},
				}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				msg := firstMsg(t, msgsOf(t, tc.body))
				if !reflect.DeepEqual(msg["content"], tc.want) {
					t.Fatalf("content=%#v want %#v", msg["content"], tc.want)
				}
			})
		}
	})

	t.Run("legal content part types are preserved", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"glm-5.2","messages":[{"role":"user","content":[
			{"type":"text","text":"a"},
			{"type":"image_url","image_url":{"url":"https://x/y.png"}},
			{"type":"input_text","text":"b"},
			{"type":"output_text","text":"c"},
			{"type":"summary_text","text":"d"},
			"bare string"
		]}]}`))
		parts := partsOf(t, msg)
		if len(parts) != 6 {
			t.Fatalf("legal parts count=%d want 6: %#v", len(parts), parts)
		}
		wantTypes := []string{"text", "image_url", "input_text", "output_text", "summary_text", ""}
		for i, want := range wantTypes {
			part, ok := parts[i].(map[string]any)
			if want == "" {
				if ok {
					t.Fatalf("part[%d] should be string: %#v", i, parts[i])
				}
				continue
			}
			if !ok || part["type"] != want {
				t.Fatalf("part[%d]=%#v want type %q", i, parts[i], want)
			}
		}
	})

	t.Run("role Assistant case-insensitive merged", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"Assistant","content":[{"type":"reasoning","text":"thought"},{"type":"text","text":"visible"}]}]}`))
		if got, _ := msg["reasoning_content"].(string); got != "thought" {
			t.Fatalf("reasoning_content=%q want thought (full msg=%v)", got, msg)
		}
		parts := partsOf(t, msg)
		if len(parts) != 1 {
			t.Fatalf("content parts=%v want only text part", parts)
		}
	})

	t.Run("whitespace-only reasoning text ignored", func(t *testing.T) {
		msg := firstMsg(t, msgsOf(t, `{"model":"deepseek-v4.1-flash","messages":[{"role":"assistant","content":[{"type":"reasoning","text":"   "}]}]}`))
		if rc, _ := msg["reasoning_content"].(string); rc != "" {
			t.Fatalf("whitespace-only reasoning must not set non-empty reasoning_content: %#v", msg)
		}
		if got, ok := msg["content"].(string); !ok || got != "" {
			t.Fatalf("content=%#v want empty string", msg["content"])
		}
	})
}

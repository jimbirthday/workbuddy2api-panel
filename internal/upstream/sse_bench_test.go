package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// discardResponseWriter 只统计写出字节，不保留响应体；流式基准每轮都会写
// 数百帧，用 bytes.Buffer 会把内存增长算进基准。
type discardResponseWriter struct {
	header http.Header
	n      int64
}

func newDiscardResponseWriter() *discardResponseWriter {
	return &discardResponseWriter{header: make(http.Header)}
}

func (w *discardResponseWriter) Header() http.Header { return w.header }
func (w *discardResponseWriter) WriteHeader(int)     {}
func (w *discardResponseWriter) Flush()              {}
func (w *discardResponseWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

func contentFrameStream(frames int) string {
	frame := `data: {"choices":[{"delta":{"content":"hello world"},"finish_reason":null,"index":0}],"created":1,"id":"resp-1","model":"glm-5.2","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}` + "\n\n"
	return strings.Repeat(frame, frames) +
		"data: {\"id\":\"resp-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
}

func benchmarkStreamHint(b *testing.B, raw string) {
	b.Helper()
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := newDiscardResponseWriter()
		if err := StreamHint(w, strings.NewReader(raw), nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamHintContentFrames 是生产形态的逐帧透传：32 个普通 content
// 帧 + 1 个 usage 收尾帧。串行使每轮的跨帧状态隔离与顺序都得到验证。
func BenchmarkStreamHintContentFrames(b *testing.B) {
	benchmarkStreamHint(b, contentFrameStream(32))
}

// BenchmarkStreamHintSingleContentFrame 聚焦单帧开销，避免基准被文本读取和
// 收尾帧稀释。
func BenchmarkStreamHintSingleContentFrame(b *testing.B) {
	benchmarkStreamHint(b, contentFrameStream(1))
}

// BenchmarkStreamHintUpstreamContentFrames 使用上游常见键序（非规范化顺序），
// 用于观察严格直出路径的命中率：这类帧应安全回退旧路径。
func BenchmarkStreamHintUpstreamContentFrames(b *testing.B) {
	frame := `data: {"id":"resp-1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"hello world"},"finish_reason":null}]}` + "\n\n"
	benchmarkStreamHint(b, strings.Repeat(frame, 32)+"data: [DONE]\n\n")
}

// BenchmarkFastFrame 只测快速判定本身：命中路径应为零分配；回退用例用于
// 确认异常形态不会漏进直出路径。
func BenchmarkFastFrame(b *testing.B) {
	payload := []byte(`{"choices":[{"delta":{"content":"hello world"},"finish_reason":null,"index":0}],"created":1,"id":"resp-1","model":"glm-5.2","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ""
		_, kind, ok := fastFrame(payload, &id, false, nil)
		if !ok || kind != fastFrameContent || id != "resp-1" {
			b.Fatal("fast frame rejected")
		}
	}
}

// BenchmarkFastFrameFallback 量化 usage 收尾帧回退判定的成本：usage 对象需要
// normalizeUsageCacheAliases，必须继续走 Unmarshal + normalizeFrame。
func BenchmarkFastFrameFallback(b *testing.B) {
	payload := []byte(`{"id":"resp-1","usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7},"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ""
		if _, _, ok := fastFrame(payload, &id, true, nil); ok {
			b.Fatal("usage frame must fall back")
		}
	}
}

// BenchmarkFastFrameToolFallback 覆盖 tool_calls 帧：即使结构合法也必须回退，
// 因为 name 收敛与 id 续传由旧路径维护。
func BenchmarkFastFrameToolFallback(b *testing.B) {
	payload := []byte(`{"id":"resp-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := ""
		if _, _, ok := fastFrame(payload, &id, true, nil); ok {
			b.Fatal("tool_calls frame must fall back")
		}
	}
}

// BenchmarkNormalizeContentFrame 记录旧路径（Unmarshal + normalizeFrame +
// Marshal）的单帧成本，便于与快速路径直接对比。
func BenchmarkNormalizeContentFrame(b *testing.B) {
	const payload = `{"choices":[{"delta":{"content":"hello world"},"finish_reason":null,"index":0}],"created":1,"id":"resp-1","model":"glm-5.2","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}`
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			b.Fatal(err)
		}
		if _, err := json.Marshal(normalizeFrame(obj)); err != nil {
			b.Fatal(err)
		}
	}
}

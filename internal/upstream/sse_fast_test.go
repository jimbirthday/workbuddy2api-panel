package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFastFrameCanonicalFieldsEmitted 钉住快速路径的两个前提：
//  1. 上游常见键序（非 json.Marshal 字典序）必须回退，不能直出半成品；
//  2. 规范键序帧直出后必须包含 normalizeFrame 会补齐的顶层字段，且与旧路径
//     逐字节一致（否则客户端会拿到缺 created/object/usage 的帧）。
func TestFastFrameCanonicalFieldsEmitted(t *testing.T) {
	old := fastFrameDisabled
	fastFrameDisabled = false
	defer func() { fastFrameDisabled = old }()

	upstreamOrder := []byte(`{"id":"resp-1","object":"chat.completion.chunk","created":123,"model":"glm-5.3","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)
	var id string
	if _, _, ok := fastFrame(upstreamOrder, &id, false, nil); ok {
		t.Fatal("upstream key order must fall back to legacy normalize path")
	}

	canonical := []byte(`{"choices":[{"delta":{"content":"hello"},"finish_reason":null,"index":0}],"created":1,"id":"x1","model":"m","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}`)
	raw, kind, ok := fastFrame(canonical, &id, false, nil)
	if !ok || kind != fastFrameContent {
		t.Fatalf("canonical content frame must fast-path: ok=%v kind=%d", ok, kind)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("fast frame emitted invalid JSON: %v raw=%s", err, raw)
	}
	for _, k := range []string{"choices", "created", "id", "model", "object", "usage"} {
		if _, exists := obj[k]; !exists {
			t.Errorf("fast frame missing top-level %q: %s", k, raw)
		}
	}
}

// TestFastFrameCanonicalMatchesLegacyStream 规范键序的 content / role+content /
// finish 流必须与关闭快速路径时逐字节一致。
func TestFastFrameCanonicalMatchesLegacyStream(t *testing.T) {
	const (
		seed    = `data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}],"created":1,"id":"x1","model":"m","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}` + "\n\n"
		content = `data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null,"index":0}],"created":1,"id":"x1","model":"m","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}` + "\n\n"
		finish  = `data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1,"id":"x1","model":"m","object":"chat.completion.chunk","service_tier":"","system_fingerprint":"","usage":null}` + "\n\n"
	)
	raw := seed + content + finish + "data: [DONE]\n\n"
	render := func(fast bool) string {
		old := fastFrameDisabled
		fastFrameDisabled = !fast
		defer func() { fastFrameDisabled = old }()
		rec := httptest.NewRecorder()
		if err := StreamHint(rec, strings.NewReader(raw), nil); err != nil {
			t.Fatalf("stream err (fast=%v): %v", fast, err)
		}
		return rec.Body.String()
	}
	if got, want := render(true), render(false); got != want {
		t.Fatalf("fast path output diverged\nfast:   %q\nlegacy: %q", got, want)
	}
}

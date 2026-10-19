package session

import (
	"strings"
	"testing"
)

func TestImageFingerprint(t *testing.T) {
	img1 := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	img2 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]}]}`
	strForm := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,AAAA"}]}]}`
	objForm := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	multiA := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"a"},{"type":"image_url","image_url":"b"}]}]}`
	multiB := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"b"},{"type":"image_url","image_url":"a"}]}]}`

	a := ImageFingerprint([]byte(img1))
	if a == "" || a != ImageFingerprint([]byte(img1)) {
		t.Fatalf("same image must produce stable non-empty fingerprint: %q", a)
	}
	if a == ImageFingerprint([]byte(img2)) {
		t.Fatal("different image produced same fingerprint")
	}
	if ImageFingerprint([]byte(strForm)) != ImageFingerprint([]byte(objForm)) {
		t.Fatal("string and object image_url forms must normalize to same fingerprint")
	}
	if ImageFingerprint([]byte(multiA)) == ImageFingerprint([]byte(multiB)) {
		t.Fatal("image order must participate in fingerprint")
	}
	if got := ImageFingerprint([]byte(`{"messages":[{"role":"user","content":"只有文字"}]} `)); got != "" {
		t.Fatalf("text-only request fingerprint=%q want empty", got)
	}
	if got := ImageFingerprint([]byte(`{bad`)); got != "" {
		t.Fatalf("bad JSON fingerprint=%q want empty", got)
	}
}

func TestWithImageFingerprint(t *testing.T) {
	if got := WithImageFingerprint("conv-1", "u1", ""); got != "conv-1" {
		t.Fatalf("empty fingerprint changed conversation: %q", got)
	}
	if got := WithImageFingerprint("", "", "abcd"); got != "img:abcd" {
		t.Fatalf("image-only conversation without scope=%q want img:abcd", got)
	}
	if got := WithImageFingerprint("", "u1", ""); got != "" {
		t.Fatalf("no image/conversation changed to: %q", got)
	}
	a := WithImageFingerprint("conv-1", "u1", "abcd")
	b := WithImageFingerprint("conv-1", "u1", "ef01")
	if a == b || !strings.HasPrefix(a, "conv-1:img:") || !strings.HasSuffix(a, ":abcd") {
		t.Fatalf("image-derived ids a=%q b=%q", a, b)
	}
	if WithImageFingerprint("conv-1", "u1", "abcd") == WithImageFingerprint("conv-1", "u2", "abcd") {
		t.Fatal("same image on different accounts must not share X-Conversation-ID")
	}
	if WithImageFingerprint("", "u1", "abcd") == WithImageFingerprint("", "u2", "abcd") {
		t.Fatal("image-only requests must be account isolated")
	}
}

// 多轮图片对话：历史轮里的旧图不应让指纹随轮次漂移；只有最后一轮 user 的
// 图片集合决定本次上游会话。同一最后一轮重试稳定，换最新图才变化。
func TestImageFingerprintOnlyLastUserTurn(t *testing.T) {
	round2 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"img-a"}]},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"image_url","image_url":"img-b"}]}]}`
	round3 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"img-a"}]},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"image_url","image_url":"img-c"}]}]}`
	f2 := ImageFingerprint([]byte(round2))
	if f2 == "" || f2 != ImageFingerprint([]byte(round2)) {
		t.Fatalf("same latest turn must be stable, got %q", f2)
	}
	// round1 与 round2 的最后一轮图片不同，合法地产生不同会话；核心是追加
	// 历史图片不会把「同一张最新图」算成新会话。
	sameLatest := `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"image_url","image_url":"img-b"}]}]}`
	if ImageFingerprint([]byte(sameLatest)) != f2 {
		t.Fatal("adding history text must not change latest-image fingerprint")
	}
	if f2 == ImageFingerprint([]byte(round3)) {
		t.Fatal("replacing latest image must change fingerprint")
	}
	// 最后一轮是纯文本时不应沿用更早的图片，避免把新问题绑到旧图会话。
	if got := ImageFingerprint([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"img-a"}]},{"role":"user","content":"follow up"}]}`)); got != "" {
		t.Fatalf("text-only latest user turn fingerprint=%q want empty", got)
	}
}

// 多轮追问：首轮带图后，下一轮只发文字时必须沿用最近图片会话。若退回原
// conversationId，上游会把图片上下文视为丢失，再次返回 11135。
func TestResolveImageSessionCarriesAcrossTextFollowUp(t *testing.T) {
	const conv = "conv-follow-up"
	forgetImageSession(conv)
	first := ResolveImageSession(conv, "u1", "img-a")
	if !strings.HasPrefix(first, "conv-follow-up:img:") || !strings.HasSuffix(first, ":img-a") {
		t.Fatalf("first image session=%q", first)
	}
	if got := ResolveImageSession(conv, "u1", ""); got != first {
		t.Fatalf("text follow-up session=%q want carry %q", got, first)
	}
	// 换图后进入新会话；再之后的纯文本追问继续沿用新图会话。
	second := ResolveImageSession(conv, "u1", "img-b")
	if second == first {
		t.Fatalf("new image should switch session, both=%q", second)
	}
	if got := ResolveImageSession(conv, "u1", ""); got != second {
		t.Fatalf("follow-up after image change=%q want %q", got, second)
	}
	if got := ResolveImageSession("conv-without-images", "u1", ""); got != "" {
		t.Fatalf("never-image conversation changed: %q", got)
	}
	// 换号重试：同一原会话、同一图片，但账号不同 -> 必须得到不同的会话头。
	if respawn := ResolveImageSession(conv, "u2", "img-b"); respawn == second {
		t.Fatal("account rotation reused another account's image session")
	}
}

// 同一原会话在两个账号间轮转时，图片谱系也必须按账号隔离：A 带图后 B 的
// 纯文本追问不能继承 A 的图片指纹。否则换号重试会把 A 的图片上下文发给 B。
func TestResolveImageSessionLineageIsAccountScoped(t *testing.T) {
	const conv = "conv-shared-lineage"
	forgetImageSession(conv)
	// 清掉双键形态的测试残留（forgetImageSession 只删旧单键）。
	for _, scope := range []string{"u1", "u2"} {
		imageSessions.mu.Lock()
		delete(imageSessions.entries, scope+"\x00"+conv)
		imageSessions.mu.Unlock()
	}
	first := ResolveImageSession(conv, "u1", "img-a")
	if first == "" {
		t.Fatal("u1 image session is empty")
	}
	if got := ResolveImageSession(conv, "u2", ""); got != "" {
		t.Fatalf("u2 text follow-up inherited u1 lineage: %q", got)
	}
	// 各自独立登记后仍分别稳定。
	u2 := ResolveImageSession(conv, "u2", "img-b")
	if u2 == "" || u2 == first {
		t.Fatalf("u2 image session=%q first=%q", u2, first)
	}
	if got := ResolveImageSession(conv, "u1", ""); got != first {
		t.Fatalf("u1 follow-up=%q want %q", got, first)
	}
}

// TestImageSummary 日志摘要只暴露 MIME/字节数/短哈希，不泄露 base64 原文；
// 坏 base64、远程 URL 与同一图片的稳定哈希都要可区分。
func TestImageSummary(t *testing.T) {
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}]}`)
	got := ImageSummary(body)
	if !strings.Contains(got, "images=1") || !strings.Contains(got, "image/png") || !strings.Contains(got, "B/") {
		t.Fatalf("summary=%q", got)
	}
	if strings.Contains(got, png) || strings.Contains(got, "base64,") {
		t.Fatalf("summary leaked payload: %q", got)
	}
	if again := ImageSummary(body); again != got {
		t.Fatalf("summary unstable: %q vs %q", got, again)
	}
	bad := ImageSummary([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%%%"}}]}]}`))
	if !strings.Contains(bad, "bad-base64") {
		t.Fatalf("bad base64 summary=%q", bad)
	}
	remote := ImageSummary([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png?token=secret"}}]}]}`))
	if !strings.Contains(remote, "url/") || strings.Contains(remote, "secret") {
		t.Fatalf("remote summary=%q", remote)
	}
}

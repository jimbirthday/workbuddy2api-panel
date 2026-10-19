package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
)

// imageLineage 记录每个「账号 + 原会话」最近一次带图轮的指纹。图片上下文
// 会跨轮延续：用户先发图，下一轮只发文字追问时，上游仍要求沿用上一轮的图片
// 会话；如果这时把会话切回原 ID，上游会认为图片会话丢失并再次返回 11135。
// 因此空指纹不能直接退回原会话，而要回落到该会话最近一次图片指纹。
//
// 键必须包含账号维度：同一个客户端会话在限流换号后会落到不同账号，图片谱系
// 若只按原会话键存，B 账号的纯文本追问会继承 A 账号的图片指纹，把 A 的图片
// 上下文发进 B 的上游会话。账号段由 handler 传入实际出站 UID。
//
// 只保留进程内有界条目：会话键最长 4k，最多 8192 条，避免任意客户端输入把
// 内存撑大；淘汰按最近使用顺序（命中会把条目移到队尾），高并发下近似 LRU。
type imageLineage struct {
	mu      sync.Mutex
	entries map[string]imageLineageEntry
	order   []string
}
type imageLineageEntry struct {
	fingerprint string
}

const (
	imageLineageLimit = 8192
	// imageLineageMinFingerprint 防止把畸形/空指纹写进会话状态。
	imageLineageMinConversation = 1
)

var imageSessions = &imageLineage{entries: make(map[string]imageLineageEntry)}

// ResolveImageSession 用「当前图片指纹」和该会话最近一次图片指纹，得到本轮
// 出站会话源：
//   - 当前请求带图：登记/更新该会话的图片谱系，并使用当前指纹；
//   - 当前请求无图但历史有图：沿用最近指纹，纯文本追问仍留在同一图片会话；
//   - 从未带过图：返回空串，调用方保持原会话（无图行为零漂移）。
func ResolveImageSession(conversationID, scope, fingerprint string) string {
	conversationID = strings.TrimSpace(conversationID)
	scope = strings.TrimSpace(scope)
	fingerprint = strings.TrimSpace(fingerprint)
	if conversationID == "" {
		// 无会话键时没有可关联的历史；图片-only 请求仍用指纹派生稳定键。
		if fingerprint == "" {
			return ""
		}
		return WithImageFingerprint("", scope, fingerprint)
	}
	if len(conversationID) < imageLineageMinConversation {
		return WithImageFingerprint(conversationID, scope, fingerprint)
	}
	// 账号 + 会话双键：换号后不能读到上一个账号的图片谱系。scope 为空时
	// 保持旧行为（测试/无账号上下文的直接调用），生产 handler 始终传 UID。
	lineageKey := conversationID
	if scope != "" {
		lineageKey = scope + "\x00" + conversationID
	}
	imageSessions.mu.Lock()
	defer imageSessions.mu.Unlock()
	if fingerprint != "" {
		imageSessions.putLocked(lineageKey, fingerprint)
		return WithImageFingerprint(conversationID, scope, fingerprint)
	}
	if e, ok := imageSessions.entries[lineageKey]; ok && e.fingerprint != "" {
		return WithImageFingerprint(conversationID, scope, e.fingerprint)
	}
	return ""
}

func (l *imageLineage) putLocked(conversationID, fingerprint string) {
	if old, ok := l.entries[conversationID]; ok {
		old.fingerprint = fingerprint
		l.entries[conversationID] = old
		// 命中已存在的会话：移到 order 末尾，否则淘汰实际是 FIFO，长期活跃的
		// 图片会话会被最早写入的冷会话挤掉。
		for i, key := range l.order {
			if key == conversationID {
				copy(l.order[i:], l.order[i+1:])
				l.order[len(l.order)-1] = conversationID
				break
			}
		}
		return
	}
	l.entries[conversationID] = imageLineageEntry{fingerprint: fingerprint}
	l.order = append(l.order, conversationID)
	if len(l.order) <= imageLineageLimit {
		return
	}
	drop := len(l.order) - imageLineageLimit
	for _, key := range l.order[:drop] {
		delete(l.entries, key)
	}
	l.order = append([]string(nil), l.order[drop:]...)
}

// forgetImageSession 供测试复位；生产不需要主动清空（条目有上限且会自然淘汰）。
func forgetImageSession(conversationID string) {
	imageSessions.mu.Lock()
	defer imageSessions.mu.Unlock()
	delete(imageSessions.entries, strings.TrimSpace(conversationID))
}

// ImageFingerprint 返回请求体里「图片集合」的稳定短指纹；无图片返回空串。
//
// 上游 11135（Please start a new conversation, replace the image, and try
// again.）表示图片与会话上下文绑定：同一 conversationId 里换一张图，上游会拒绝
// 整段请求。网关不解析/转码图片，只在会话维度做隔离：
//   - 同一图片重试 → 同一指纹 → 同一个上游会话，客户端重试仍能命中前缀缓存；
//   - 替换/追加图片 → 指纹变化 → 派生新会话，避免复用旧会话的图片状态。
//
// 指纹只看 image_url part 的规范化内容，不读文本。data: URL 与普通 URL 原样参与
// 哈希（哈希不可逆，不落原文）；同一请求里多张图的顺序也参与，天然区分
// 「换一张」与「多一张」。part 本身非法（非对象 / 缺 url）时不参与——校验与
// 拒绝交给正常请求路径/上游，指纹不能把畸形请求重新变成「有图」。
func ImageFingerprint(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var root struct {
		Messages []struct {
			Role    json.RawMessage `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	// 只对最后一轮 user 消息取指纹：历史轮里的图片已经属于既有会话，继续把
	// 它们计入会让每追加一张图就换一次会话，破坏多轮图片对话。最后一轮决定
	// 「这次要上游识别哪组图」：同轮重试稳定、换图/改图顺序变化。
	var lastUserParts []map[string]any
	for _, m := range root.Messages {
		var role string
		_ = json.Unmarshal(m.Role, &role)
		if !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		raw := strings.TrimSpace(string(m.Content))
		if len(raw) == 0 || raw[0] != '[' {
			// 最后一轮 user 是纯字符串时，图片集合为空；不能继续沿用前一轮。
			lastUserParts = nil
			continue
		}
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			lastUserParts = nil
			continue
		}
		lastUserParts = parts
	}
	var urls []string
	for _, part := range lastUserParts {
		if !strings.EqualFold(imageString(part["type"]), "image_url") {
			continue
		}
		url := imageURLFromPart(part)
		if url == "" {
			continue
		}
		urls = append(urls, url)
	}
	if len(urls) == 0 {
		return ""
	}
	h := sha256.New()
	for _, url := range urls {
		// 长度前缀防止 ["ab","c"] 与 ["a","bc"] 拼接后碰撞。
		var size [8]byte
		n := uint64(len(url))
		for i := 0; i < 8; i++ {
			size[i] = byte(n >> (8 * i))
		}
		h.Write(size[:])
		h.Write([]byte(url))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// imageURLFromPart 取 image_url part 的 url。兼容 OpenAI 对象形态与字符串形态
// （出站归一化之前两种都可能出现）。
func imageURLFromPart(part map[string]any) string {
	switch v := part["image_url"].(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		return strings.TrimSpace(imageString(v["url"]))
	}
	return ""
}

// imageString 读取 part 的字符串字段；非字符串视为空，避免把畸形 part
// 误判成有效图片而改变会话。
func imageString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// WithImageFingerprint 在原会话 ID 后追加账号隔离段与图片指纹，得到同一账号、
// 同一图片稳定，换图/换号变化的上游会话 ID。空 conversationID 或空指纹时原样
// 返回（无图请求不改变任何会话）。
//
// 不直接改写 body 里的 conversation_id（那是客户端语义，客户端再次发同一 ID 时
// 网关需要重新判断图片是否变化）；仅用于出站头族 session ID 与 prompt_cache_key
// 的派生源。
//
// scope 必须是账号维度（handler 传入账号 UID）：上游按 X-Conversation-ID 绑定
// 图片状态，只靠 prompt_cache_key 的 uid8 隔离不够——同一会话换号重试时，
// 不带账号段的会话头会把 A 账号的图片上下文发给 B 账号。
func WithImageFingerprint(conversationID, scope, fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return conversationID
	}
	scope = strings.TrimSpace(scope)
	scopeSeg := ""
	if scope != "" {
		sum := sha256.Sum256([]byte(scope))
		scopeSeg = ":" + hex.EncodeToString(sum[:6])
	}
	if strings.TrimSpace(conversationID) == "" {
		// 图片-only 请求没有文本会话键可复用；用指纹本身仍保持「同图重试稳定、
		// 换图变化」的上游会话语义。账号段必须一并带上，避免跨账号同名图片复用。
		if scopeSeg == "" {
			return "img:" + fingerprint
		}
		return "img" + scopeSeg + ":" + fingerprint
	}
	return conversationID + ":img" + scopeSeg + ":" + fingerprint
}

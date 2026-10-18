package upstream

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// fastFrameKind 是快速判定结果：ok=false 回退旧路径。
type fastFrameKind uint8

const (
	fastFrameNoop fastFrameKind = iota
	fastFrameContent
	fastFrameFinish
)

// fastFrame 按 normalizeFrame 的白名单与 json.Marshal(map) 的固定键序重建
// 普通帧，避免 Unmarshal + map 重建 + Marshal 的开销。
//
// 只接受语义完全可控的帧：choices 非空，每个 choice 只有 index/delta/
// finish_reason，delta 只有非空 role/content；顶层只含 id/object/created/
// model/choices/usage 及两个可选元信息字段；usage 只允许缺失或 null。
// tool_calls/function_call/refusal/error/code/未知字段、需要转义或 HTML
// 转义的字符串、空 content、finish_reason 空串、缺必需字段、重复键、非法
// JSON 一律回退旧路径。字段值直接复用原 JSON 片段，输出键序固定，因此
// 出站字节与旧路径逐字节一致。
func fastFrame(payload []byte, firstID *string, gotAnswer bool, dst []byte) ([]byte, fastFrameKind, bool) {
	if len(payload) == 0 || bytes.IndexByte(payload, '\n') >= 0 || bytes.IndexByte(payload, '\r') >= 0 {
		return nil, fastFrameNoop, false
	}
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return nil, fastFrameNoop, false
	}

	p := payload
	i := fastSkipSpace(p, 0)
	if i >= len(p) || p[i] != '{' {
		return nil, fastFrameNoop, false
	}
	i++

	var (
		seenID, seenObject, seenCreated, seenModel bool
		seenTier, seenFingerprint, seenUsage       bool
		seenChoices                                bool
		seenOrder                                  [8]bool
		lastRank                                   = -1
		idRaw, objectRaw, createdRaw               []byte
		modelRaw, tierRaw, fingerprintRaw          []byte
		out                                        = dst[:0]
		kind                                       fastFrameKind
	)

	for {
		i = fastSkipSpace(p, i)
		if i >= len(p) {
			return nil, fastFrameNoop, false
		}
		if p[i] == '}' {
			i++
			break
		}
		if p[i] != '"' {
			return nil, fastFrameNoop, false
		}
		keyStart := i + 1
		keyEnd := fastStringEnd(p, i)
		if keyEnd < 0 {
			return nil, fastFrameNoop, false
		}
		key := p[keyStart : keyEnd-1]
		if len(key) == 0 || bytes.IndexByte(key, '\\') >= 0 {
			return nil, fastFrameNoop, false
		}
		rank := fastRootKeyRank(key)
		if rank < 0 || seenOrder[rank] || rank < lastRank {
			return nil, fastFrameNoop, false
		}
		seenOrder[rank] = true
		lastRank = rank
		i = fastSkipSpace(p, keyEnd)
		if i >= len(p) || p[i] != ':' {
			return nil, fastFrameNoop, false
		}
		i = fastSkipSpace(p, i+1)
		if i >= len(p) {
			return nil, fastFrameNoop, false
		}
		valueStart := i
		valueEnd := fastValueEnd(p, i)
		if valueEnd < 0 {
			return nil, fastFrameNoop, false
		}

		switch {
		case bytes.Equal(key, []byte("id")):
			if seenID || p[valueStart] != '"' {
				return nil, fastFrameNoop, false
			}
			if !fastCanonicalString(p[valueStart:valueEnd]) {
				return nil, fastFrameNoop, false
			}
			seenID = true
			idRaw = p[valueStart:valueEnd]
		case bytes.Equal(key, []byte("object")):
			if seenObject {
				return nil, fastFrameNoop, false
			}
			seenObject = true
			if !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				if p[valueStart] != '"' || !fastCanonicalString(p[valueStart:valueEnd]) {
					return nil, fastFrameNoop, false
				}
				objectRaw = p[valueStart:valueEnd]
			}
		case bytes.Equal(key, []byte("created")):
			if seenCreated {
				return nil, fastFrameNoop, false
			}
			seenCreated = true
			if !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				if !fastCanonicalInteger(p[valueStart:valueEnd]) {
					return nil, fastFrameNoop, false
				}
				createdRaw = p[valueStart:valueEnd]
			}
		case bytes.Equal(key, []byte("model")):
			if seenModel {
				return nil, fastFrameNoop, false
			}
			seenModel = true
			if !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				if p[valueStart] != '"' || !fastCanonicalString(p[valueStart:valueEnd]) {
					return nil, fastFrameNoop, false
				}
				modelRaw = p[valueStart:valueEnd]
			}
		case bytes.Equal(key, []byte("service_tier")):
			if seenTier {
				return nil, fastFrameNoop, false
			}
			seenTier = true
			if !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				if p[valueStart] != '"' || !fastCanonicalString(p[valueStart:valueEnd]) {
					return nil, fastFrameNoop, false
				}
				tierRaw = p[valueStart:valueEnd]
			}
		case bytes.Equal(key, []byte("system_fingerprint")):
			if seenFingerprint {
				return nil, fastFrameNoop, false
			}
			seenFingerprint = true
			if !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				if p[valueStart] != '"' || !fastCanonicalString(p[valueStart:valueEnd]) {
					return nil, fastFrameNoop, false
				}
				fingerprintRaw = p[valueStart:valueEnd]
			}
		case bytes.Equal(key, []byte("usage")):
			if seenUsage || !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				return nil, fastFrameNoop, false
			}
			seenUsage = true
		case bytes.Equal(key, []byte("choices")):
			if seenChoices || p[valueStart] != '[' {
				return nil, fastFrameNoop, false
			}
			seenChoices = true
			var ok bool
			// 根对象尚未写过任何键（choices 是字典序第一项），必须先开 `{`。
			// 后续键由下面固定顺序的 append 继续，最终统一补 `}`。
			out = append(out, `{"choices":`...)
			out, kind, ok = fastAppendChoices(out, p, valueStart)
			if !ok {
				return nil, fastFrameNoop, false
			}
			valueEnd = fastChoicesEnd(p, valueStart)
		default:
			return nil, fastFrameNoop, false
		}

		i = fastSkipSpace(p, valueEnd)
		if i >= len(p) {
			return nil, fastFrameNoop, false
		}
		if p[i] == ',' {
			i++
			continue
		}
		if p[i] == '}' {
			i++
			break
		}
		return nil, fastFrameNoop, false
	}
	if fastSkipSpace(p, i) != len(p) || !seenChoices {
		return nil, fastFrameNoop, false
	}

	// 键序已确认与 json.Marshal(map) 的字典序一致，下面只需补齐 map 中
	// 规范存在但输入缺失/为 null 的字段，值原样复用输入片段。
	if !seenOrder[3] {
		return nil, fastFrameNoop, false
	}
	// id：缺 id 用已缓存 id；全流无 id 才写哨兵；空 id 只在无缓存时保留。
	idOut := idRaw
	switch {
	case len(idRaw) > 2:
		if *firstID == "" {
			*firstID = string(idRaw[1 : len(idRaw)-1])
		}
	case len(idRaw) == 2:
		if *firstID != "" {
			return nil, fastFrameNoop, false
		}
	case *firstID != "":
		if bytes.IndexByte([]byte(*firstID), '\\') >= 0 || bytes.IndexByte([]byte(*firstID), '<') >= 0 ||
			bytes.IndexByte([]byte(*firstID), '>') >= 0 || bytes.IndexByte([]byte(*firstID), '&') >= 0 {
			return nil, fastFrameNoop, false
		}
		idOut = appendQuote(idOut[:0], []byte(*firstID))
	default:
		idOut = []byte(`"chatcmpl-wb2api"`)
	}
	out = append(out, `,"created":`...)
	if len(createdRaw) > 0 {
		out = append(out, createdRaw...)
	} else {
		return nil, fastFrameNoop, false
	}
	out = append(out, `,"id":`...)
	out = append(out, idOut...)
	if len(modelRaw) > 0 {
		out = append(out, `,"model":`...)
		out = append(out, modelRaw...)
	}
	out = append(out, `,"object":`...)
	if len(objectRaw) > 0 {
		out = append(out, objectRaw...)
	} else {
		out = append(out, `"chat.completion.chunk"`...)
	}
	if len(tierRaw) > 0 {
		out = append(out, `,"service_tier":`...)
		out = append(out, tierRaw...)
	}
	if len(fingerprintRaw) > 0 {
		out = append(out, `,"system_fingerprint":`...)
		out = append(out, fingerprintRaw...)
	}
	out = append(out, `,"usage":null}`...)
	if kind == fastFrameFinish && !gotAnswer {
		kind = fastFrameNoop
	}
	return out, kind, true
}

func fastRootKeyRank(key []byte) int {
	switch {
	case bytes.Equal(key, []byte("choices")):
		return 0
	case bytes.Equal(key, []byte("created")):
		return 1
	case bytes.Equal(key, []byte("id")):
		return 2
	case bytes.Equal(key, []byte("model")):
		return 3
	case bytes.Equal(key, []byte("object")):
		return 4
	case bytes.Equal(key, []byte("service_tier")):
		return 5
	case bytes.Equal(key, []byte("system_fingerprint")):
		return 6
	case bytes.Equal(key, []byte("usage")):
		return 7
	default:
		return -1
	}
}

func appendQuote(dst, body []byte) []byte {
	dst = append(dst, '"')
	dst = append(dst, body...)
	return append(dst, '"')
}

func fastSkipSpace(p []byte, i int) int {
	for i < len(p) && (p[i] == ' ' || p[i] == '\t') {
		i++
	}
	return i
}

// fastStringEnd 返回 JSON 字符串右引号之后的位置（p[i] 必须是 '"'）。
func fastStringEnd(p []byte, i int) int {
	for j := i + 1; j < len(p); j++ {
		switch p[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// fastValueEnd 返回一个 JSON 值结束后的位置。调用前已由 json.Valid 确认
// 整帧合法，这里只负责结构定位。
func fastValueEnd(p []byte, i int) int {
	if i >= len(p) {
		return -1
	}
	switch p[i] {
	case '"':
		return fastStringEnd(p, i)
	case '{', '[':
		depth := 0
		inString := false
		for j := i; j < len(p); j++ {
			if inString {
				if p[j] == '\\' {
					j++
				} else if p[j] == '"' {
					inString = false
				}
				continue
			}
			switch p[j] {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	default:
		j := i
		for j < len(p) && p[j] != ',' && p[j] != '}' && p[j] != ']' && p[j] != ' ' && p[j] != '\t' {
			j++
		}
		if j == i {
			return -1
		}
		return j
	}
}

// fastCanonicalString 报告带引号的 JSON 字符串能否被 json.Marshal 原样重编码。
// 转义序列、HTML 敏感字符与 U+2028/U+2029 会被重编码成不同字节，统一回退。
func fastCanonicalString(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	body := raw[1 : len(raw)-1]
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\' || c == '<' || c == '>' || c == '&':
			return false
		case c == 0xe2 && i+2 < len(body) && body[i+1] == 0x80 && (body[i+2] == 0xa8 || body[i+2] == 0xa9):
			return false
		}
	}
	return true
}

// fastCanonicalInteger 只接受可被 float64 精确表示且重编码字节不变的非负
// 整数。指数、小数、前导零和超长数字都回退旧路径。
func fastCanonicalInteger(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 15 {
		return false
	}
	if len(raw) > 1 && raw[0] == '0' {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// fastAppendChoices 按 normalizeFrame 的 choice/delta 白名单重建 choices 数组，
// 同时返回整帧语义：content 或 role-only。
func fastAppendChoices(out, p []byte, i int) ([]byte, fastFrameKind, bool) {
	i++ // [
	out = append(out, '[')
	seen := false
	kind := fastFrameNoop
	for {
		i = fastSkipSpace(p, i)
		if i >= len(p) {
			return nil, fastFrameNoop, false
		}
		if p[i] == ']' {
			out = append(out, ']')
			return out, kind, seen
		}
		if p[i] != '{' {
			return nil, fastFrameNoop, false
		}
		if seen {
			out = append(out, ',')
		}
		var ok bool
		out, i, kind, ok = fastAppendChoice(out, p, i, kind)
		if !ok {
			return nil, fastFrameNoop, false
		}
		seen = true

		i = fastSkipSpace(p, i)
		if i >= len(p) {
			return nil, fastFrameNoop, false
		}
		if p[i] == ',' {
			i++
			continue
		}
		if p[i] == ']' {
			out = append(out, ']')
			return out, kind, true
		}
		return nil, fastFrameNoop, false
	}
}

// fastAppendChoice 重建单个 choice，返回右花括号之后的位置。
func fastAppendChoice(out, p []byte, i int, frameKind fastFrameKind) ([]byte, int, fastFrameKind, bool) {
	i++ // {
	var indexRaw, deltaContentRaw []byte
	seenIndex, seenDelta, seenFinish := false, false, false
	kind := fastFrameNoop
	deltaRole := false
	for {
		i = fastSkipSpace(p, i)
		if i >= len(p) {
			return nil, 0, fastFrameNoop, false
		}
		if p[i] == '}' {
			i++
			if !seenIndex || !seenDelta || !seenFinish {
				return nil, 0, fastFrameNoop, false
			}
			// 每个 choice 语义必须一致，混合 content/role 的帧回退旧路径。
			if frameKind != fastFrameNoop && kind != frameKind {
				return nil, 0, fastFrameNoop, false
			}
			if kind == fastFrameNoop {
				kind = frameKind
			}
			out = append(out, `{"delta":`...)
			out = append(out, `{"content":`...)
			if len(deltaContentRaw) > 0 {
				out = append(out, deltaContentRaw...)
			} else {
				if !deltaRole {
					return nil, 0, fastFrameNoop, false
				}
				out = append(out[:len(out)-len(`{"content":`)], `{"role":"assistant"`...)
				_ = deltaRole
			}
			out = append(out, '}')
			out = append(out, `,"finish_reason":null,"index":`...)
			out = append(out, indexRaw...)
			out = append(out, '}')
			return out, i, kind, true
		}
		if p[i] != '"' {
			return nil, 0, fastFrameNoop, false
		}
		keyStart := i + 1
		keyEnd := fastStringEnd(p, i)
		if keyEnd < 0 {
			return nil, 0, fastFrameNoop, false
		}
		key := p[keyStart : keyEnd-1]
		i = fastSkipSpace(p, keyEnd)
		if i >= len(p) || p[i] != ':' {
			return nil, 0, fastFrameNoop, false
		}
		i = fastSkipSpace(p, i+1)
		if i >= len(p) {
			return nil, 0, fastFrameNoop, false
		}
		valueStart := i
		valueEnd := fastValueEnd(p, i)
		if valueEnd < 0 {
			return nil, 0, fastFrameNoop, false
		}
		switch {
		case bytes.Equal(key, []byte("index")):
			if seenIndex || !fastCanonicalInteger(p[valueStart:valueEnd]) {
				return nil, 0, fastFrameNoop, false
			}
			indexRaw = p[valueStart:valueEnd]
			seenIndex = true
		case bytes.Equal(key, []byte("delta")):
			if seenDelta || p[valueStart] != '{' {
				return nil, 0, fastFrameNoop, false
			}
			end, content, role, ok := fastDeltaParts(p, valueStart)
			if !ok {
				return nil, 0, fastFrameNoop, false
			}
			deltaContentRaw = content
			deltaRole = role
			seenDelta = true
			i = end
			if role && len(content) == 0 {
				kind = fastFrameNoop
			} else {
				kind = fastFrameContent
			}
		case bytes.Equal(key, []byte("finish_reason")):
			if seenFinish || !bytes.Equal(p[valueStart:valueEnd], []byte("null")) {
				return nil, 0, fastFrameNoop, false
			}
			seenFinish = true
		default:
			return nil, 0, fastFrameNoop, false
		}

		i = fastSkipSpace(p, valueEnd)
		if i >= len(p) {
			return nil, 0, fastFrameNoop, false
		}
		if p[i] == ',' {
			i++
			continue
		}
		if p[i] == '}' {
			i++
			if !seenIndex || !seenDelta || !seenFinish {
				return nil, 0, fastFrameNoop, false
			}
			if frameKind != fastFrameNoop && kind != frameKind {
				return nil, 0, fastFrameNoop, false
			}
			if kind == fastFrameNoop {
				kind = frameKind
			}
			if len(deltaContentRaw) > 0 {
				out = append(out, `{"delta":{"content":`...)
				out = append(out, deltaContentRaw...)
				out = append(out, '}')
			} else {
				out = append(out, `{"delta":{"role":"assistant"}`...)
			}
			out = append(out, `,"finish_reason":null,"index":`...)
			out = append(out, indexRaw...)
			out = append(out, '}')
			return out, i, kind, true
		}
		return nil, 0, fastFrameNoop, false
	}
}

// fastDeltaParts 只接受 role:assistant 与非空 content 的任意组合，返回
// content 原始 JSON 片段与是否带 role。
func fastDeltaParts(p []byte, i int) (int, []byte, bool, bool) {
	i++
	var content []byte
	seenRole, seenContent := false, false
	for {
		i = fastSkipSpace(p, i)
		if i >= len(p) {
			return 0, nil, false, false
		}
		if p[i] == '}' {
			i++
			return i, content, seenRole, seenContent || seenRole
		}
		if p[i] != '"' {
			return 0, nil, false, false
		}
		keyStart := i + 1
		keyEnd := fastStringEnd(p, i)
		if keyEnd < 0 {
			return 0, nil, false, false
		}
		key := p[keyStart : keyEnd-1]
		i = fastSkipSpace(p, keyEnd)
		if i >= len(p) || p[i] != ':' {
			return 0, nil, false, false
		}
		i = fastSkipSpace(p, i+1)
		if i >= len(p) || p[i] != '"' {
			return 0, nil, false, false
		}
		valueEnd := fastStringEnd(p, i)
		if valueEnd < 0 || !fastCanonicalString(p[i:valueEnd]) {
			return 0, nil, false, false
		}
		switch {
		case bytes.Equal(key, []byte("role")):
			if seenRole || !bytes.Equal(p[i:valueEnd], []byte(`"assistant"`)) {
				return 0, nil, false, false
			}
			seenRole = true
		case bytes.Equal(key, []byte("content")):
			if seenContent || valueEnd-i <= 2 {
				return 0, nil, false, false
			}
			content = p[i:valueEnd]
			seenContent = true
		default:
			return 0, nil, false, false
		}
		i = fastSkipSpace(p, valueEnd)
		if i >= len(p) {
			return 0, nil, false, false
		}
		if p[i] == ',' {
			i++
			continue
		}
		if p[i] == '}' {
			i++
			return i, content, seenRole, seenContent || seenRole
		}
		return 0, nil, false, false
	}
}

// fastChoicesEnd 返回 choices 数组右方括号之后的位置。
func fastChoicesEnd(p []byte, i int) int {
	i++
	for {
		i = fastSkipSpace(p, i)
		if i >= len(p) {
			return -1
		}
		if p[i] == ']' {
			return i + 1
		}
		end := fastValueEnd(p, i)
		if end < 0 {
			return -1
		}
		i = fastSkipSpace(p, end)
		if i < len(p) && p[i] == ',' {
			i++
			continue
		}
		if i < len(p) && p[i] == ']' {
			return i + 1
		}
		return -1
	}
}

package httpauth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// VerifyBearer 校验请求头是否携带正确的 Bearer 密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare，常量时间，不因前缀匹配
// 长度而泄露信息。保留此入口是为了兼容既有调用方与测试。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// Key 是一个可鉴权的 API key 定义。Key 为明文，只在进程内存中用于校验；
// 任何 REST/归档/日志输出都必须使用 ID/Name，绝不能回显 Key 字段。
type Key struct {
	ID      string
	Name    string
	Key     string
	Realm   string   // "cn" / "global" / ""（不限制）
	Models  []string // 空 = 不限制；非空时仅允许这些模型（裸名，不含 realm: 前缀）
	Enabled bool
}

// DefaultLegacyKeyID 是顶层 api_key 兼容项的固定身份。它的语义必须与旧版完全
// 一致：不限制 realm、没有独立名称。
const DefaultLegacyKeyID = "legacy"

// Identity 是请求命中 key 后写入 context 的只读身份。
type Identity struct {
	ID     string
	Name   string
	Realm  string
	Models []string
}

// AllowsModel 判断身份是否允许 bareModel。空 Models = 不限制；非空时按
// shell 通配符匹配（支持 `deepseek/*`、`*-flash` 等 hub 现有写法）。
func (id Identity) AllowsModel(model string) bool {
	if len(id.Models) == 0 {
		return true
	}
	return AllowedModel(model, id.Models)
}

type identityKey struct{}

// VerifyBearerKey 校验请求的 Bearer 是否命中给定 key，命中时返回其身份。
// 空 key 表示该条目不参与鉴权（调用方应先把空 key 过滤掉）。
//
// 比较使用 SHA-256 摘要 + subtle.ConstantTimeCompare，且遍历所有条目而不是命中
// 即返回，避免不同 key 位置/数量导致可观测的耗时差异。
func VerifyBearerKey(r *http.Request, key Key) (Identity, bool) {
	return matchBearer(r, []Key{key})
}

// VerifyAny 按 keyring 校验请求。空 keyring 才表示“未启用鉴权”，放行且不带
// 身份；keyring 非空时，即使所有条目都已禁用，也必须拒绝请求。否则“先配置
// 多 key、后逐个禁用”会意外把网关变成匿名公开服务。
func VerifyAny(r *http.Request, keys []Key) (Identity, bool) {
	if len(keys) == 0 {
		return Identity{}, true
	}
	return matchBearer(r, keys)
}

// matchBearer 是常量时间多 key 匹配的唯一实现。
//
// 无论是否有 Authorization 头、命中第几条，都完整遍历所有 enabled key 并做一次
// 定长摘要比较；命中项只记录身份，不提前 break。这样请求耗时主要随 key 数量增长，
// 不随“命中位置”或“前缀正确长度”变化。
func matchBearer(r *http.Request, keys []Key) (Identity, bool) {
	tok := ""
	hasBearer := false
	if r != nil {
		authz := r.Header.Get("Authorization")
		if strings.HasPrefix(authz, bearerPrefix) {
			tok = authz[len(bearerPrefix):]
			hasBearer = true
		}
	}
	tokDigest := digest(tok)
	var hit Identity
	found := false
	for _, k := range keys {
		// 禁用/空 key 条目也参与一次摘要比较：比较次数只随配置条目数变化，
		// 不随 enabled 状态或命中位置变化。只有 enabled 的非空 key 才能命中。
		matched := subtleCompare(tokDigest, digest(k.Key))
		if matched && hasBearer && k.Enabled && strings.TrimSpace(k.Key) != "" && !found {
			hit = Identity{ID: k.ID, Name: k.Name, Realm: k.Realm, Models: append([]string(nil), k.Models...)}
			found = true
		}
	}
	return hit, found
}

// WithIdentity 把命中身份写入 context。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom 读取请求上下文中的 key 身份；未命中/未鉴权时 ok=false。
func IdentityFrom(r *http.Request) (Identity, bool) {
	if r == nil {
		return Identity{}, false
	}
	id, ok := r.Context().Value(identityKey{}).(Identity)
	return id, ok
}

// subtleCompare 是 subtle.ConstantTimeCompare 的薄封装，保持 matchBearer 读起来
// 只表达“比较”而不是导入细节。
func subtleCompare(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

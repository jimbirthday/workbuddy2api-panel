package httpauth

import "crypto/sha256"

// KeySpec 是配置层到 Keyring 的中立描述，避免 cmd/server 与 livecfg 各自
// 拼装 keyring 语义。Enabled 缺省由调用方按“条目是否提供 key”决定。
type KeySpec struct {
	ID      string
	Name    string
	Key     string
	Realm   string
	Models  []string
	Enabled bool
	// Legacy 为真时 key 为空不代表“脏条目”，而是“旧配置未启用鉴权”的兼容项。
	Legacy bool
}

// Keyring 把 legacy 顶层 api_key 与新增 api_keys 合并成唯一的鉴权列表。
// 返回的切片必须是新分配：livecfg 快照会被并发读取，不能与配置共享可变底层数组。
// key 摘要在此一次性预计算：鉴权在每请求路径上，key 只在配置变化时重算。
func Keyring(legacy string, specs []KeySpec) []Key {
	out := make([]Key, 0, len(specs)+1)
	if legacy != "" {
		out = append(out, Key{ID: DefaultLegacyKeyID, Key: legacy, Enabled: true, digest: sha256.Sum256([]byte(legacy))})
	}
	for _, s := range specs {
		if s.Key == "" && s.Legacy {
			continue
		}
		out = append(out, Key{
			ID:      s.ID,
			Name:    s.Name,
			Key:     s.Key,
			Realm:   s.Realm,
			Models:  append([]string(nil), s.Models...),
			Enabled: s.Enabled,
			digest:  sha256.Sum256([]byte(s.Key)),
		})
	}
	return out
}

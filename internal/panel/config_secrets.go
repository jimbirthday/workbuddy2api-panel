package panel

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
)

// redactHostPaths 把配置回显中可能暴露宿主机目录结构的路径收敛成 basename。
// 配置页只需要知道“已有值/文件名”，不需要完整 /app/... 或用户 home 路径。
// 保存仍是深合并：前端不回填的值不会覆盖原配置，因此这里只影响 GET 展示。
func redactHostPaths(cfg any) any {
	root, ok := configJSONMap(cfg)
	if !ok {
		return cfg
	}
	walk := func(m map[string]any, keys ...string) {
		for _, key := range keys {
			if raw, exists := m[key].(string); exists {
				m[key] = displayBaseName(raw)
			}
		}
	}
	walk(root, "auth_dir", "state_file", "config_path")
	// 刻意不处理 prompt.file：它是配置表单可直接编辑的字段，回显 basename 会被
	// 用户下一次保存原样写回，把原本的绝对路径降级成相对文件名，反而破坏部署。
	// 该字段属于“用户自己填的配置”，不是服务端必须隐藏的宿主路径。
	if logging, ok := root["logging"].(map[string]any); ok {
		walk(logging, "request_archive_dir", "activity_history_file")
	}
	return root
}

// maskedSecret 是管理接口回显所有密钥字段时的统一哨兵。前端原样提交该值
// 表示「保持原密钥不变」；空串同样保留，*_set=false 才显式清空。
const maskedSecret = "***"

// maskConfigSecrets 返回配置的深拷贝并把所有凭证替换为脱敏标记。
//
// 分层语义：
//   - api_key / api_keys[].key：整体替换为 ***，并附 api_key_set / key_set；
//   - upstream.device_token：整体替换为 ***，并附 device_token_set；
//   - upstash.token：整体替换为 ***，并附 token_set；
//   - upstash.url：只去掉 userinfo，保留 scheme/host/path 供排查，并附 url_redacted。
//
// 不修改 LoadConfig 返回的原对象，避免面板 GET 污染运行中的配置缓存。
func maskConfigSecrets(cfg any) any {
	root, ok := configJSONMap(cfg)
	if !ok {
		// 无法解析为 JSON 对象时失败关闭：宁可让配置页显示空对象，也不能把
		// 未知结构原样回显而泄露其中可能存在的凭证。
		return map[string]any{}
	}
	if _, exists := root["api_key"]; exists {
		root["api_key"], root["api_key_set"] = maskSecretValue(root["api_key"])
	}
	if list, ok := root["api_keys"].([]any); ok {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if _, exists := m["key"]; exists {
				m["key"], m["key_set"] = maskSecretValue(m["key"])
			}
		}
	}
	if up, ok := root["upstream"].(map[string]any); ok {
		if _, exists := up["device_token"]; exists {
			up["device_token"], up["device_token_set"] = maskSecretValue(up["device_token"])
		}
	}
	if up, ok := root["upstash"].(map[string]any); ok {
		if _, exists := up["token"]; exists {
			up["token"], up["token_set"] = maskSecretValue(up["token"])
		}
		if raw, exists := up["url"].(string); exists {
			safe, redacted := redactURLUserinfo(raw)
			up["url"] = safe
			up["url_redacted"] = redacted
		}
	}
	return root
}

// maskSecretValue 把非空凭证替换为哨兵；空值保持空串并标记未设置。
func maskSecretValue(v any) (any, bool) {
	s, _ := v.(string)
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	return maskedSecret, true
}

// redactURLUserinfo 去掉 URL 中的 user:password@ 部分。连接串里的用户名和
// 密码都按凭证处理（Upstash 的 default:<token> 形式尤其常见）。
func redactURLUserinfo(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err == nil {
		if u.User == nil {
			return raw, false
		}
		u.User = nil
		return u.String(), true
	}
	// url.Parse 对个别畸形连接串会直接报错；仍按 scheme://...@host 做一次
	// 保守剥离，避免解析失败反而把 userinfo 原样回显。
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			return raw[:i+3] + rest[at+1:], true
		}
	}
	return raw, false
}

// preserveBlankAPIKeys 在保存前把空白/*** 的凭证还原成旧值，并处理显式清空。
// 辅助字段仅用于 GET 回显，绝不写回配置文件。
func preserveBlankAPIKeys(raw []byte, load func() (any, error)) ([]byte, error) {
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if incoming == nil {
		return nil, fmt.Errorf("config body must be a JSON object")
	}

	var current map[string]any
	if load != nil {
		v, err := load()
		if err != nil {
			// 失败关闭：读取不到旧配置时，无法区分“真的清空”与“GET 回显的
			// ***/空串哨兵”。若继续以空 map 还原，会把 api_key 写成空串；
			// keyring 非空时旧 Key 可能被删、keyring 为空时网关会变成匿名放行。
			// 直接拒绝保存，比任何“尽量猜”都安全。
			return nil, fmt.Errorf("cannot preserve masked secrets: reload current config failed: %w", err)
		}
		current, _ = configJSONMap(v)
	}
	if current == nil {
		current = map[string]any{}
	}

	preserveSecretField(incoming, current, "api_key", "api_key_set")
	preserveAPIKeyEntries(incoming, current)
	preserveNestedSecret(incoming, current, "upstream", "device_token", "device_token_set")
	preserveNestedSecret(incoming, current, "upstash", "token", "token_set")
	preserveUpstashURL(incoming, current)

	out, err := json.Marshal(incoming)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return out, nil
}

// preserveSecretField 处理同一对象内的一个凭证字段。
//
// 默认（空串 / *** / 字段缺席）保留旧值；*_set=false 才显式清空。旧值不存在时
// 哨兵回落成空串，绝不把 *** 写进配置文件。
func preserveSecretField(incoming, current map[string]any, field, setField string) {
	clear := false
	if b, ok := incoming[setField].(bool); ok && !b {
		clear = true
	}
	delete(incoming, setField)
	if clear {
		incoming[field] = ""
		return
	}

	raw, exists := incoming[field]
	if !exists {
		return
	}
	s, _ := raw.(string)
	if strings.TrimSpace(s) != "" && s != maskedSecret {
		return
	}
	old, _ := current[field].(string)
	if old != "" {
		incoming[field] = old
		return
	}
	if s == maskedSecret {
		incoming[field] = ""
	}
}

func preserveNestedSecret(incoming, current map[string]any, section, field, setField string) {
	inSection, ok := incoming[section].(map[string]any)
	if !ok {
		return
	}
	curSection, _ := current[section].(map[string]any)
	preserveSecretField(inSection, curSection, field, setField)
}

func preserveAPIKeyEntries(incoming, current map[string]any) {
	list, ok := incoming["api_keys"].([]any)
	if !ok {
		return
	}
	oldKeys := map[string]string{}
	if oldList, ok := current["api_keys"].([]any); ok {
		for _, item := range oldList {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			key, _ := m["key"].(string)
			if id != "" && key != "" {
				oldKeys[id] = key
			}
		}
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		preserveSecretField(m, map[string]any{"key": oldKeys[id]}, "key", "key_set")
	}
}

// preserveUpstashURL 在 GET 去掉 userinfo 后恢复原始连接串。url_redacted
// 是「本次回显被裁剪」的标记，保存时据此换回旧 URL；空串/*** 同样默认保留。
func preserveUpstashURL(incoming, current map[string]any) {
	section, ok := incoming["upstash"].(map[string]any)
	if !ok {
		return
	}
	redacted := false
	if b, ok := section["url_redacted"].(bool); ok {
		redacted = b
	}
	delete(section, "url_redacted")

	raw, exists := section["url"]
	if !exists && !redacted {
		return
	}
	s, _ := raw.(string)
	if exists && strings.TrimSpace(s) != "" && s != maskedSecret && !redacted {
		return
	}
	curSection, _ := current["upstash"].(map[string]any)
	if old, _ := curSection["url"].(string); old != "" {
		section["url"] = old
		return
	}
	if s == maskedSecret {
		section["url"] = ""
	}
}

// configJSONMap 把 map 或生产注入的 *Config 统一转成可深拷贝的 JSON 对象。
func configJSONMap(v any) (map[string]any, bool) {
	switch x := v.(type) {
	case map[string]any:
		out, _ := deepCopyJSONLike(x).(map[string]any)
		return out, true
	case nil:
		return nil, false
	default:
		out, ok := reflectJSONObject(v)
		if !ok {
			return nil, false
		}
		return out, true
	}
}

// deepCopyJSONLike 复制 JSON 友好的 map/slice，避免脱敏/合并时修改调用方对象。
// 非 JSON 友好类型原样返回；配置来自 JSON 解析，正常路径均为 map/slice/标量。
func deepCopyJSONLike(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = deepCopyJSONLike(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = deepCopyJSONLike(v)
		}
		return out
	default:
		return v
	}
}

// reflectJSONObject 深拷贝真实配置结构体（生产注入的是 *server.Config），而不是
// 先 Marshal 再 Unmarshal：后者会把零值切片变成 null、给只写了少数键的结构体
// 补出大量零值字段，改变 GET 的响应形态。只用反射读取导出字段，并按项目的
// json tag 命名；`json:"-"` 字段忽略，omitempty 的零值字段跳过（与编码行为一致）。
func reflectJSONObject(v any) (map[string]any, bool) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, false
	}
	return reflectStructMap(rv), true
}

func reflectStructMap(rv reflect.Value) map[string]any {
	rt := rv.Type()
	out := make(map[string]any, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		name, omitEmpty, skip := jsonFieldName(sf)
		if skip {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		fv := rv.Field(i)
		if omitEmpty && fv.IsZero() {
			continue
		}
		out[name] = reflectJSONValue(fv)
	}
	return out
}

func jsonFieldName(sf reflect.StructField) (name string, omitEmpty, skip bool) {
	tag, hasTag := sf.Tag.Lookup("json")
	if !hasTag {
		return "", false, false
	}
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, opt := range parts[1:] {
		if opt == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func reflectJSONValue(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return reflectJSONValue(v.Elem())
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return reflectJSONValue(v.Elem())
	case reflect.Struct:
		return reflectStructMap(v)
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := make(map[string]any, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out[fmt.Sprint(iter.Key().Interface())] = reflectJSONValue(iter.Value())
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return nil
		}
		out := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			out[i] = reflectJSONValue(v.Index(i))
		}
		return out
	case reflect.Array:
		out := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			out[i] = reflectJSONValue(v.Index(i))
		}
		return out
	default:
		return v.Interface()
	}
}

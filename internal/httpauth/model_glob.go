package httpauth

import "strings"

// AllowedModel 判断模型名是否命中白名单。空白名单表示不限制。
//
// 与 hub 的 fnmatch 语义对齐：`*` 可跨 `/`，因此 `deepseek/*` 能匹配
// `deepseek/chat`；同时兼容配置里用逗号分隔的单个字符串。匹配不做大小写
// 折叠，模型 ID 是上游权威值。
func AllowedModel(model string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	for _, raw := range patterns {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if globMatch(p, model) {
				return true
			}
		}
	}
	return false
}

// globMatch 实现仅含 `*` 与 `?` 的通配匹配。使用贪心回溯，最坏 O(len(p)*len(s))，
// 但模型名与白名单都很短；相比 path.Match，它允许 `*` 跨 `/`。
func globMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, match := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star = pi
			match = si
			pi++
		case star >= 0:
			pi = star + 1
			match++
			si = match
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

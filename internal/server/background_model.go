package server

import (
	"encoding/json"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// rewriteBackgroundModel 在原始 body 上完成一次原子改写：返回新 body 与是否
// 命中。只在 model 字段确实变化时重新序列化，未命中直接返回原切片（零分配）。
//
// 与 rewriteModel 的差异：这里保留结构化 envelope 的其余字段由调用方处理；
// 该函数只负责“把 JSON 里的 model 换成 target”，不解释 realm 前缀。
func rewriteBackgroundModel(body []byte, original, target string) ([]byte, bool) {
	if strings.TrimSpace(original) == "" || strings.TrimSpace(target) == "" || original == target {
		return body, false
	}
	if len(body) == 0 {
		return body, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, false
	}
	raw, ok := obj["model"]
	if !ok {
		return body, false
	}
	var current string
	if err := json.Unmarshal(raw, &current); err != nil || current != original {
		return body, false
	}
	next, err := json.Marshal(target)
	if err != nil {
		return body, false
	}
	obj["model"] = next
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// backgroundModelRewrite 描述一次「客户端后台请求 → 免费目标」的改写。
// Original 只用于日志/统计；Empty 为真时表示没有命中，调用方应保持原值。
type backgroundModelRewrite struct {
	Original string
	Target   string
	Empty    bool
}

// backgroundModelAlias 判断 model 是否命中后台模型名单并返回改写目标。
//
// 规则与 hub 的 WB_BACKGROUND_MODEL_NAMES 对齐：大小写不敏感、支持 shell
// 通配符（如 gpt-5.6-*），但只在本地匹配，不请求上游。名单/目标任一为空即
// 关闭，避免「只配了名单」时把请求改写成空模型。
//
// 安全边界：调用方必须先在**原始模型名**上完成 Key 的 realm/模型白名单校验，
// 再调用本函数。否则受限 Key 可以用 gpt-5.6-terra（白名单内）被改写成
// deepseek-v4-pro（白名单外）。本函数刻意不自行鉴权，避免两套判断漂移。
func backgroundModelAlias(model, names, target string) backgroundModelRewrite {
	original := strings.TrimSpace(model)
	target = strings.TrimSpace(target)
	if original == "" || target == "" || strings.TrimSpace(names) == "" {
		return backgroundModelRewrite{Original: model, Empty: true}
	}
	// realm 前缀属于网关路由协议，不参与模型名匹配；否则 "global:gpt-5.6-terra"
	// 配裸名单永远命不中。改写目标保留自己的 realm 前缀（若配置了）。
	bare := bareModelForAlias(original)
	for _, raw := range strings.FieldsFunc(names, func(r rune) bool {
		return r == ',' || r == '，' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		pattern := strings.TrimSpace(raw)
		if pattern == "" {
			continue
		}
		if strings.EqualFold(pattern, bare) || httpauth.AllowedModel(bare, []string{pattern}) {
			return backgroundModelRewrite{Original: original, Target: target}
		}
	}
	return backgroundModelRewrite{Original: original, Empty: true}
}

// bareModelForAlias 去掉可选的 realm: 前缀。与 resolveModel 的裸名口径一致，
// 但只用一次 strings.Cut，避免为每个后台请求走完整路由解析。
func bareModelForAlias(model string) string {
	if realm, bare, ok := strings.Cut(model, ":"); ok {
		if realm == "cn" || realm == "global" {
			return bare
		}
	}
	return model
}

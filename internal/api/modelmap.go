// modelmap.go —— 模型映射：客户端发来的模型名按面板配置的映射表改写后再出站。
// 存储复用 settings 表的 model_mapping 键（JSON 对象 {"客户端模型":"实际发送模型"}），
// 每请求实时读（与 cache_enabled/proxy 设置同款先例），面板改完即时生效、无需重启。
// 未命中或未配置一律原样透传。
package api

import (
	"encoding/json"
	"strings"
)

// applyModelMapping 在映射表 JSON 里查找 model 的替换名。键匹配为「去空格 + 转小写」，
// 与 ResolvePostmanModel 的归一口径一致；目标为空白视为无效条目跳过。解析失败原样返回。
func applyModelMapping(mappingJSON, model string) string {
	mappingJSON = strings.TrimSpace(mappingJSON)
	if mappingJSON == "" || model == "" {
		return model
	}
	var mm map[string]string
	if json.Unmarshal([]byte(mappingJSON), &mm) != nil {
		return model
	}
	key := strings.ToLower(strings.TrimSpace(model))
	for k, v := range mm {
		if strings.ToLower(strings.TrimSpace(k)) == key && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return model
}

// applyModelMappingOnServer 读库里的映射设置并套用到 model。
func (s *Server) applyModelMapping(model string) string {
	v, _ := s.Store.GetSetting("model_mapping")
	return applyModelMapping(v, model)
}

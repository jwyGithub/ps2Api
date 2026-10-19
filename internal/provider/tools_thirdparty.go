package provider

import (
	"strings"
)

// MaxThirdPartyTools 是 thirdParty 工具的出站数量硬上限。
// 2026-10-19 线上实测（二分探针，opus-4-8 与 opus-5-5 同结果）：上游按「Agent Mode
// 启用工具总数」卡上限——报数 = thirdParty 数 + 12（恒定偏移，nativeToolsHash 全集里
// 未被 excludedTools 盖住的计数口径），边界 thirdParty 108 过 / 109 挂 → 总数上限 120
// （"Agent Mode has 121 tools enabled, which is more than this model supports… |
// Tool limit exceeded"）。超限按客户端声明顺序保留前 108 个——被裁掉的 MCP 工具模型
// 不调而已，客户端本地工具执行不受影响。
const MaxThirdPartyTools = 108

func (p *Provider) buildThirdPartyTools(tools []interface{}) map[string]interface{} {
	if len(tools) == 0 {
		return map[string]interface{}{}
	}
	var mcpTools []map[string]interface{}
	for _, tool := range tools {
		name := extractToolName(tool)
		if name == "" || isClientReservedTool(name) {
			continue
		}
		if len(mcpTools) >= MaxThirdPartyTools {
			break
		}
		desc := extractToolDesc(tool)
		if desc == "" {
			desc = name
		} else if len(desc) > MaxToolDescLen {
			desc = strings.ToValidUTF8(desc[:MaxToolDescLen], "")
		}
		params := compactToolSchema(extractToolSchema(tool))
		mcpTools = append(mcpTools, map[string]interface{}{
			"name": name, "description": desc, "parameters": params,
		})
	}
	if len(mcpTools) == 0 {
		return map[string]interface{}{}
	}
	return map[string]interface{}{"proxy-tools": map[string]interface{}{"tools": mcpTools}}
}

func compactToolSchema(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, child := range v {
			switch key {
			case "description", "title", "examples", "default", "$comment":
				continue
			}
			out[key] = compactToolSchema(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, child := range v {
			out[i] = compactToolSchema(child)
		}
		return out
	default:
		return value
	}
}

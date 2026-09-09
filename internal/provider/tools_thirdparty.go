package provider

import (
	"strings"
)

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

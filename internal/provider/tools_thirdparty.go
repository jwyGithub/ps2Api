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

// compactThirdPartyTools keeps the callable tool names while dropping the large
// schema/docs envelope for a single gateway retry. The client still owns the
// real schemas and executes the returned tool calls, so names remain necessary.
func compactThirdPartyTools(value map[string]interface{}) map[string]interface{} {
	proxy, ok := value["proxy-tools"].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	tools, ok := proxy["tools"].([]map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	compact := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		compact = append(compact, map[string]interface{}{
			"name": name,
			"parameters": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": true,
			},
		})
	}
	if len(compact) == 0 {
		return map[string]interface{}{}
	}
	return map[string]interface{}{"proxy-tools": map[string]interface{}{"tools": compact}}
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

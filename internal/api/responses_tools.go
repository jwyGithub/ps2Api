package api

import (
	"encoding/json"
	"strings"
)

// 本文件集中处理 Responses 入站的工具声明收集与「custom 工具 → function 形状」桥接，
// 对齐 raycast2api 的可用实现（codex 客户端实测可用）：
//   - Codex 把工具声明塞在 input 的 additional_tools 项里（functions namespace 嵌套），
//     不在顶层 tools —— 必须递归剥壳合并，否则模型看不到任何工具。
//   - type:"custom" 的自由文本工具（如 exec，lark 文法）没有 JSON Schema，
//     桥接成「单 input string 参数 + 文法折进 description」的 function 工具注册给上游，
//     出站时该名字的调用再渲染回 custom_tool_call（见 responses.go / responses_stream.go）。

// flattenResponsesTools 合并顶层 tools 与 input 里所有 additional_tools 项的工具，
// 递归剥 namespace 壳（元素带非空 tools 数组即 namespace 组，取其成员；空组跳过），
// custom 工具桥接成 function 形状。返回合并后的工具列表与 custom 工具裸名集合。
func flattenResponsesTools(top []map[string]interface{}, items []respInputItem) ([]interface{}, map[string]bool) {
	var out []interface{}
	custom := map[string]bool{}
	flattenToolList(top, &out, custom)
	for _, it := range items {
		if it.Type == "additional_tools" {
			flattenToolList(it.Tools, &out, custom)
		}
	}
	return out, custom
}

// flattenToolList 递归展平一组工具声明：namespace 组取成员、custom 桥接、其余原样收下。
func flattenToolList(list []map[string]interface{}, out *[]interface{}, custom map[string]bool) {
	for _, t := range list {
		if t == nil {
			continue
		}
		// namespace 组：元素自身只是标签，真正的工具在其 tools 数组里（可能再嵌套）。
		if nested, ok := t["tools"].([]interface{}); ok && len(nested) > 0 {
			for _, item := range nested {
				if m, ok := item.(map[string]interface{}); ok {
					flattenToolList([]map[string]interface{}{m}, out, custom)
				}
			}
			continue
		}
		if typ, _ := t["type"].(string); typ == "namespace" {
			continue // 空 namespace 组
		}
		if typ, _ := t["type"].(string); typ == "custom" {
			if name, _ := t["name"].(string); name != "" {
				custom[name] = true
				*out = append(*out, bridgeCustomTool(t))
				continue
			}
		}
		*out = append(*out, t)
	}
}

// bridgeCustomTool 把 type:"custom" 的自由文本工具改写成 function 形状：
// 单 input string 参数承载原始文本，lark 文法（format.definition）折进 description，
// 让只有 JSON Schema 概念的上游（thirdParty proxy-tools）也能注册它。
func bridgeCustomTool(m map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"type": "function",
		"name": m["name"],
		"parameters": map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{"input": map[string]interface{}{"type": "string"}},
			"required":             []string{"input"},
			"additionalProperties": false,
		},
	}
	desc, _ := m["description"].(string)
	grammar := ""
	if f, ok := m["format"].(map[string]interface{}); ok {
		grammar, _ = f["definition"].(string)
	}
	if grammar != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += "Input grammar:\n" + grammar
	}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// customInputOf 从桥接后的 arguments（{"input":"..."}）里 unwrap 出自由文本 input；
// 不是该形状则把 arguments 原样当 input 透传（对齐 raycast2api custom_input）。
func customInputOf(args string) string {
	var v struct {
		Input string `json:"input"`
	}
	if json.Unmarshal([]byte(args), &v) == nil && v.Input != "" {
		return v.Input
	}
	return args
}

// registeredToolNames 收集本轮已注册工具的裸名集合（供出站纠名用）。
func registeredToolNames(tools []interface{}) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		m, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if fn, ok := m["function"].(map[string]interface{}); ok {
			if n, _ := fn["name"].(string); n != "" {
				out[n] = true
			}
		}
		if n, _ := m["name"].(string); n != "" {
			out[n] = true
		}
	}
	return out
}

// stripUpstreamToolPrefix 纠正上游回吐时被改写的工具名：Postman thirdParty 机制可能给
// MCP 工具名加 namespace 前缀（历史 trace：functions.exec / functions__exec），而客户端
// 只认自己声明的裸名。仅当剥前缀后的裸名确实在本轮注册集里才剥，其余原样返回。
func stripUpstreamToolPrefix(name string, registered map[string]bool) string {
	if registered[name] || len(registered) == 0 {
		return name
	}
	for _, sep := range []string{".", "__"} {
		if i := strings.Index(name, sep); i > 0 {
			if bare := name[i+len(sep):]; registered[bare] {
				return bare
			}
		}
	}
	return name
}

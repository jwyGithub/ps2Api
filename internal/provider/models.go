package provider

import "strings"

// PostmanModelMap 对外模型名 -> Postman 内部 modelKey
var PostmanModelMap = map[string]string{
	"claude-opus-4-8":   "CLAUDE_OPUS_48_BEDROCK",
	"claude-opus-4-7":   "CLAUDE_OPUS_47_BEDROCK",
	"claude-opus-4-6":   "CLAUDE_OPUS_46_BEDROCK",
	"claude-opus-4-5":   "CLAUDE_OPUS_45_BEDROCK",
	"claude-sonnet-4-6": "CLAUDE_46_SONNET_BEDROCK",
	"claude-sonnet-4-5": "CLAUDE_45_SONNET_BEDROCK",
	"claude-haiku-4-5":  "CLAUDE_45_HAIKU_BEDROCK",
	"gpt-5.6-sol":       "GPT_56_SOL",
	"gpt-5.6-terra":     "GPT_56_TERRA",
	"gpt-5.6-luna":      "GPT_56_LUNA",
	"gpt-5.5":           "GPT_55",
	"gpt-5.4":           "GPT_54",
	"gpt-5.2":           "GPT_52",
	"auto":              "",
}

const DefaultPostmanModel = "CLAUDE_OPUS_48_BEDROCK"

func ResolvePostmanModel(model string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(model))
	v, ok := PostmanModelMap[key]
	if !ok {
		return "", false
	}
	if v == "" {
		return DefaultPostmanModel, true
	}
	return v, true
}

type ModelInfo struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxOutput     int    `json:"max_output,omitempty"`
	Thinking      bool   `json:"thinking,omitempty"`
}

func pm(id string, ctx, maxOut int, thinking bool) ModelInfo {
	return ModelInfo{ID: id, Object: "model", Created: 1700000000, OwnedBy: "postman", ContextWindow: ctx, MaxOutput: maxOut, Thinking: thinking}
}

var PostmanModels = []ModelInfo{
	pm("claude-opus-4-8", 200000, 64000, true),
	pm("claude-opus-4-7", 200000, 64000, true),
	pm("claude-opus-4-6", 200000, 64000, true),
	pm("claude-opus-4-5", 200000, 64000, true),
	pm("claude-sonnet-4-6", 200000, 64000, true),
	pm("claude-sonnet-4-5", 200000, 64000, true),
	pm("claude-haiku-4-5", 200000, 64000, false),
	pm("gpt-5.6-sol", 128000, 32000, true),
	pm("gpt-5.6-terra", 128000, 32000, true),
	pm("gpt-5.6-luna", 128000, 32000, true),
	pm("gpt-5.5", 128000, 32000, false),
	pm("gpt-5.4", 128000, 32000, false),
	pm("gpt-5.2", 128000, 32000, false),
	pm("auto", 200000, 64000, false),
	// toolsets 端点专属模型（/chat 白名单外）：走 ToolsetsProvider 原生透传。
	// maxOut 标 8096（ToolsetsMaxTokens 上游硬夹紧值），客户端照此填写避免被静默夹断。
	pm("claude-opus-5", 200000, 8096, true),
	pm("claude-sonnet-5", 200000, 8096, true),
	pm("claude-opus-5-5", 200000, 8096, true),
	pm("claude-sonnet-5-5", 200000, 8096, true),
	// toolsets Responses 端点（openai/openai-responses 路由）：走 ToolsetsProvider.ChatResponses
	// /StreamChatResponses 原生透传（仅 /v1/responses 端点可调）。只挂 /chat 白名单外的
	// gpt-6 系——gpt-5.x 走既有 /chat 管线（PostmanModelMap）。
	pm("gpt-6-sol", 400000, 8096, true),
	pm("gpt-6.1-sol", 400000, 8096, true),
	pm("gpt-6-luna", 400000, 8096, true),
}

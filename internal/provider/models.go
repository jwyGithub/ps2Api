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
	// gpt-6 系（2026-10-22 桌面客户端 12.33.2 逆向）：/chat 端 devModeOptions.selectedModel
	// 已接受 GPT_61_SOL（leveldb agentModeSettings 实锤，webview 从 GET gateway.postman.com/
	// config?platform=... 实时拉 models[].key 下发）。GPT_6_SOL / GPT_6_LUNA 按同族命名规则推定，
	// 上游 /config 是权威清单，拉取成功后以 DynamicModelKeys 为准。
	"gpt-6-sol":         "GPT_6_SOL",
	"gpt-6.1-sol":       "GPT_61_SOL",
	"gpt-6-luna":        "GPT_6_LUNA",
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
		// 静态表未命中 → 动态清单兜底（/config 实时拉取的 models[].key，键为对外名小写）。
		// 上游加新模型而网关未发版时，改映射或直接用 key 小写名即可路由。
		return resolveDynamicModel(key)
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
	// gpt-6 系：走 /chat 管线（devModeOptions.selectedModel=GPT_6x_SOL 等，2026-10-22 起），
	// 不再走 toolsets Responses 透传。上限对齐 gpt-5.6 系（/chat 无 8096 夹紧）。
	pm("gpt-6-sol", 400000, 32000, true),
	pm("gpt-6.1-sol", 400000, 32000, true),
	pm("gpt-6-luna", 400000, 32000, true),
	// toolsets 端点专属模型（/chat 白名单外）：走 ToolsetsProvider 原生透传（/v1/messages）。
	// maxOut 标 8096（ToolsetsMaxTokens 上游硬夹紧值），客户端照此填写避免被静默夹断。
	pm("claude-opus-5", 200000, 8096, true),
	pm("claude-sonnet-5", 200000, 8096, true),
	pm("claude-opus-5-5", 200000, 8096, true),
	pm("claude-sonnet-5-5", 200000, 8096, true),
}

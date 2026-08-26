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
	// codex-mini-latest:codex 客户端常用的模型名别名,映射到 GPT_56_SOL,上游仍走 gpt-5.6-sol。
	// 注:工具执行现在靠运行时探测客户端声明的 exec custom 工具(见 api/codex_exec.go),
	// 与模型名无关;此条目仅为兼容仍以 codex-mini-latest 请求的客户端而保留。
	"codex-mini-latest": "GPT_56_SOL",
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
	pm("codex-mini-latest", 128000, 32000, true),
	pm("gpt-5.6-terra", 128000, 32000, true),
	pm("gpt-5.6-luna", 128000, 32000, true),
	pm("gpt-5.5", 128000, 32000, false),
	pm("gpt-5.4", 128000, 32000, false),
	pm("gpt-5.2", 128000, 32000, false),
	pm("auto", 200000, 64000, false),
}

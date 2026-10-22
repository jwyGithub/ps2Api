package api

import (
	"strings"
	"testing"

	"ps2api/internal/provider"
)

// TestModelListNoDuplicateUpstreamModels 锁住 /v1/models 合并去重：动态 /config key
// （下划线形态）与静态对外名（连字符）指同一上游模型时只保留静态条目，不出现重复。
func TestModelListNoDuplicateUpstreamModels(t *testing.T) {
	// 模拟已拉取动态清单：含静态表已覆盖的模型 + 一个真正的新模型。
	provider.ApplyDynamicModels([]provider.AgentModeModel{
		{Key: "GPT_61_SOL", DisplayName: "GPT-6.1 Sol"},
		{Key: "CLAUDE_OPUS_48_BEDROCK", DisplayName: "Claude Opus 4.8"},
		{Key: "GPT_NEW_X", DisplayName: "GPT New X"},
	}, nil)
	defer provider.ApplyDynamicModels(nil, nil)

	s := &Server{}
	list := s.modelList()
	seen := map[string]bool{}
	for _, m := range list {
		id := strings.ToLower(m.ID)
		if seen[id] {
			t.Fatalf("duplicate model id in /v1/models: %s", id)
		}
		seen[id] = true
	}
	// 静态条目仍在；动态新模型以下划线 key 的小写形态出现。
	for _, want := range []string{"gpt-6.1-sol", "claude-opus-4-8", "gpt_new_x"} {
		if !seen[want] {
			t.Errorf("model %q missing from merged list", want)
		}
	}
	// 被静态覆盖的上游模型不得以下划线形态重复出现。
	if seen["gpt_61_sol"] || seen["claude_opus_48_bedrock"] {
		t.Error("static-covered upstream keys leaked as duplicate underscore entries")
	}
}

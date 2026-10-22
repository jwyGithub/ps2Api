package provider

import "testing"

// TestResolveGPT6ChatModels 锁住 gpt-6 系走 /chat 的静态映射（2026-10-22 /config
// 实测三 key：GPT_61_SOL / GPT_6_SOL / GPT_6_LUNA，primaryModelKeys 均在列）。
func TestResolveGPT6ChatModels(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gpt-6.1-sol", "GPT_61_SOL"},
		{"gpt-6-sol", "GPT_6_SOL"},
		{"gpt-6-luna", "GPT_6_LUNA"},
		{"GPT-6.1-SOL", "GPT_61_SOL"}, // 大小写归一
	}
	for _, c := range cases {
		got, ok := ResolvePostmanModel(c.in)
		if !ok || got != c.want {
			t.Errorf("ResolvePostmanModel(%q) = %q,%v; want %q", c.in, got, ok, c.want)
		}
	}
	// gpt-5.x 既有映射不受影响。
	if got, _ := ResolvePostmanModel("gpt-5.6-sol"); got != "GPT_56_SOL" {
		t.Errorf("gpt-5.6-sol mapping changed: %q", got)
	}
}

// TestApplyDynamicModels 验证 /config 动态清单合并与解析优先级（静态表优先）。
func TestApplyDynamicModels(t *testing.T) {
	ApplyDynamicModels([]AgentModeModel{
		{Key: "GPT_NEW_X", DisplayName: "GPT New X"},
		{Key: "GPT_61_SOL", DisplayName: "GPT-6.1 Sol"},
	}, []string{"GPT_NEW_X"})
	defer ApplyDynamicModels(nil, nil)

	keys := DynamicModelKeys()
	if len(keys) != 2 || keys[0] != "GPT_61_SOL" || keys[1] != "GPT_NEW_X" {
		t.Errorf("DynamicModelKeys = %v", keys)
	}
	// 动态新模型可直接解析为 selectedModel 值。
	if got, ok := resolveDynamicModel("gpt_new_x"); !ok || got != "GPT_NEW_X" {
		t.Errorf("resolveDynamicModel(gpt_new_x) = %q,%v", got, ok)
	}
	// 静态表优先：gpt-6.1-sol 不被动态别名覆盖。
	v, ok := PostmanModelMap["gpt-6.1-sol"]
	if !ok || v != "GPT_61_SOL" {
		t.Errorf("static map must win: %q,%v", v, ok)
	}
	// 未拉取（清空缓存）时动态解析返回未命中。
	ApplyDynamicModels(nil, nil)
	if _, ok := resolveDynamicModel("gpt_new_x"); ok {
		t.Error("cleared cache must not resolve")
	}
}

// TestPostmanModelsNoToolsetsResponsesGPT6 确认 /v1/models 清单里 gpt-6 系不再标
// 8096 toolsets 上限（已改走 /chat，上限对齐 gpt-5.6 系的 32000）。
func TestPostmanModelsNoToolsetsResponsesGPT6(t *testing.T) {
	for _, m := range PostmanModels {
		if m.ID == "gpt-6-sol" || m.ID == "gpt-6.1-sol" || m.ID == "gpt-6-luna" {
			if m.MaxOutput == 8096 {
				t.Errorf("%s still carries toolsets 8096 cap", m.ID)
			}
		}
	}
}

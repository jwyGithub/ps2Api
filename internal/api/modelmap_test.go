package api

import "testing"

func TestApplyModelMapping(t *testing.T) {
	cases := []struct {
		name, mapping, model, want string
	}{
		{"未配置透传", "", "claude-opus-5-5", "claude-opus-5-5"},
		{"命中映射", `{"claude-opus-5-5":"claude-opus-4-8"}`, "claude-opus-5-5", "claude-opus-4-8"},
		{"键大小写与空格不敏感", `{"Claude-Opus-5-5":"claude-opus-4-8"}`, " claude-opus-5-5 ", "claude-opus-4-8"},
		{"未命中透传", `{"gpt-9":"gpt-5.5"}`, "claude-opus-4-8", "claude-opus-4-8"},
		{"坏 JSON 透传", "not-json", "claude-opus-5-5", "claude-opus-5-5"},
		{"空目标视为无效条目", `{"claude-opus-5-5":"  "}`, "claude-opus-5-5", "claude-opus-5-5"},
	}
	for _, c := range cases {
		if got := applyModelMapping(c.mapping, c.model); got != c.want {
			t.Errorf("%s: applyModelMapping(%q, %q) = %q, want %q", c.name, c.mapping, c.model, got, c.want)
		}
	}
}

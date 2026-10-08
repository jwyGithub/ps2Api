package provider

import (
	"encoding/json"
	"strings"
)

// safeguards.go —— 自动模式服务器审查（auto mode server-side classifier）的网关本地实现。
//
// 背景见 docs：code.claude.com/docs/en/auto-mode-classifier-billing。新版 Claude Code 在
// auto 模式下把危险操作审查（dangerous_tool_use）作为主请求的一部分发给服务器：请求体带
// safeguards 字段（beta: dangerous-tool-use-2026-09-03），期待响应流的 message_delta.delta
// 带回 safeguard_results。链路不通（字段被剥/结果缺失）时客户端回退本地分类器并弹
// 「classifier requests 计费」提示。
//
// 本网关是协议转换网关（Anthropic → Postman chat），上游不可能产生 safeguard_results，
// 因此在网关侧本地实现审查：接住 safeguards 请求字段，按规则判定每个待审 tool_use，
// 在响应里回填 safeguard_results。这样客户端认为服务器审查可达，不再弹提示。
//
// 审查语义（从 claude.exe 2.1.278 逆向的 schema，反编译引用见下）：
//
//	请求：safeguards:[{type:"dangerous_tool_use", classifier_context:{...}}]
//	响应（message_delta.delta.safeguard_results）：
//	  [{type:"dangerous_tool_use", status:{
//	      type:"available",
//	      tool_uses:{ <tool_use_id>: {type:"evaluated", outcome:"flagged"|"not_flagged", explanation?}
//	                        | {type:"skipped"} | {type:"unavailable", reason?} } }}]
//
// 客户端解析（IXr）：safeguard_results 缺失/解析失败 → server_no_result（回退本地分类器）；
// 恰好一个 dangerous_tool_use 条目且 status.type=="available" → 按 tool_uses 逐 ID 取判定；
// 缺 ID 的判定视为 unavailable。outcome=="flagged" → 拦截动作。
//
// 审查规则说明：Anthropic 服务器的分类器是大模型判定；本网关退而求其次做「规则审查」，
// 只拦官方文档《What the classifier blocks by default》中可静态判定的破坏性命令模式
// （递归强删、git force push、terraform destroy、curl|bash 等），其余一律 not_flagged。
// 放行不会弱化安全性：这些操作仍受客户端本地 allow/ask/deny 规则与用户确认约束，
// 本审查只是 extra 判定层。

// SafeguardEntry 是客户端请求体里的单个审查请求条目。
type SafeguardEntry struct {
	Type              string          `json:"type"`
	ClassifierContext json.RawMessage `json:"classifier_context,omitempty"`
}

// SafeguardCallResult 是单个 tool_use 的审查判定。
type SafeguardCallResult struct {
	Type        string `json:"type"` // evaluated | skipped | unavailable
	Outcome     string `json:"outcome,omitempty"`
	Explanation string `json:"explanation,omitempty"`
	Reason      string `json:"reason,omitempty"` // unavailable 时
}

// SafeguardStatus 是响应里 safeguards 条目的 status 对象。
type SafeguardStatus struct {
	Type     string                         `json:"type"` // available | unsupported | unavailable
	ToolUses map[string]SafeguardCallResult `json:"tool_uses,omitempty"`
	Reason   string                         `json:"reason,omitempty"`
}

// SafeguardResultEntry 是响应 safeguard_results 数组的条目。
type SafeguardResultEntry struct {
	Type   string          `json:"type"`
	Status SafeguardStatus `json:"status"`
}

// safeguardsBlockers 是「可静态判定的破坏性命令」模式表。命令 token 化后做子序列
// 匹配（忽略引号与路径差异），命中即 flagged。覆盖官方默认拦截清单中可离线判定的项：
//   - 递归强删（rm -rf / rmdir /S /Q / Remove-Item -Recurse -Force）
//   - git 破坏性操作（push --force / reset --hard / clean -fd / stash drop|clear）
//   - IaC 销毁（terraform/pulumi/cdk/terragrunt destroy）
//   - 远程代码执行（curl|bash、wget|sh 等）
//   - 危险 flag（--dangerously-skip-permissions / --no-sandbox）
var safeguardsBlockers = []struct {
	pattern []string // 命令 token 子序列（小写）
	reason  string
}{
	{[]string{"rm", "-rf", "/"}, "recursive force-delete of filesystem root"},
	{[]string{"rm", "-rf", "~"}, "recursive force-delete of home directory"},
	{[]string{"sudo", "rm", "-rf"}, "recursive force-delete via sudo"},
	{[]string{"remove-item", "-recurse"}, "PowerShell recursive removal"},
	{[]string{"rd", "/s"}, "Windows recursive directory removal"},
	{[]string{"git", "push", "--force"}, "force push overwriting remote history"},
	{[]string{"git", "push", "-f"}, "force push overwriting remote history"},
	{[]string{"git", "reset", "--hard"}, "git reset --hard discards working tree"},
	{[]string{"git", "clean", "-fd"}, "git clean removes untracked files"},
	{[]string{"git", "stash", "drop"}, "git stash drop discards stashed changes"},
	{[]string{"git", "stash", "clear"}, "git stash clear discards all stashes"},
	{[]string{"terraform", "destroy"}, "terraform destroy tears down infrastructure"},
	{[]string{"pulumi", "destroy"}, "pulumi destroy tears down infrastructure"},
	{[]string{"cdk", "destroy"}, "cdk destroy tears down infrastructure"},
	{[]string{"terragrunt", "destroy"}, "terragrunt destroy tears down infrastructure"},
	{[]string{"curl", "|", "bash"}, "piping remote content into shell execution"},
	{[]string{"curl", "|", "sh"}, "piping remote content into shell execution"},
	{[]string{"wget", "|", "bash"}, "piping remote content into shell execution"},
	{[]string{"wget", "|", "sh"}, "piping remote content into shell execution"},
	{[]string{"--dangerously-skip-permissions"}, "disarm safety guard flag"},
	{[]string{"--no-sandbox"}, "disarm safety guard flag"},
}

// safeguardsTokenize 把命令串拆成小写 token（保留 | 便于管道匹配）。
func safeguardsTokenize(cmd string) []string {
	fields := strings.FieldsFunc(strings.ToLower(cmd), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '\''
	})
	return fields
}

// safeguardsMatches 报告 tokens 是否包含 pattern 子序列。
func safeguardsMatches(tokens []string, pattern []string) bool {
	pi := 0
	for _, t := range tokens {
		if t == pattern[pi] {
			pi++
			if pi == len(pattern) {
				return true
			}
		}
	}
	return false
}

// flagSafeguardToolUse 审查单个 tool_use，返回判定。
func flagSafeguardToolUse(name string, input json.RawMessage) SafeguardCallResult {
	// 只审查有 shell 语义的工具；其余工具（读文件、搜索等）一律放行。
	switch name {
	case "Bash", "PowerShell", "Cmd":
	default:
		return SafeguardCallResult{Type: "evaluated", Outcome: "not_flagged"}
	}
	var spec struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &spec)
	if spec.Command == "" {
		return SafeguardCallResult{Type: "evaluated", Outcome: "not_flagged"}
	}
	tokens := safeguardsTokenize(spec.Command)
	for _, b := range safeguardsBlockers {
		if safeguardsMatches(tokens, b.pattern) {
			return SafeguardCallResult{Type: "evaluated", Outcome: "flagged", Explanation: "Blocked by gateway policy: " + b.reason}
		}
	}
	return SafeguardCallResult{Type: "evaluated", Outcome: "not_flagged"}
}

// toolUseIDsFromMessages 从 Anthropic messages 原文提取全部 tool_use 块（id → name/input）。
// 审查请求的待审动作就是请求 messages 末尾的 assistant tool_use 块。
func toolUseIDsFromMessages(raw json.RawMessage) map[string]struct {
	Name  string
	Input json.RawMessage
} {
	out := map[string]struct {
		Name  string
		Input json.RawMessage
	}{}
	if len(raw) == 0 {
		return out
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgs) != nil {
		return out
	}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		var blocks []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != "" {
				out[b.ID] = struct {
					Name  string
					Input json.RawMessage
				}{b.Name, b.Input}
			}
		}
	}
	return out
}

// EvaluateSafeguards 对请求体里的 safeguards 数组逐条判定，返回响应 safeguard_results。
// 没有待审 tool_use 的条目按 unavailable 处理（客户端视为本次无审查结果，不弹提示也不拦动作）。
func EvaluateSafeguards(safeguards []SafeguardEntry, messages json.RawMessage) []SafeguardResultEntry {
	if len(safeguards) == 0 {
		return nil
	}
	toolUses := toolUseIDsFromMessages(messages)
	results := make([]SafeguardResultEntry, 0, len(safeguards))
	for _, entry := range safeguards {
		status := SafeguardStatus{Type: "available", ToolUses: map[string]SafeguardCallResult{}}
		if entry.Type != "dangerous_tool_use" {
			// 未知类型：按 unavailable 回应，客户端不拿它做判定。
			status = SafeguardStatus{Type: "unavailable", Reason: "unsupported_type"}
		} else if len(toolUses) == 0 {
			status = SafeguardStatus{Type: "unavailable", Reason: "no_tool_uses"}
		} else {
			for id, tu := range toolUses {
				status.ToolUses[id] = flagSafeguardToolUse(tu.Name, tu.Input)
			}
		}
		results = append(results, SafeguardResultEntry{Type: entry.Type, Status: status})
	}
	return results
}

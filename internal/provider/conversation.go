package provider

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 会话指纹需要在"多轮之间"稳定。Claude Code 等无状态客户端每一轮都会把完整历史
// 重发，并在其中注入随轮变化的包装块（<system-reminder>…、<total_tokens>… 及时间戳等）。
// 若把这些易变片段计入指纹，历史前缀每轮都变 → LookupConversation 必然落空 → 退回被上游
// 拒收的 seedingMessages 路径。这里在计算指纹前剥离这些易变包装，只保留稳定正文，
// 使续聊能稳定命中同一个 Postman conversationId（对齐网页版靠 conversationId 续接的行为）。
var (
	volatileSystemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	volatileTotalTokensRe    = regexp.MustCompile(`(?s)<total_tokens>.*?</total_tokens>`)
)

// stableFingerprintText 去除随轮变化的注入包装，返回用于指纹计算的稳定文本。
// 注意：只影响"指纹匹配"，不改变真正发往上游的 query 文本。
func stableFingerprintText(s string) string {
	s = volatileSystemReminderRe.ReplaceAllString(s, "")
	s = volatileTotalTokensRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func conversationFingerprint(messages []ChatMessage) string {
	parts := make([]string, 0, len(messages)*4)
	for _, m := range messages {
		parts = append(parts, m.Role, stableFingerprintText(ExtractText(m.Content)), m.ToolCallID, toolCallFingerprint(m))
	}
	return fingerprint(parts...)
}

func toolCallFingerprint(m ChatMessage) string {
	if len(m.ToolCalls) == 0 {
		return ""
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(m.ToolCalls, &calls) != nil {
		return string(m.ToolCalls)
	}
	var b strings.Builder
	for _, c := range calls {
		b.WriteString(c.ID)
		b.WriteByte(':')
		b.WriteString(c.Function.Name)
		b.WriteByte(':')
		b.WriteString(c.Function.Arguments)
		b.WriteByte(';')
	}
	return b.String()
}

func hasReusableHistory(messages []ChatMessage) bool {
	for _, m := range messages {
		if m.Role == "assistant" || m.Role == "tool" || isAnthropicToolResult(m) {
			return true
		}
	}
	return false
}

// HasReusableHistory 报告消息里是否含可复用的会话历史（assistant / tool / Anthropic tool_result）。
// router 用它区分两类请求，对网关(Cloudflare 403)拦截采取截然不同的策略：
//   - true（续聊）：Postman 服务端已有该会话，绑定在首次使用的账号上。换号会丢掉服务端会话，
//     请求被降级为 USER_QUERY（历史折叠进单条 query，虽不截断但历史工具输出会被省略），
//     故遇 403 必须原号退避重试、绝不换号、也不得改写 req.Messages
//     （改写会破坏会话指纹 → 触发静默换号与降级）。
//   - false（新对话）：无服务端会话可丢，遇 403 可安全换号 failover。
func HasReusableHistory(messages []ChatMessage) bool { return hasReusableHistory(messages) }

// LookupConversation 按消息历史前缀匹配已有 Postman 会话。
// 新对话（没有 assistant/tool）一律开新会话，避免不同 agent 串上下文。
func (p *Provider) LookupConversation(accountID int64, messages []ChatMessage) string {
	if !hasReusableHistory(messages) {
		return ""
	}
	for i := len(messages) - 1; i >= 1; i-- {
		if id, ok := p.convStore.GetConversation(accountID, conversationFingerprint(messages[:i])); ok {
			return id
		}
	}
	return ""
}

func (p *Provider) setConversationID(accountID int64, messages []ChatMessage, id string) {
	if id == "" || len(messages) == 0 {
		return
	}
	fp := conversationFingerprint(messages)
	p.convStore.PutConversation(accountID, fp, id)
	p.convStore.PutOwner(fp, accountID)
}

// StickyAccount 返回该消息历史（会话）首次使用的账号，供路由层做会话粘性：
// 续聊请求固定回原账号，避免池子轮询换号导致上下文丢失。
// 新对话（无 assistant/tool 历史）返回 false。
func (p *Provider) StickyAccount(messages []ChatMessage) (int64, bool) {
	if !hasReusableHistory(messages) {
		return 0, false
	}
	for i := len(messages) - 1; i >= 1; i-- {
		if owner, ok := p.convStore.GetOwner(conversationFingerprint(messages[:i])); ok {
			return owner, true
		}
	}
	return 0, false
}

func assistantFollowup(res *Result) *ChatMessage {
	if res == nil || (res.Content == "" && len(res.ToolCalls) == 0) {
		return nil
	}
	raw, _ := json.Marshal(res.Content)
	msg := &ChatMessage{Role: "assistant", Content: raw}
	if len(res.ToolCalls) > 0 {
		msg.ToolCalls, _ = json.Marshal(res.ToolCalls)
	}
	return msg
}

// RememberConversation 在会话成功后记录会话映射（请求历史 + 请求+助手回复），
// 供 LookupConversation / StickyAccount 复用。
func (p *Provider) RememberConversation(accountID int64, messages []ChatMessage, res *Result) {
	if res == nil || res.ConversationID == "" || len(messages) == 0 {
		return
	}
	if hasReusableHistory(messages) {
		p.setConversationID(accountID, messages, res.ConversationID)
	}
	if asst := assistantFollowup(res); asst != nil {
		next := append(append([]ChatMessage{}, messages...), *asst)
		p.setConversationID(accountID, next, res.ConversationID)
	}
}

func (p *Provider) ResetConversation(accountID int64) {
	p.convStore.Reset(accountID)
}

// ---------- 消息提取 ----------

func (p *Provider) rememberToolGroups(accountID int64, calls []ToolCall) {
	for _, call := range calls {
		if call.ID != "" && call.GroupID != "" {
			p.convStore.PutToolGroup(accountID, call.ID, call.GroupID)
		}
	}
}

func (p *Provider) lookupToolGroup(accountID int64, toolCallID string) string {
	if g, ok := p.convStore.GetToolGroup(accountID, toolCallID); ok {
		return g
	}
	return ""
}

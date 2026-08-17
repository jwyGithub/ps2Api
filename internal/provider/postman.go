package provider

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ps2api/internal/store"
)

const (
	DesktopAppVersion  = "12.23.1"
	DesktopToolsHash   = "clienttools-workspace_localmode_v12-desktop-win32-12.23.1-ui-260811-0231-828d6b3ed37b"
	DesktopKBTermsHash = "kbterms-workspace_localmode_v12-desktop-win32-12.23.1-ui-260811-0231-828d6b3ed37b"
	DesktopChatURL     = "https://gateway.postman.com/chat"

	WebAppVersion  = "12.15.4-260616-1202"
	WebToolsHash   = "clienttools-workspace_v12-browser-12.15.4-260616-1202-d5808662718f"
	WebKBTermsHash = "kbterms-workspace_v12-browser-12.15.4-260616-1202-4755650f241c"

	RequestTimeout = 300 * time.Second
	MaxQueryLen    = 9500
)

// Tokens 兼容桌面（access_token）和 web（postman.sid）两种登录态。
type Tokens struct {
	AccessToken        string `json:"access_token,omitempty"`
	PostmanSID         string `json:"postman_sid,omitempty"`
	UserID             string `json:"user_id"`
	WorkspaceID        string `json:"workspace_id"`
	WorkspaceSubdomain string `json:"workspace_subdomain,omitempty"`
	UserName           string `json:"user_name,omitempty"`
}

func (t *Tokens) IsDesktop() bool { return t.AccessToken != "" }

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string 或 content blocks 数组
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ChatRequest struct {
	Model             string          `json:"model"`
	Messages          []ChatMessage   `json:"messages"`
	Stream            bool            `json:"stream,omitempty"`
	Tools             []interface{}   `json:"tools,omitempty"`
	ToolChoice        interface{}     `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	Raw               json.RawMessage `json:"-"`
	// Endpoint 调用来源兼容端点：anthropic(/v1/messages) | openai(/v1/chat/completions)，
	// 由 HTTP handler 注入，只进日志，不随请求体透传。
	Endpoint string `json:"-"`
}

type Provider struct {
	Client     *http.Client
	convMap    sync.Map // accountID:fingerprint -> conversationID
	convOwn    sync.Map // fingerprint -> accountID(int64) 会话归属账号（粘性路由用）
	toolGroups sync.Map // accountID:toolCallID -> Postman toolCallGroupId
}

func New() *Provider {
	return &Provider{Client: &http.Client{Timeout: 0}} // 用 ctx 控制超时，流式不能有总超时
}

func (p *Provider) GetTokens(acc *store.Account) (*Tokens, error) {
	if acc.Tokens == "" {
		return nil, fmt.Errorf("no tokens")
	}
	var t Tokens
	if err := json.Unmarshal([]byte(acc.Tokens), &t); err != nil {
		return nil, fmt.Errorf("bad tokens json: %w", err)
	}
	if t.WorkspaceID == "" || t.UserID == "" {
		return nil, fmt.Errorf("tokens missing user_id/workspace_id")
	}
	if t.AccessToken == "" && t.PostmanSID == "" {
		return nil, fmt.Errorf("tokens missing access_token or postman_sid")
	}
	return &t, nil
}

func convKey(accountID int64, fp string) string {
	return strconv.FormatInt(accountID, 10) + ":" + fp
}

func conversationFingerprint(messages []ChatMessage) string {
	h := sha256.New()
	for _, m := range messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(ExtractText(m.Content)))
		h.Write([]byte{0})
		h.Write([]byte(m.ToolCallID))
		h.Write([]byte{0})
		h.Write([]byte(toolCallFingerprint(m)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
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

// LookupConversation 按消息历史前缀匹配已有 Postman 会话。
// 新对话（没有 assistant/tool）一律开新会话，避免不同 agent 串上下文。
func (p *Provider) LookupConversation(accountID int64, messages []ChatMessage) string {
	if !hasReusableHistory(messages) {
		return ""
	}
	for i := len(messages) - 1; i >= 1; i-- {
		if v, ok := p.convMap.Load(convKey(accountID, conversationFingerprint(messages[:i]))); ok {
			return v.(string)
		}
	}
	return ""
}

func (p *Provider) setConversationID(accountID int64, messages []ChatMessage, id string) {
	if id == "" || len(messages) == 0 {
		return
	}
	fp := conversationFingerprint(messages)
	p.convMap.Store(convKey(accountID, fp), id)
	p.convOwn.Store(fp, accountID)
}

// StickyAccount 返回该消息历史（会话）首次使用的账号，供路由层做会话粘性：
// 续聊请求固定回原账号，避免池子轮询换号导致上下文丢失。
// 新对话（无 assistant/tool 历史）返回 false。
func (p *Provider) StickyAccount(messages []ChatMessage) (int64, bool) {
	if !hasReusableHistory(messages) {
		return 0, false
	}
	for i := len(messages) - 1; i >= 1; i-- {
		if v, ok := p.convOwn.Load(conversationFingerprint(messages[:i])); ok {
			return v.(int64), true
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
	prefix := strconv.FormatInt(accountID, 10) + ":"
	p.convMap.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok && strings.HasPrefix(s, prefix) {
			p.convMap.Delete(k)
		}
		return true
	})
	p.convOwn.Range(func(k, v any) bool {
		if id, ok := v.(int64); ok && id == accountID {
			p.convOwn.Delete(k)
		}
		return true
	})
}

// ---------- 消息提取 ----------

func ExtractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var out []string
	for _, part := range parts {
		typ, _ := part["type"].(string)
		switch typ {
		case "text":
			if t, ok := part["text"].(string); ok {
				out = append(out, t)
			}
		case "tool_result":
			toolID, _ := part["tool_use_id"].(string)
			out = append(out, fmt.Sprintf("<tool_result id=%q>\n%s\n</tool_result>", toolID, toolResultText(part["content"])))
		case "image_url", "image", "input_image":
			out = append(out, "[image attachment]")
		default:
			if t, ok := part["text"].(string); ok {
				out = append(out, t)
			}
		}
	}
	return strings.Join(out, "\n")
}

func toolResultText(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		var out []string
		for _, b := range c {
			if m, ok := b.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					out = append(out, t)
					continue
				}
				typ, _ := m["type"].(string)
				switch typ {
				case "image", "image_url", "input_image":
					out = append(out, "[image attachment]")
				case "document":
					out = append(out, "[document attachment]")
				default:
					if raw, err := json.Marshal(m); err == nil {
						out = append(out, string(raw))
					}
				}
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

func isAnthropicToolResult(msg ChatMessage) bool {
	if msg.Role != "user" || len(msg.Content) == 0 {
		return false
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		return false
	}
	for _, p := range parts {
		if p["type"] == "tool_result" {
			return true
		}
	}
	return false
}

// UnsupportedToolResult returns the tool name when the caller reports that it
// cannot execute a custom tool. Replaying this history only makes the model
// emit the same tool call again, so the API layer can stop the loop early.
func UnsupportedToolResult(messages []ChatMessage) (string, bool) {
	for _, msg := range messages {
		if msg.Role == "tool" {
			if name := unsupportedToolName(ExtractText(msg.Content)); name != "" {
				return name, true
			}
			continue
		}
		if !isAnthropicToolResult(msg) {
			continue
		}
		var blocks []map[string]interface{}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			if block["type"] != "tool_result" {
				continue
			}
			failed, _ := block["is_error"].(bool)
			if !failed {
				continue
			}
			if name := unsupportedToolName(toolResultText(block["content"])); name != "" {
				return name, true
			}
		}
	}
	return "", false
}

func unsupportedToolName(content string) string {
	const marker = "unsupported custom tool call:"
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(strings.ToLower(content), marker) {
		return ""
	}
	name := strings.TrimSpace(content[len(marker):])
	if fields := strings.Fields(name); len(fields) > 0 {
		return strings.Trim(fields[0], "`\"'")
	}
	return "unknown"
}

// ---------- 请求构造 ----------

func (p *Provider) buildThirdPartyTools(tools []interface{}) map[string]interface{} {
	if len(tools) == 0 {
		return map[string]interface{}{}
	}
	var mcpTools []map[string]interface{}
	for _, tool := range tools {
		name := extractToolName(tool)
		if name == "" {
			continue
		}
		desc := extractToolDesc(tool)
		if desc == "" {
			desc = name
		}
		params := extractToolSchema(tool)
		mcpTools = append(mcpTools, map[string]interface{}{
			"name": name, "description": desc, "parameters": params,
		})
	}
	if len(mcpTools) == 0 {
		return map[string]interface{}{}
	}
	return map[string]interface{}{"proxy-tools": map[string]interface{}{"tools": mcpTools}}
}

type splitResult struct {
	Query           string
	SeedingMessages []map[string]string
}

func toolTail(messages []ChatMessage) bool {
	return toolTailIndex(messages) >= 0
}

// toolTailIndex finds the last tool result while ignoring token accounting
// system messages appended by Anthropic-compatible clients.
func toolTailIndex(messages []ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if isTrailingTokenMetadata(messages[i]) {
			continue
		}
		if messages[i].Role == "tool" || isAnthropicToolResult(messages[i]) {
			return i
		}
		return -1
	}
	return -1
}

func isTrailingTokenMetadata(msg ChatMessage) bool {
	if msg.Role != "system" {
		return false
	}
	text := ExtractText(msg.Content)
	return strings.Contains(text, "<total_tokens>")
}

func formatAssistantToolCalls(raw json.RawMessage) string {
	var calls []ToolCall
	if len(raw) == 0 || json.Unmarshal(raw, &calls) != nil {
		return ""
	}
	var out []string
	for _, call := range calls {
		out = append(out, fmt.Sprintf("[Assistant Tool Call id=%s name=%s]\n%s", call.ID, call.Function.Name, call.Function.Arguments))
	}
	return strings.Join(out, "\n\n")
}

func (p *Provider) splitMessages(messages []ChatMessage, convID string) splitResult {
	toolIdx := toolTailIndex(messages)
	isToolTail := toolIdx >= 0
	hasConv := convID != ""

	var query string
	queryIdx := -1
	skipFrom := len(messages)

	if isToolTail {
		var parts []string
		for i := toolIdx; i >= 0; i-- {
			msg := messages[i]
			if msg.Role == "tool" {
				skipFrom = i
				parts = append([]string{fmt.Sprintf("[Tool Result id=%s]\n%s", msg.ToolCallID, ExtractText(msg.Content))}, parts...)
				continue
			}
			if isAnthropicToolResult(msg) {
				skipFrom = i
				var blocks []map[string]interface{}
				if json.Unmarshal(msg.Content, &blocks) == nil {
					for _, b := range blocks {
						if b["type"] == "tool_result" {
							id, _ := b["tool_use_id"].(string)
							label := "Tool Result"
							if failed, _ := b["is_error"].(bool); failed {
								label = "Tool Error"
							}
							parts = append([]string{fmt.Sprintf("[%s id=%s]\n%s", label, id, toolResultText(b["content"]))}, parts...)
						} else if b["type"] == "text" {
							if text, _ := b["text"].(string); text != "" {
								parts = append(parts, "[User Message]\n"+text)
							}
						}
					}
				}
				continue
			}
			break
		}
		block := strings.Join(parts, "\n\n")
		instruction := "\n\nProcess these tool results and continue. If you need another tool, emit <tool_call> markup; otherwise answer the user."
		budget := MaxQueryLen - len(instruction)
		if len(block) > budget {
			head := 256
			block = block[:head] + "\n...[tool result truncated]...\n" + block[len(block)-(budget-head-32):]
		}
		query = block + instruction
	} else {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				queryIdx = i
				break
			}
		}
		raw := ""
		if queryIdx >= 0 {
			raw = ExtractText(messages[queryIdx].Content)
		}
		if len(raw) > MaxQueryLen {
			raw = raw[len(raw)-MaxQueryLen:]
		}
		query = raw
	}

	if hasConv {
		return splitResult{Query: query}
	}

	// 首轮：把历史塞进 seedingMessages
	var contextParts []string
	for i, msg := range messages {
		if i == queryIdx || i >= skipFrom {
			continue
		}
		text := ExtractText(msg.Content)
		switch msg.Role {
		case "system":
			if text != "" {
				contextParts = append(contextParts, "[System]\n"+text)
			}
		case "user":
			if text != "" {
				contextParts = append(contextParts, "[User]\n"+text)
			}
		case "assistant":
			block := "[Assistant]"
			if text != "" {
				block = "[Assistant]\n" + text
			}
			if calls := formatAssistantToolCalls(msg.ToolCalls); calls != "" {
				block += "\n\n" + calls
			}
			contextParts = append(contextParts, block)
		case "tool":
			contextParts = append(contextParts, fmt.Sprintf("Tool result for id=%s:\n%s", msg.ToolCallID, text))
		}
	}
	context := strings.Join(contextParts, "\n\n")
	if context == "" {
		return splitResult{Query: query}
	}
	return splitResult{
		Query: query,
		SeedingMessages: []map[string]string{
			{"role": "user", "content": context},
			{"role": "assistant", "content": "I have the full conversation history above and will continue from where we left off."},
		},
	}
}

var desktopExcludedTools = []string{
	"listDatasets", "createDataset", "previewDataset", "queryDatasetView", "deleteDataset",
	"getDatasetSchema", "createDatasetView", "deleteDatasetView", "runQuery", "insertDatasetRows",
	"modifyDatasetView", "refreshDatasource", "addDatasetSource", "editDatasetSource",
	"removeDatasetSource", "testDatasourceConnection", "listCloudMocks", "getCloudMock",
	"getCloudMockLogs", "renameCloudMock", "deleteCloudMock", "checkMockSlugAvailability",
	"createCloudMock", "listWorkspaceDocs", "getWorkspaceDoc", "createWorkspaceDoc",
	"updateWorkspaceDoc", "deleteWorkspaceDoc", "askUser",
}

type nativeToolResponse struct {
	conversationID string
	groupID        string
	responses      []map[string]interface{}
}

func (p *Provider) nativeToolResponse(accountID int64, messages []ChatMessage) (nativeToolResponse, bool) {
	toolIdx := toolTailIndex(messages)
	if toolIdx < 0 {
		return nativeToolResponse{}, false
	}
	response := nativeToolResponse{conversationID: p.LookupConversation(accountID, messages)}
	if response.conversationID == "" {
		return nativeToolResponse{}, false
	}
	add := func(toolCallID, content string, failed bool) bool {
		groupID := p.lookupToolGroup(accountID, toolCallID)
		if groupID == "" {
			return false
		}
		if response.groupID == "" {
			response.groupID = groupID
		} else if response.groupID != groupID {
			return false
		}
		status := "SUCCESS"
		if failed {
			status = "FAILED"
		}
		payload := strings.TrimSpace(content)
		if !json.Valid([]byte(payload)) {
			encoded, _ := json.Marshal(map[string]string{"status": status, "message": content})
			payload = string(encoded)
		}
		summary := content
		if len(summary) > 512 {
			summary = summary[:512] + "..."
		}
		entry := map[string]interface{}{
			"toolCallId":          toolCallID,
			"content":             payload,
			"toolResponseSummary": summary,
			"toolResponseStatus":  status,
		}
		if failed {
			entry["toolResponseFailureType"] = "UNHANDLED_ERROR"
		}
		response.responses = append([]map[string]interface{}{entry}, response.responses...)
		return true
	}
	for i := toolIdx; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "tool" {
			content := ExtractText(msg.Content)
			failed := strings.Contains(content, "<tool_use_error>")
			if !failed {
				var result struct {
					Status  string `json:"status"`
					IsError bool   `json:"is_error"`
				}
				if json.Unmarshal([]byte(content), &result) == nil {
					failed = result.IsError || strings.EqualFold(result.Status, "FAILED") || strings.EqualFold(result.Status, "ERROR")
				}
			}
			if !add(msg.ToolCallID, content, failed) {
				return nativeToolResponse{}, false
			}
			continue
		}
		if !isAnthropicToolResult(msg) {
			break
		}
		var blocks []map[string]interface{}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			return nativeToolResponse{}, false
		}
		for i := len(blocks) - 1; i >= 0; i-- {
			block := blocks[i]
			if block["type"] != "tool_result" {
				continue
			}
			toolCallID, _ := block["tool_use_id"].(string)
			failed, _ := block["is_error"].(bool)
			if toolCallID == "" || !add(toolCallID, toolResultText(block["content"]), failed) {
				return nativeToolResponse{}, false
			}
		}
	}
	return response, len(response.responses) > 0 && response.groupID != ""
}

func (p *Provider) buildBody(req *ChatRequest, tokens *Tokens, postmanModel string, accountID int64) map[string]interface{} {
	nativeResponse, useNativeResponse := p.nativeToolResponse(accountID, req.Messages)
	convID := p.LookupConversation(accountID, req.Messages)
	// Native tool responses must keep the pending Postman conversation. If the
	// group ID is unavailable (for example after a process restart), replay the
	// history instead of sending a tool result as USER_QUERY.
	if useNativeResponse {
		convID = nativeResponse.conversationID
	} else if toolTail(req.Messages) {
		convID = ""
	}
	split := p.splitMessages(req.Messages, convID)
	tools, toolInstruction := selectedTools(req.Tools, req.ToolChoice)
	if len(tools) > 0 {
		if toolInstruction != "" {
			split.Query = toolInstruction + "\n\n" + split.Query
		}
	}
	thirdParty := p.buildThirdPartyTools(tools)

	input := map[string]interface{}{
		"chatType":     "USER_QUERY",
		"query":        split.Query,
		"toolResponse": "",
		"useCase":      nil,
		"agent":        nil,
	}
	if convID != "" {
		input["conversationId"] = convID
	} else {
		input["conversationId"] = nil
	}
	if convID == "" && split.SeedingMessages != nil {
		input["seedingMessages"] = split.SeedingMessages
	}

	var body map[string]interface{}
	if tokens.IsDesktop() {
		input["product"] = "workspace_localmode_v12"
		body = map[string]interface{}{
			"input":    input,
			"platform": "DESKTOP_WINDOWS",
			"clientTools": map[string]interface{}{
				"nativeToolsHash": DesktopToolsHash,
				"excludedTools":   []string{},
				"thirdParty":      thirdParty,
			},
			"clientKBTerms": map[string]interface{}{
				"nativeTermsHash": DesktopKBTermsHash,
				"excludedKBTerms": []string{"DATASETS"},
			},
			"mandatoryContext": map[string]interface{}{"workspaceId": tokens.WorkspaceID},
			"selectedContext":  []interface{}{},
			"backgroundContext": []interface{}{
				map[string]interface{}{"type": "ACTIVE_ENVIRONMENT", "value": nil},
				map[string]interface{}{"type": "VARIABLES_IN_SCOPE", "value": []interface{}{}},
				map[string]interface{}{"type": "COLLECTION_LIST", "value": []interface{}{}},
			},
			"availableSkills": []interface{}{},
		}
	} else {
		input["product"] = "workspace_v12"
		input["startedFrom"] = "CHAT_INPUT"
		body = map[string]interface{}{
			"input":    input,
			"platform": "WEB",
			"clientTools": map[string]interface{}{
				"nativeToolsHash": WebToolsHash,
				"excludedTools":   desktopExcludedTools,
				"thirdParty":      thirdParty,
			},
			"clientKBTerms": map[string]interface{}{
				"nativeTermsHash": WebKBTermsHash,
				"excludedKBTerms": []string{"DATASETS"},
			},
			"mandatoryContext":  map[string]interface{}{"workspaceId": tokens.WorkspaceID},
			"selectedContext":   []interface{}{},
			"backgroundContext": []interface{}{},
			"availableSkills":   []interface{}{},
		}
	}

	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}
	devMode := map[string]interface{}{
		"selectedModel":                  postmanModel,
		"isParallelToolCallingSupported": parallel,
		"autoRun":                        len(thirdParty) > 0,
		"supportsAskUser":                false,
		"supportsActionRecommendations":  true,
		"useThinkingModeIfAvailable":     true,
		"thinkingLevel":                  "medium",
	}
	body["devModeOptions"] = devMode
	if useNativeResponse {
		input["chatType"] = "TOOL_RESPONSE"
		input["query"] = ""
		input["conversationId"] = nativeResponse.conversationID
		input["toolCallGroupId"] = nativeResponse.groupID
		input["toolResponses"] = nativeResponse.responses
		delete(input, "toolResponse")
		delete(input, "agent")
		delete(input, "seedingMessages")
	}

	return body
}

func (p *Provider) buildHeaders(tokens *Tokens) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("x-pstmn-req-service", "agent-mode-service")
	if tokens.IsDesktop() {
		h.Set("x-access-token", tokens.AccessToken)
		h.Set("x-app-version", DesktopAppVersion)
		h.Set("User-Agent", "PostmanDesktop/"+DesktopAppVersion)
	} else {
		h.Set("Cookie", "postman.sid="+tokens.PostmanSID)
		h.Set("x-app-version", WebAppVersion)
		h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
		h.Set("Origin", "https://"+tokens.WorkspaceSubdomain+".postman.co")
		h.Set("Referer", "https://"+tokens.WorkspaceSubdomain+".postman.co/")
	}
	return h
}

func (p *Provider) chatURL(tokens *Tokens) string {
	if tokens.IsDesktop() {
		return DesktopChatURL
	}
	return "https://" + tokens.WorkspaceSubdomain + ".postman.co/_gw/chat"
}

// ---------- 结果类型 ----------

type Result struct {
	Success          bool
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	ActualModel      string
	ConversationID   string
	Usage            *Usage
	RateLimit        *RateLimit
	PromptTokens     int
	CompletionTokens int
	Error            string
	RateLimited      bool
	QuotaExhausted   bool
	AuthFailed       bool
}

func parseRateLimit(headers http.Header, now time.Time) *RateLimit {
	limit, _ := strconv.Atoi(strings.TrimSpace(headers.Get("X-RateLimit-Limit")))
	remaining, _ := strconv.Atoi(strings.TrimSpace(headers.Get("X-RateLimit-Remaining")))
	rate := &RateLimit{Limit: limit, Remaining: remaining}
	for _, part := range strings.Split(headers.Get("RateLimit-Policy"), ";") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(part), "w="); ok {
			rate.WindowSeconds, _ = strconv.Atoi(value)
		}
	}
	if value, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Reset")), 10, 64); err == nil && value > 0 {
		var reset time.Time
		switch {
		case value >= 1_000_000_000_000:
			reset = time.UnixMilli(value)
		case value >= 1_000_000_000:
			reset = time.Unix(value, 0)
		default:
			reset = now.Add(time.Duration(value) * time.Second)
		}
		rate.ResetAt = &reset
	}
	if rate.Limit == 0 && rate.Remaining == 0 && rate.WindowSeconds == 0 && rate.ResetAt == nil {
		return nil
	}
	return rate
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	GroupID  string `json:"-"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func toolGroupKey(accountID int64, toolCallID string) string {
	return strconv.FormatInt(accountID, 10) + ":" + toolCallID
}

func (p *Provider) rememberToolGroups(accountID int64, calls []ToolCall) {
	for _, call := range calls {
		if call.ID != "" && call.GroupID != "" {
			p.toolGroups.Store(toolGroupKey(accountID, call.ID), call.GroupID)
		}
	}
}

func (p *Provider) lookupToolGroup(accountID int64, toolCallID string) string {
	if value, ok := p.toolGroups.Load(toolGroupKey(accountID, toolCallID)); ok {
		return value.(string)
	}
	return ""
}

// EmitFunc 流式增量回调。
type EmitFunc func(d Delta) error

// Chat 非流式调用。
func (p *Provider) Chat(ctx context.Context, acc *store.Account, req *ChatRequest) *Result {
	res := &Result{}
	defer func() { Trace(ctx, "provider.result", res) }()
	postmanModel, ok := ResolvePostmanModel(req.Model)
	if !ok {
		res.Error = "Invalid model: " + req.Model
		return res
	}
	tokens, err := p.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res
	}

	var content, reasoning strings.Builder
	toolAcc := map[int]*ToolCall{}
	tools, _ := selectedTools(req.Tools, req.ToolChoice)
	err = p.streamInternal(ctx, acc, req, tokens, postmanModel, func(d Delta) error {
		content.WriteString(d.Content)
		reasoning.WriteString(d.ReasoningContent)
		for _, tc := range d.ToolCalls {
			entry, ok := toolAcc[tc.Index]
			if !ok && tc.ID != "" {
				entry = &ToolCall{ID: tc.ID, Type: "function"}
				toolAcc[tc.Index] = entry
			}
			if entry != nil && tc.Function != nil {
				if tc.GroupID != "" {
					entry.GroupID = tc.GroupID
				}
				if tc.Function.Name != "" {
					entry.Function.Name = tc.Function.Name
				}
				entry.Function.Arguments = appendToolArguments(entry.Function.Arguments, tc.Function.Arguments)
			}
		}
		return nil
	}, res)
	if err != nil {
		return res
	}
	res.Success = true
	res.Content = content.String()
	res.ReasoningContent = reasoning.String()
	res.ToolCalls = collectToolCalls(toolAcc)
	p.rememberToolGroups(acc.ID, res.ToolCalls)
	res.Content = applySimulatedTools(res, res.Content, tools)
	res.PromptTokens = EstimateMessagesTokens(req.Messages)
	res.CompletionTokens = EstimateTokens(res.Content + res.ReasoningContent)
	p.RememberConversation(acc.ID, req.Messages, res)
	return res
}

// collectToolCalls 按 index 升序聚合工具调用，避免上游索引跳号导致调用丢失。
func collectToolCalls(toolAcc map[int]*ToolCall) []ToolCall {
	idx := make([]int, 0, len(toolAcc))
	for i := range toolAcc {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		if tc, ok := toolAcc[i]; ok {
			out = append(out, *tc)
		}
	}
	return out
}

func appendToolArguments(current, next string) string {
	if current == "" {
		return next
	}
	if next == "" {
		return current
	}
	combined := current + next
	if json.Valid([]byte(combined)) {
		return combined
	}
	var left, right map[string]interface{}
	if json.Unmarshal([]byte(current), &left) == nil && json.Unmarshal([]byte(next), &right) == nil {
		for key, value := range right {
			left[key] = value
		}
		if b, err := json.Marshal(left); err == nil {
			return string(b)
		}
	}
	return combined
}

// StreamChat 流式调用：上游每个增量通过 emit 回调吐给调用方。
func (p *Provider) StreamChat(ctx context.Context, acc *store.Account, req *ChatRequest, emit EmitFunc) *Result {
	res := &Result{}
	defer func() { Trace(ctx, "provider.result", res) }()
	postmanModel, ok := ResolvePostmanModel(req.Model)
	if !ok {
		res.Error = "Invalid model: " + req.Model
		return res
	}
	tokens, err := p.GetTokens(acc)
	if err != nil {
		res.Error = err.Error()
		res.AuthFailed = true
		return res
	}
	tools, _ := selectedTools(req.Tools, req.ToolChoice)
	if len(tools) == 0 {
		var captured strings.Builder
		p.streamInternal(ctx, acc, req, tokens, postmanModel, func(d Delta) error {
			captured.WriteString(d.Content)
			return emit(d)
		}, res)
		res.Content = captured.String()
		res.PromptTokens = EstimateMessagesTokens(req.Messages)
		p.RememberConversation(acc.ID, req.Messages, res)
		return res
	}
	var content strings.Builder
	sawNativeTools := false
	toolAcc := map[int]*ToolCall{}
	flushText := func() error {
		if content.Len() == 0 {
			return nil
		}
		err := emit(Delta{Content: content.String()})
		content.Reset()
		return err
	}
	wrapped := func(d Delta) error {
		if d.HasFinish {
			return nil
		}
		if d.ReasoningContent != "" {
			if err := emit(Delta{ReasoningContent: d.ReasoningContent}); err != nil {
				return err
			}
		}
		if len(d.ToolCalls) > 0 {
			if err := flushText(); err != nil {
				return err
			}
			sawNativeTools = true
			for _, tc := range d.ToolCalls {
				entry, ok := toolAcc[tc.Index]
				if !ok && tc.ID != "" {
					entry = &ToolCall{ID: tc.ID, Type: "function"}
					toolAcc[tc.Index] = entry
				}
				if entry != nil && tc.Function != nil {
					if tc.GroupID != "" {
						entry.GroupID = tc.GroupID
					}
					if tc.Function.Name != "" {
						entry.Function.Name = tc.Function.Name
					}
					entry.Function.Arguments = appendToolArguments(entry.Function.Arguments, tc.Function.Arguments)
				}
			}
			return nil
		}
		if d.Content != "" {
			if sawNativeTools {
				return emit(d)
			}
			content.WriteString(d.Content)
		}
		return nil
	}
	p.streamInternal(ctx, acc, req, tokens, postmanModel, wrapped, res)
	if !res.Success {
		_ = flushText()
		res.PromptTokens = EstimateMessagesTokens(req.Messages)
		p.RememberConversation(acc.ID, req.Messages, res)
		return res
	}
	if sawNativeTools {
		_ = flushText()
		res.ToolCalls = collectToolCalls(toolAcc)
		p.rememberToolGroups(acc.ID, res.ToolCalls)
		for i, tc := range res.ToolCalls {
			_ = emit(Delta{ToolCalls: []DeltaToolCall{{
				Index: i,
				ID:    tc.ID,
				Type:  tc.Type,
				Function: &struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			}}})
		}
		_ = emit(Delta{FinishReason: "tool_calls", HasFinish: true})
		res.PromptTokens = EstimateMessagesTokens(req.Messages)
		p.RememberConversation(acc.ID, req.Messages, res)
		return res
	}
	cleaned, sim := simulatedDeltas(content.String(), tools)
	content.Reset()
	if cleaned != "" {
		_ = emit(Delta{Content: cleaned})
	}
	res.Content = cleaned
	if len(sim) == 0 {
		_ = emit(Delta{FinishReason: "stop", HasFinish: true})
		res.PromptTokens = EstimateMessagesTokens(req.Messages)
		p.RememberConversation(acc.ID, req.Messages, res)
		return res
	}
	for _, d := range sim {
		_ = emit(d)
		for _, tc := range d.ToolCalls {
			entry := ToolCall{ID: tc.ID, Type: "function"}
			if tc.Function != nil {
				entry.Function.Name = tc.Function.Name
				entry.Function.Arguments = tc.Function.Arguments
			}
			res.ToolCalls = append(res.ToolCalls, entry)
		}
	}
	_ = emit(Delta{FinishReason: "tool_calls", HasFinish: true})
	res.PromptTokens = EstimateMessagesTokens(req.Messages)
	p.RememberConversation(acc.ID, req.Messages, res)
	return res
}

func (p *Provider) streamInternal(ctx context.Context, acc *store.Account, req *ChatRequest, tokens *Tokens, postmanModel string, emit EmitFunc, res *Result) error {
	started := time.Now()
	defer func() {
		Trace(ctx, "upstream.complete", map[string]interface{}{
			"account_id": acc.ID, "duration_ms": time.Since(started).Milliseconds(),
			"success": res.Success, "error": res.Error, "conversation_id": res.ConversationID,
		})
	}()
	body := p.buildBody(req, tokens, postmanModel, acc.ID)
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		res.Error = err.Error()
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(tokens), strings.NewReader(string(bodyBytes)))
	if err != nil {
		res.Error = err.Error()
		return err
	}
	httpReq.Header = p.buildHeaders(tokens)
	Trace(ctx, "upstream.request", map[string]interface{}{
		"method": httpReq.Method, "url": httpReq.URL.String(), "headers": httpReq.Header,
		"body": json.RawMessage(bodyBytes), "account_id": acc.ID,
	})

	resp, err := p.Client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			res.Error = "Upstream timeout"
		} else if strings.Contains(err.Error(), "Client disconnected") || ctx.Err() == context.Canceled {
			res.Error = "Client disconnected"
		} else {
			res.Error = "Postman request failed: " + err.Error()
		}
		return err
	}
	defer resp.Body.Close()
	Trace(ctx, "upstream.response.headers", map[string]interface{}{
		"status": resp.StatusCode, "headers": resp.Header, "account_id": acc.ID,
	})
	res.RateLimit = parseRateLimit(resp.Header, time.Now())

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		res.Error = fmt.Sprintf("Postman auth failed (%d)", resp.StatusCode)
		res.AuthFailed = true
		return fmt.Errorf("%s", res.Error)
	}
	if resp.StatusCode == 429 {
		res.Error = "Postman rate limited"
		res.RateLimited = true
		return fmt.Errorf("%s", res.Error)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		Trace(ctx, "upstream.response.body", map[string]interface{}{"body": string(b), "account_id": acc.ID})
		res.Error = fmt.Sprintf("Postman API error (%d): %s", resp.StatusCode, string(b))
		return fmt.Errorf("%s", res.Error)
	}

	reader := NewStreamReader()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		Trace(ctx, "upstream.response.sse", map[string]interface{}{"line": line, "account_id": acc.ID})
		for _, d := range reader.Feed(line) {
			if err := emit(d); err != nil {
				res.Error = "Client disconnected"
				return err
			}
		}
		if reader.QuotaExceeded {
			break
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		res.Error = "stream read error: " + err.Error()
		return err
	}

	if reader.QuotaExceeded {
		res.Error = "Postman AI quota exceeded"
		res.QuotaExhausted = true
		res.Usage = reader.Usage
		return fmt.Errorf("%s", res.Error)
	}
	if reader.Err != "" {
		res.Error = reader.Err
		res.Usage = reader.Usage
		return fmt.Errorf("%s", res.Error)
	}

	if reader.ConversationID != "" {
		res.ConversationID = reader.ConversationID
	}
	res.ActualModel = reader.ActualModel
	res.Usage = reader.Usage

	for _, d := range reader.Finish() {
		if err := emit(d); err != nil {
			res.Error = "Client disconnected"
			return err
		}
	}
	res.Success = true
	return nil
}

// ---------- token 估算 ----------

func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	n := (len(text) + 3) / 4
	if n < 1 {
		return 1
	}
	return n
}

func EstimateMessagesTokens(messages []ChatMessage) int {
	total := 0
	for _, m := range messages {
		total += EstimateTokens(ExtractText(m.Content)) + 4
	}
	return total
}

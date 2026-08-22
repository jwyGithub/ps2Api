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
	// Desktop* 取自真实 macOS 桌面端「本地模式(localmode)」会话抓包(native/openai/openai.chls)——
	// 只有 localmode workspace 才暴露 executeShellCommand/readFile/listDirectory/searchInFiles
	// 这些本地工具。hash 是工具目录快照的指纹,随桌面构建版本漂移;换新版桌面端重新抓包对齐即可。
	DesktopAppVersion  = "12.23.7"
	DesktopToolsHash   = "clienttools-workspace_localmode_v12-desktop-darwin-12.23.7-ui-260814-0232-a0d1149cc7c7"
	DesktopKBTermsHash = "kbterms-workspace_localmode_v12-desktop-darwin-12.23.7-ui-260814-0232-2ebdcef5a027"
	DesktopChatURL     = "https://gateway.postman.com/chat"

	// Web* 取自真实浏览器 Web 会话抓包(native/claude/web-chat-1.txt，已知正常、未触发 403)。
	// product/toolsHash/termsHash 必须是同一个 workspace_v12 三元组，与真实浏览器逐字段一致——
	// 三者错配会导致服务端路由异常。换新版 Web 构建重新抓包对齐即可。
	WebAppVersion  = "12.24.0-260817-0232"
	WebToolsHash   = "clienttools-workspace_v12-browser-12.24.0-260817-0232-e18c30182b36"
	WebKBTermsHash = "kbterms-workspace_v12-browser-12.24.0-260817-0232-02e13a7c1aeb"
	WebProduct     = "workspace_v12"

	RequestTimeout = 300 * time.Second
	MaxQueryLen    = 9500
	MaxToolDescLen = 512

	// MaxToolResponseContentLen 是 TOOL_RESPONSE 续期时单条 toolResponses[].content 的上限。
	// 该字段此前不设限，续期时容易把出站 body 顶过 ~80KB 的 Cloudflare WAF 信封而触发 403
	// （精确解释了「只有带工具续期才 403、单条新消息不 403」的现象）。原生客户端本身也会裁剪
	// tool result，这里对齐该行为，保留头尾、中段截断。
	MaxToolResponseContentLen = 16 * 1024

	// MaxRequestBodyWarnBytes 是出站请求体的软告警阈值。超过此值时记录告警，
	// 因为过大的 body 更容易触发 Postman 网关侧的 Cloudflare WAF（返回 403 HTML）。
	// 仅告警、不阻断，避免误伤合法的大请求。
	MaxRequestBodyWarnBytes = 80 * 1024
)

// Tokens 兼容桌面（access_token）和 web（postman.sid）两种登录态。
type Tokens struct {
	AccessToken        string `json:"access_token,omitempty"`
	PostmanSID         string `json:"postman_sid,omitempty"`
	UserID             string `json:"user_id"`
	WorkspaceID        string `json:"workspace_id"`
	WorkspaceUUID      string `json:"workspace_uuid,omitempty"`
	WorkspaceSubdomain string `json:"workspace_subdomain,omitempty"`
	UserName           string `json:"user_name,omitempty"`
}

func (t *Tokens) IsWeb() bool     { return t.PostmanSID != "" && t.WorkspaceSubdomain != "" }
func (t *Tokens) IsDesktop() bool { return t.AccessToken != "" && !t.IsWeb() }

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
	// EgressAttempt 是本次出站的出口重试序号（由 router 每次重试递增）。0 用账号粘性出口，
	// 遇 Cloudflare 403 重试时 +1 切到下一个代理出口 IP；越过所有出口后回退本机直连。
	EgressAttempt int `json:"-"`
	// GatewayRetry enables the degraded retry after a gateway block.
	GatewayRetry bool `json:"-"`
	// GatewayRetryRotateEgress 在续聊(有可复用历史)遇网关 403 的「钉账号重试」时置真：
	// 钉住原账号(保住服务端会话)的同时让出口 IP 随 attempt 轮换——403 是有状态的出口信誉
	// 风控，换 IP 大概率通过。为真时 EgressAttempt 随 attempt 递增(而非旧式钉成 attempt-1)。
	GatewayRetryRotateEgress bool `json:"-"`
}

type Provider struct {
	Client     *http.Client
	proxies    *proxyPool
	cookies    *accountCookieJars
	convMap    sync.Map // accountID:fingerprint -> conversationID
	convOwn    sync.Map // fingerprint -> accountID(int64) 会话归属账号（粘性路由用）
	toolGroups sync.Map // accountID:toolCallID -> Postman toolCallGroupId
}

func New() *Provider {
	// Client 为本机直连出口（用 ctx 控制超时，流式不能有总超时）；proxies 为可选出口代理池，
	// 未配置时所有请求走 Client 直连。
	return &Provider{Client: &http.Client{Timeout: 0}, proxies: newProxyPool(), cookies: newAccountCookieJars()}
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
	if t.PostmanSID != "" && t.WorkspaceSubdomain == "" {
		return nil, fmt.Errorf("tokens missing workspace_subdomain for postman.sid")
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

// HasReusableHistory 报告消息里是否含可复用的会话历史（assistant / tool / Anthropic tool_result）。
// router 用它区分两类请求，对网关(Cloudflare 403)拦截采取截然不同的策略：
//   - true（续聊）：Postman 服务端已有该会话，绑定在首次使用的账号上。换号会丢掉服务端会话
//     上下文（请求被降级为 USER_QUERY 且历史被截断到 MaxQueryLen），故遇 403 必须原号退避重试、
//     绝不换号、也不得改写 req.Messages（改写会破坏会话指纹 → 触发静默换号与降级）。
//   - false（新对话）：无服务端会话可丢，遇 403 可安全换号 failover。
func HasReusableHistory(messages []ChatMessage) bool { return hasReusableHistory(messages) }

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
		if name == "" || isClientReservedTool(name) {
			continue
		}
		desc := extractToolDesc(tool)
		if desc == "" {
			desc = name
		} else if len(desc) > MaxToolDescLen {
			desc = strings.ToValidUTF8(desc[:MaxToolDescLen], "")
		}
		params := compactToolSchema(extractToolSchema(tool))
		mcpTools = append(mcpTools, map[string]interface{}{
			"name": name, "description": desc, "parameters": params,
		})
	}
	if len(mcpTools) == 0 {
		return map[string]interface{}{}
	}
	return map[string]interface{}{"proxy-tools": map[string]interface{}{"tools": mcpTools}}
}

// compactThirdPartyTools keeps the callable tool names while dropping the large
// schema/docs envelope for a single gateway retry. The client still owns the
// real schemas and executes the returned tool calls, so names remain necessary.
func compactThirdPartyTools(value map[string]interface{}) map[string]interface{} {
	proxy, ok := value["proxy-tools"].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	tools, ok := proxy["tools"].([]map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}
	compact := make([]map[string]interface{}, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		compact = append(compact, map[string]interface{}{
			"name": name,
			"parameters": map[string]interface{}{
				"type":                 "object",
				"additionalProperties": true,
			},
		})
	}
	if len(compact) == 0 {
		return map[string]interface{}{}
	}
	return map[string]interface{}{"proxy-tools": map[string]interface{}{"tools": compact}}
}

func compactToolSchema(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, child := range v {
			switch key {
			case "description", "title", "examples", "default", "$comment":
				continue
			}
			out[key] = compactToolSchema(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, child := range v {
			out[i] = compactToolSchema(child)
		}
		return out
	default:
		return value
	}
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
		out = append(out, fmt.Sprintf("[Assistant Tool Call id=%s name=%s]", call.ID, call.Function.Name))
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
		if msg.Role == "tool" || isAnthropicToolResult(msg) {
			contextParts = append(contextParts, "[Previous tool result omitted]")
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
		}
	}
	context := strings.Join(contextParts, "\n\n")
	if context == "" {
		return splitResult{Query: query}
	}
	if len(context) > MaxQueryLen {
		const marker = "\n...[conversation context truncated]...\n"
		budget := MaxQueryLen - len(marker)
		head := budget / 2
		context = strings.ToValidUTF8(context[:head], "") + marker +
			strings.ToValidUTF8(context[len(context)-(budget-head):], "")
	}
	return splitResult{
		Query: query,
		SeedingMessages: []map[string]string{
			{"role": "user", "content": context},
			{"role": "assistant", "content": "I have the full conversation history above and will continue from where we left off."},
		},
	}
}

// desktopLocalModeExcludedTools 取自真实 localmode 桌面会话抓包里 clientTools.excludedTools。
// 它只是「隐藏这些工具不给模型」的客户端清单,不影响 executeShellCommand 等本地工具的可用性;
// 原样对齐是为了让网关 desktop 请求与已验证能跑 shell 的抓包一致,减少实测变量。
var desktopLocalModeExcludedTools = []string{
	"listDatasets", "createDataset", "previewDataset", "queryDatasetView", "deleteDataset",
	"getDatasetSchema", "createDatasetView", "deleteDatasetView", "runQuery", "insertDatasetRows",
	"modifyDatasetView", "refreshDatasource", "addDatasetSource", "editDatasetSource",
	"removeDatasetSource", "testDatasourceConnection", "readDatasetRunForScenario",
	"attachDatasetToScenario", "setDatasetInputMapping", "setDatasetInputLiteral",
	"clearDatasetInputMapping", "runFlowWithDataset", "listCloudCodeMocks", "getCloudCodeMock",
	"createCloudCodeMock", "updateCloudCodeMock", "deleteCloudCodeMock", "deployMockServer",
	"listCloudMockServers", "getCloudMockServer", "getMockServerLogs", "getMockServerState",
	"setMockServerEnableSession", "clearMockServerSession", "deleteMockServerStateKey",
	"updateMockServer", "unpublishMockServer", "deleteMockServer", "checkMockServerSlugAvailability",
	"createCloudSimulation", "listCloudSimulations", "getCloudSimulation", "updateCloudSimulation",
	"deleteCloudSimulation", "startCloudSimulation", "stopCloudSimulation", "getCloudSimulationLogs",
	"checkSimulationSlugAvailability", "validateMockForCloud", "publishMockToCloud",
	"runCollectionWithSimulation", "getCollectionRunWithSimulationResults", "configureScenarios",
	"startSimulation", "stopSimulation", "stopSimulationCollectionRun",
	"analyzeCollectionRunWithSimulationResults", "listSimulations", "getSimulationConfig",
	"createSimulationConfig", "updateSimulationConfig", "deleteSimulation", "askUser",
}

// desktopExcludedTools 是旧 api_catalog Web 分支曾用的 excludedTools 清单。Web 分支已切回
// workspace_v12 三元组(excludedTools 对齐真实浏览器的 ["askUser"]),此清单目前未被引用,
// 仅保留作参考/回滚依据。
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
		// 给 content 上限，避免续期时把出站 body 顶过 Cloudflare WAF 信封（触发 403）。
		// 保留头尾、中段截断,并修正可能被切断的 UTF-8。
		if len(payload) > MaxToolResponseContentLen {
			head := 512
			tail := MaxToolResponseContentLen - head - 32
			payload = strings.ToValidUTF8(payload[:head], "") +
				"\n...[tool result truncated]...\n" +
				strings.ToValidUTF8(payload[len(payload)-tail:], "")
		}
		entry := map[string]interface{}{
			"toolCallId":          toolCallID,
			"content":             payload,
			"toolResponseSummary": safeToolResponseSummary(status, content),
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

// safeToolResponseSummary mirrors Postman's short native summaries without copying
// source code, HTML, commands, or other tool output into a second request field.
func safeToolResponseSummary(status, content string) string {
	return fmt.Sprintf("Tool result: %s, %d bytes", status, len(content))
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
	// Keep full third-party registration on normal Web requests. Only the bounded
	// retry after a gateway block uses a name-only schema to preserve custom-tool
	// dispatch without repeating the large docs envelope.
	if req.GatewayRetry {
		thirdParty = compactThirdPartyTools(thirdParty)
	}

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
			"platform": "DESKTOP_MACOS",
			"clientTools": map[string]interface{}{
				"nativeToolsHash": DesktopToolsHash,
				"excludedTools":   desktopLocalModeExcludedTools,
				"thirdParty":      thirdParty,
			},
			"clientKBTerms": map[string]interface{}{
				"nativeTermsHash": DesktopKBTermsHash,
				"excludedKBTerms": []string{"DATASETS"},
			},
			"mandatoryContext": workspaceContext(tokens),
			"selectedContext":  []interface{}{},
			"backgroundContext": []interface{}{
				map[string]interface{}{"type": "ACTIVE_ENVIRONMENT", "value": nil},
				map[string]interface{}{"type": "VARIABLES_IN_SCOPE", "value": []interface{}{}},
				map[string]interface{}{"type": "COLLECTION_LIST", "value": []interface{}{}},
			},
			"availableSkills": []interface{}{},
		}
	} else {
		input["product"] = WebProduct
		body = map[string]interface{}{
			"input":    input,
			"platform": "WEB",
			"clientTools": map[string]interface{}{
				"nativeToolsHash": WebToolsHash,
				"excludedTools":   []string{"askUser"},
				"thirdParty":      thirdParty,
			},
			"clientKBTerms": map[string]interface{}{
				"nativeTermsHash": WebKBTermsHash,
				"excludedKBTerms": []string{},
			},
			"mandatoryContext":  workspaceContext(tokens),
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
	h.Set("accept-language", "en-US,en;q=0.9")
	if tokens.IsDesktop() {
		h.Set("x-access-token", tokens.AccessToken)
		h.Set("x-app-version", DesktopAppVersion)
		h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Postman/"+DesktopAppVersion+" Electron/37.10.3 Safari/537.36")
	} else {
		if tokens.PostmanSID != "" {
			h.Set("Cookie", "postman.sid="+tokens.PostmanSID)
		}
		if tokens.AccessToken != "" {
			h.Set("x-access-token", tokens.AccessToken)
		}
		h.Set("x-app-version", WebAppVersion)
		h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0")
		h.Set("Origin", "https://"+tokens.WorkspaceSubdomain+".postman.co")
		h.Set("Referer", "https://"+tokens.WorkspaceSubdomain+".postman.co/")
		// 浏览器指纹头:UA 自称 Edge 151 却不发 sec-ch-ua*/sec-fetch-* 是 Cloudflare Bot
		// Management 的教科书级机器人信号。逐字段对齐真实浏览器抓包(web-chat-1.txt),压低基线 bot 分。
		h.Set("Accept", "*/*")
		h.Set("sec-ch-ua", `"Not=A?Brand";v="99", "Microsoft Edge";v="151", "Chromium";v="151"`)
		h.Set("sec-ch-ua-mobile", "?0")
		h.Set("sec-ch-ua-platform", `"macOS"`)
		h.Set("sec-fetch-dest", "empty")
		h.Set("sec-fetch-mode", "cors")
		h.Set("sec-fetch-site", "same-origin")
		h.Set("priority", "u=1, i")
	}
	return h
}

func (p *Provider) applyCookies(accountID int64, req *http.Request, egress string) {
	if p.cookies == nil || req == nil {
		return
	}
	jarCookies := p.cookies.cookies(accountID, req.URL, egress)
	if len(jarCookies) == 0 {
		return
	}
	parts := []string{}
	jarValues := map[string]string{}
	for _, cookie := range jarCookies {
		if cookie != nil && cookie.Name != "" {
			jarValues[cookie.Name] = cookie.Value
		}
	}
	if existing := req.Header.Get("Cookie"); existing != "" {
		for _, part := range strings.Split(existing, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name := strings.TrimSpace(strings.SplitN(part, "=", 2)[0])
			if _, replaced := jarValues[name]; !replaced {
				parts = append(parts, part)
			}
		}
	}
	names := make([]string, 0, len(jarValues))
	for name := range jarValues {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		parts = append(parts, name+"="+jarValues[name])
	}
	if len(parts) > 0 {
		req.Header.Set("Cookie", strings.Join(parts, "; "))
	}
}

func (p *Provider) chatURL(tokens *Tokens) string {
	if tokens.IsDesktop() {
		return DesktopChatURL
	}
	return "https://" + tokens.WorkspaceSubdomain + ".postman.co/_gw/chat"
}

func workspaceContext(tokens *Tokens) map[string]interface{} {
	// 服务端把 mandatoryContext.workspaceId 视为必填字符串；缺失会返回
	// INPUT_VALIDATION_ERROR（用户侧显示 "That was unexpected :(. Try closing active
	// tabs..."）。优先使用抓包中的 workspace UUID；未采集到 UUID 时回退到 workspace_id
	// （8 位短 id，重构前一直这样发送且服务端可正常解析）。只有两者都为空才发空对象。
	if isUUID(tokens.WorkspaceUUID) {
		return map[string]interface{}{"workspaceId": tokens.WorkspaceUUID}
	}
	if tokens.WorkspaceID != "" {
		return map[string]interface{}{"workspaceId": tokens.WorkspaceID}
	}
	return map[string]interface{}{}
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func postmanIdentityError(headers http.Header) string {
	var errors []string
	for key, values := range headers {
		if !strings.HasPrefix(strings.ToLower(key), "x-pm-error-") {
			continue
		}
		errors = append(errors, values...)
	}
	if len(errors) == 0 {
		return ""
	}
	sort.Strings(errors)
	joined := strings.Join(errors, "; ")
	lower := strings.ToLower(joined)
	if strings.Contains(lower, "identity_status") ||
		strings.Contains(lower, "guest_unusable") ||
		strings.Contains(lower, "jwt is missing") {
		return joined
	}
	return ""
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
	// RequestRejected 表示失败源于请求内容本身(坏请求、工具名冲突等),而非账号健康。
	// 这种错误换账号重试无用、且会污染整个号池,router 应直接返回、不标记账号。
	RequestRejected bool
	// GatewayBlocked 表示请求被上游网关(Cloudflare)的安全/风控拦截(WAF、Bot 评分、
	// 速率限制、Managed Challenge)。这类 403 是有状态、按评分/速率判定的瞬时拦截,而非
	// 请求内容错误也非账号损坏——退避后重试常能成功。router 应退避重试(不标记账号、不换号),
	// 而不是像 RequestRejected 那样直接返回。
	GatewayBlocked bool
	// RejectionDetail 是请求被网关拒绝时采集的排查上下文(如 Cloudflare Ray ID、
	// 出站 body 大小、响应体片段)。非空时 router 会据此写入一条告警展示到仪表盘,
	// 方便定位 403 的具体诱因。仅诊断用,不影响重试/路由决策。
	RejectionDetail string
	// RequestBytes 是本次出站请求体(JSON marshal 后)的字节数。写入 request_logs 后
	// 用于按体积分桶统计 403 发生率,判断 body 大小与 Cloudflare 403 是否相关。
	RequestBytes int
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
	res.RequestBytes = len(bodyBytes) // 记录出站体积，供 403 与请求体大小相关性分析
	// 出站 body 体检：超大 payload 是触发 Cloudflare WAF 403 的常见诱因，
	// 提前告警以便定位（如历史工具原文、超大 schema 未压缩等）。仅告警不阻断。
	if len(bodyBytes) > MaxRequestBodyWarnBytes {
		Trace(ctx, "upstream.request.oversize", map[string]interface{}{
			"account_id":      acc.ID,
			"body_bytes":      len(bodyBytes),
			"threshold_bytes": MaxRequestBodyWarnBytes,
		})
	}

	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.chatURL(tokens), strings.NewReader(string(bodyBytes)))
	if err != nil {
		res.Error = err.Error()
		return err
	}
	httpReq.Header = p.buildHeaders(tokens)
	// 出口选择：默认按账号粘性走同一代理出口；遇 Cloudflare 403 重试（EgressAttempt 递增）
	// 切下一个出口 IP；未配置代理或所有出口都试过后回退本机直连。
	client, egress, viaProxy := p.proxies.selectFor(acc.ID, req.EgressAttempt)
	if !viaProxy {
		client, egress = p.Client, "direct"
	}
	p.applyCookies(acc.ID, httpReq, egress)
	Trace(ctx, "upstream.request", map[string]interface{}{
		"method": httpReq.Method, "url": httpReq.URL.String(), "headers": httpReq.Header,
		"body": json.RawMessage(bodyBytes), "account_id": acc.ID, "egress": egress,
	})

	resp, err := client.Do(httpReq)
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
	if p.cookies != nil {
		p.cookies.remember(acc.ID, httpReq.URL, egress, resp.Cookies())
	}
	Trace(ctx, "upstream.response.headers", map[string]interface{}{
		"status": resp.StatusCode, "headers": resp.Header, "account_id": acc.ID,
	})
	res.RateLimit = parseRateLimit(resp.Header, time.Now())
	if identityError := postmanIdentityError(resp.Header); identityError != "" {
		res.Error = "Postman authentication failed: " + identityError
		res.AuthFailed = true
		return fmt.Errorf("%s", res.Error)
	}
	if isCloudflareHTMLRejection(resp.StatusCode, resp.Header) {
		res.Error = "Postman gateway rejected request (403, Cloudflare)"
		// 这是 Cloudflare 边缘的安全/风控拦截(瞬时、按评分/速率判定),不是请求内容错误也不是
		// 账号损坏——标记 GatewayBlocked 让 router 退避重试,而非当作 RequestRejected 直接返回。
		res.GatewayBlocked = true
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		Trace(ctx, "upstream.response.body", map[string]interface{}{"body": string(body), "account_id": acc.ID})
		res.RejectionDetail = cloudflareRejectionDetail(resp.StatusCode, resp.Header, string(body), len(bodyBytes)) + "\n出口: " + egress
		return fmt.Errorf("%s", res.Error)
	}

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
		// 4xx(除已处理的 401/403/429)是请求内容问题——坏请求、工具名冲突等,
		// 换账号重试无用,标记为 RequestRejected 让 router 直接返回、不污染账号。
		if resp.StatusCode < 500 {
			res.RequestRejected = true
		}
		return fmt.Errorf("%s", res.Error)
	}

	reader := NewStreamReader()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 4*1024*1024)

	firstPayloadLine := true
	for scanner.Scan() {
		line := scanner.Text()
		Trace(ctx, "upstream.response.sse", map[string]interface{}{"line": line, "account_id": acc.ID})
		// 首包内容探测：某些 Cloudflare 拦截会以 200/无 text/html 头返回 HTML 挑战页，
		// 头部判定(isCloudflareHTMLRejection)漏掉，此处按流式首个非空行内容兜底。
		if firstPayloadLine && strings.TrimSpace(line) != "" {
			firstPayloadLine = false
			if looksLikeHTML(line) {
				res.Error = "Postman gateway rejected request (Cloudflare HTML in stream)"
				res.RequestRejected = true
				res.RejectionDetail = cloudflareRejectionDetail(resp.StatusCode, resp.Header, line, len(bodyBytes)) + "\n出口: " + egress
				return fmt.Errorf("%s", res.Error)
			}
		}
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
		// 工具相关的 failure(工具名冲突、无可用工具等)是请求内容问题,不是账号故障——
		// 换账号重试无用,标记 RequestRejected 让 router 直接返回、不把账号踢出池。
		if reader.RequestRejected || isRequestRejectionMessage(reader.Err) {
			res.RequestRejected = true
		}
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

// cloudflareRejectionDetail 汇总一条可读的 403 排查上下文：出站请求体大小、
// Cloudflare Ray ID、命中的 WAF 规则头，以及拦截页正文里的关键行。用于写入告警，
// 让排查者不必翻日志就能判断诱因（如超大 body 触发 WAF、账号被封、规则误伤等）。
func cloudflareRejectionDetail(status int, headers http.Header, body string, reqBodyBytes int) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("HTTP 状态: %d", status))
	lines = append(lines, fmt.Sprintf("出站请求体: %d 字节 (软告警阈值 %d 字节)", reqBodyBytes, MaxRequestBodyWarnBytes))
	if reqBodyBytes > MaxRequestBodyWarnBytes {
		lines = append(lines, "提示: 请求体超过软告警阈值，超大 payload 可能是触发 Cloudflare WAF 403 的加重因素之一（并非唯一诱因，需结合下方体积分布判断相关性）")
	}
	if ray := strings.TrimSpace(headers.Get("Cf-Ray")); ray != "" {
		lines = append(lines, "Cf-Ray: "+ray)
	}
	if mitigated := strings.TrimSpace(headers.Get("Cf-Mitigated")); mitigated != "" {
		lines = append(lines, "Cf-Mitigated: "+mitigated)
	}
	if snippet := cloudflareBodySnippet(body); snippet != "" {
		lines = append(lines, "响应体片段: "+snippet)
	}
	return strings.Join(lines, "\n")
}

// cloudflareBodySnippet 从 Cloudflare 拦截页/挑战页正文里提取最有信息量的一小段：
// 优先 <title>，否则截取首个非空文本行，控制在 300 字符内避免撑爆告警。
func cloudflareBodySnippet(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	if i := strings.Index(lower, "<title>"); i >= 0 {
		if j := strings.Index(lower[i:], "</title>"); j >= 0 {
			title := strings.TrimSpace(body[i+len("<title>") : i+j])
			if title != "" {
				return truncateRunes(title, 300)
			}
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateRunes(line, 300)
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func isCloudflareHTMLRejection(status int, headers http.Header) bool {
	return status == http.StatusForbidden &&
		strings.EqualFold(strings.TrimSpace(headers.Get("Server")), "cloudflare") &&
		strings.Contains(strings.ToLower(headers.Get("Content-Type")), "text/html")
}

// looksLikeHTML 判断一行流式内容是否是 HTML 文档开头。用于兜底识别未带
// text/html 头的 Cloudflare 拦截页（挑战/阻断），此时上游本应是 SSE(data: ...)。
func looksLikeHTML(line string) bool {
	s := strings.ToLower(strings.TrimSpace(line))
	return strings.HasPrefix(s, "<!doctype html") ||
		strings.HasPrefix(s, "<html") ||
		strings.HasPrefix(s, "<head") ||
		strings.HasPrefix(s, "<!doctype")
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

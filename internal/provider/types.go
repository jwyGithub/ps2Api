package provider

import (
	"encoding/json"
	"time"
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
	MaxToolDescLen = 512

	// MaxUpstreamQueryRunes 是上游 Postman 服务端对 input.query 的硬性校验上限。
	// 2026-08-25 二分探测实测：10000 字符通过、10001 字符即被 INPUT_VALIDATION_ERROR
	// （"Invalid input query. If you have a large file, try importing it..."）拒收；
	// 9000 个中文字符（26994 字节）可通过，证明按字符数（rune）而非字节数计。
	MaxUpstreamQueryRunes = 10000

	// MaxToolResponseContentLen 是 TOOL_RESPONSE 续期时单条 toolResponses[].content 的上限。
	// 该字段此前不设限，续期时容易把出站 body 顶过 ~80KB 的 Cloudflare WAF 信封而触发 403
	// （精确解释了「只有带工具续期才 403、单条新消息不 403」的现象）。原生客户端本身也会裁剪
	// tool result，这里对齐该行为，保留头尾、中段截断。
	MaxToolResponseContentLen = 16 * 1024

	// FoldedToolResultTotalBudgetRunes 是「换号/冷启动 → conversationId=null」时，把历史工具
	// 结果折叠进单条 USER_QUERY 所能占用的 rune 总预算，由本次请求里参与折叠的全部 tool result
	// 「共享」。此前该路径对历史 tool result 一律写 [Previous tool result omitted]，换号后模型
	// 完全看不到已消费的工具输出，降级损失大；改为带上截断后的历史工具结果以减少降级损失。
	// 单条上限不再固定，而由 foldedToolResultBudget 按当前 tool result 条数「动态分摊」这份总
	// 预算——轮数少时每条配额大（尽量完整保留），多时自动收紧、公平分摊，避免固定上限在短会话
	// 下平白截断、在长会话下又整体超预算。取 6000：capUpstreamQuery 在 10000 上限里给中段折叠
	// 留的空间约为 10000 − 头部(30%≈3000) − 最新一轮尾部 ≈ 6000~7000，取其下沿留足余量。
	// 整段最终仍由出站前的 capUpstreamQuery 兜底到 MaxUpstreamQueryRunes 以内（≈30KB，远低于
	// Cloudflare WAF 80KB 信封，故不引入 403 风险）。
	FoldedToolResultTotalBudgetRunes = 6000

	// MinFoldedToolResultRunes 是动态分摊时单条历史 tool result 的 rune 下限：即便一次请求里
	// tool result 极多、按总预算均分后每条不足此值，也至少保留这么多（保头保尾、中段省略），
	// 保证再长的会话每条结果也留得下关键的命令/报错头尾，而不是被压到不可读。
	MinFoldedToolResultRunes = 500

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
	Model             string        `json:"model"`
	Messages          []ChatMessage `json:"messages"`
	Stream            bool          `json:"stream,omitempty"`
	Tools             []interface{} `json:"tools,omitempty"`
	ToolChoice        interface{}   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
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
	// SessionCorrupt 表示本次失败意味着 Postman 服务端会话已损坏、不可再复用：要么上游把
	// TOOL_RESPONSE 消费后回了空流(没有 [DONE]/正文/工具/failure),要么再交同一组 tool call
	// 收到 TOOL_CALL_NOT_FOUND。provider 据此定点失效该会话映射(见 InvalidateConversation),
	// 使下一轮续聊因 LookupConversation 落空而降级为 conversationId=null 的 USER_QUERY 重建,
	// 不再反复把已消费的 toolCallId 交回死会话。
	SessionCorrupt bool
	// UpstreamFailure 表示失败发生在上游自己调模型那一段(Postman → Bedrock,如 Policy Error),
	// 既非请求内容问题也非账号故障。router 据此:不把账号标记为 error(避免一次上游抖动就把号
	// 踢出 ActiveAccounts),且续聊时钉住原账号重试而不换号(换号会丢服务端会话上下文)。
	// 详见 provider.isUpstreamModelFailure。
	UpstreamFailure bool
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

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	GroupID  string `json:"-"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// EmitFunc 流式增量回调。
type EmitFunc func(d Delta) error

package provider

import (
	"net/http"
	"sort"
	"strings"
)

// outboundPlan 记录一次出站请求的「可分片」元信息，供 streamInternal 判断是否走分片续传：
// input 指向 body 里的 input 子对象（改 query/conversationId 后重新 marshal body 即可复用同一
// 信封发下一片）；fullQuery 是未经 capUpstreamQuery 截断的完整 query；chatType 用于只对
// USER_QUERY 启用分片（TOOL_RESPONSE 等工具语义路径不分片）；convID 是首片起始会话 ID。
type outboundPlan struct {
	input     map[string]interface{}
	fullQuery string
	chatType  string
	convID    string
	// chunkable 标记本次出站是否允许分片续传。只对「普通 USER_QUERY」（全新/续聊的用户文本，
	// 典型即 @ 大文件引用）开放；tool-tail（把工具结果作为 USER_QUERY 交回）与原生 TOOL_RESPONSE
	// 一律不分片——工具结果跨多轮 USER_QUERY 喂送语义可疑，且会与网关 403 钉号重试 / 会话粘性
	// 机制相互干扰（改变上游往返次数）。这些路径继续走单发 + capUpstreamQuery 中段截断。
	chunkable bool
}

func (p *Provider) buildBody(req *ChatRequest, tokens *Tokens, postmanModel string, accountID int64) (map[string]interface{}, outboundPlan) {
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
		"query":        capUpstreamQuery(split.Query),
		"toolResponse": "",
		"useCase":      nil,
		"agent":        nil,
	}
	// 记录未截断的完整 query，供分片续传按需切分（单发路径仍用上面已 cap 的值）。
	// chunkable 仅对普通 USER_QUERY 开放：既非原生 TOOL_RESPONSE，也非 tool-tail 交回。
	plan := outboundPlan{
		input:     input,
		fullQuery: split.Query,
		chatType:  "USER_QUERY",
		convID:    convID,
		chunkable: !useNativeResponse && !toolTail(req.Messages),
	}
	if convID != "" {
		input["conversationId"] = convID
	} else {
		input["conversationId"] = nil
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
		// TOOL_RESPONSE 走原生工具续期语义，不参与分片续传。
		plan.chatType = "TOOL_RESPONSE"
		plan.convID = nativeResponse.conversationID
	}

	return body, plan
}

func (p *Provider) buildHeaders(tokens *Tokens) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	// 真实 Chrome/Edge/Electron 每个请求都带 accept-encoding；自写的 FingerprintRoundTripper
	// 不像标准库 Transport 那样自动补，缺失即"非浏览器"信号，与自称 Edge 的 UA 矛盾。
	// 声明后由 RoundTripper 按 Content-Encoding 解压响应（gzip/deflate/br/zstd）。
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
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

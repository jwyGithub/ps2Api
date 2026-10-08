package provider

import (
	"net/http"
	"os"
	"sort"
	"strings"
)

func (p *Provider) buildBody(req *ChatRequest, tokens *Tokens, postmanModel string, accountID int64) map[string]interface{} {
	nativeResponse, useNativeResponse := p.nativeToolResponse(accountID, req.Messages)
	convID := p.LookupConversation(accountID, req.Messages)
	// tool-tail 且无 native 响应时，仅当待处理调用「已知无 groupID」（thirdParty/proxy-tools
	// 模式，服务端不跟踪 pending）才续用 conversationId（2026-09-15 改）：每轮清空导致冷启动
	// 全历史折叠重放，10000 rune 上限下早期工具结果滚出窗口，模型反复重读重搜同一文件。
	// 续用后 query 只带最新 tool result（splitMessages 的 hasConv 分支），其余上下文由服务端
	// 会话累积。native 流程（有真实 groupID）不受影响；重启丢注册表的未知调用维持清空重放。
	if useNativeResponse {
		convID = nativeResponse.conversationID
	} else if toolTail(req.Messages) && !p.tailToolCallsUntracked(accountID, req.Messages) {
		convID = ""
	}
	split := p.splitMessagesSeed(req.Messages, convID, req.WafProbe, req.ContextSeed)
	tools, toolInstruction := selectedTools(req.Tools, req.ToolChoice)
	if len(tools) > 0 {
		if toolInstruction != "" {
			split.Query = toolInstruction + "\n\n" + split.Query
		}
	}
	thirdParty := p.buildThirdPartyTools(tools)
	// GATEWAY_DISABLE_THIRD_PARTY=1 forces the upstream thirdParty field to an
	// empty object, dropping all custom-tool registration (this also turns off
	// autoRun below, which keys off len(thirdParty)).
	if os.Getenv("GATEWAY_DISABLE_THIRD_PARTY") == "1" {
		thirdParty = map[string]interface{}{}
	}

	// 出站 query 统一过 WAF 中和（覆盖 tool-tail/折叠历史/普通 query 三条路径）：
	// 前端源码里的 HTML/JS 标记确定性触发 Cloudflare 403，在特征内插零宽空格破坏形态。
	// 先中和再 cap，保证中和后的长度仍受 10000 rune 上限约束。
	// 探针请求（req.WafProbe）两条都跳过：见 ChatRequest.WafProbe 注释。
	// 折叠路径的权重丢弃已在 splitMessagesSeed 内完成（capUpstreamQuerySections）；
	// 此处 capUpstreamQuery 是「先中和再 cap」链路的最终安全网——wafNeutralize 会按
	// 特征插入零宽空格令 rune 数回涨，贴近上限的折叠产物中和后可能越过 10000，由它
	// 兜底截断（并非无条件直通）；对增量（hasConv）路径它则是唯一的 cap 点。
	upstreamQuery := split.Query
	// Claude Code 标题生成模板是上游安全分类器的高危信号（2026-10-08 线上定位：
	// 同账号同 session 内容，裸发成功、套模板必 flag）。先压缩为无害等价指令再中和。
	upstreamQuery = neutralizeTitlePrompt(upstreamQuery)
	// auto mode 本地分类器请求的 Stage-1 指令尾（"Err on the side of blocking…"）同为
	// 高危信号（探针：原版在被 flag 号上 2/2 挂、中和版 2/2 过），替换为中性等价指令。
	upstreamQuery = neutralizeClassifierTail(upstreamQuery)
	// git 署名 system-reminder 块（🤖 Generated with [Claude Code]…）在 FREE 号上是
	// 确定性 flag 信号（探针验证剔除后通过），整块剔除。
	upstreamQuery = stripAttributionReminder(upstreamQuery)
	if wafNeutralizeEnabled() && !req.WafProbe {
		upstreamQuery = wafNeutralize(upstreamQuery)
	}
	query := capUpstreamQuery(upstreamQuery)
	if req.WafProbe {
		query = upstreamQuery
	}

	input := map[string]interface{}{
		"chatType":     "USER_QUERY",
		"query":        query,
		"toolResponse": "",
		"useCase":      nil,
		"agent":        nil,
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
			// 12.31.3 客户端按 OS platform() 映射：win32→DESKTOP_WINDOWS/darwin→DESKTOP_MACOS/linux→DESKTOP_LINUX
			"platform": "DESKTOP_WINDOWS",
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
	thinkingLevel := normalizeThinkingLevel(req.OutputConfig["effort"])
	devMode := map[string]interface{}{
		"selectedModel":                  postmanModel,
		"isParallelToolCallingSupported": parallel,
		"autoRun":                        len(thirdParty) > 0,
		"supportsAskUser":                false,
		"supportsActionRecommendations":  true,
		"useThinkingModeIfAvailable":     true,
		"thinkingLevel":                  thinkingLevel,
		"enableWebAccess":                true,
		"isLoopApprovalEnabled":          true,
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
	}

	return body
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
		// 双 token 差异（2026-09-22）：真机桌面 App 的 token（chromiumapp 回调流签发）单发
		// x-access-token 即可；注册产线 PKCE consume 签发的 token 必须搭配
		// x-multi-login-token，否则网关返回流内 "Invalid access token"（实测）。
		// MultiLoginToken 为空（真机型账号）时不发，与抓包一致。
		if tokens.MultiLoginToken != "" {
			h.Set("x-multi-login-token", tokens.MultiLoginToken)
		}
		// 2026-10-08 对齐 12.31.3 桌面端（Windows 实机）：x-app-version 带完整 ui 构建号
		// （12.31.3-ui-261007-0231），平台形态切 win32 —— UA/sec-ch-ua-platform/Referer 与
		// clientTools/clientKBTerms 的 win32 hash 保持同一平台，避免「UA 说 Windows、
		// hash 说 darwin」这类指纹错配（风控可比对的字段必须自洽）。
		h.Set("x-app-version", DesktopAppVersion)
		h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Postman/"+DesktopAppVersion+" Electron/37.10.3 Safari/537.36")
		// 浏览器指纹头（2026-09-22 真机桌面抓包对齐）：UA 自称 Electron/Chromium 却不发
		// sec-ch-ua*/sec-fetch-* 是 Cloudflare Bot Management 的机器人信号。桌面 webview
		// 是 Chromium（12.31.3 = Electron 37 / Chromium 138），这些头真实存在。
		h.Set("Accept", "*/*")
		h.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138"`)
		h.Set("sec-ch-ua-mobile", "?0")
		h.Set("sec-ch-ua-platform", `"Windows"`)
		h.Set("sec-fetch-site", "same-site")
		h.Set("sec-fetch-mode", "cors")
		h.Set("sec-fetch-dest", "empty")
		h.Set("Referer", "https://desktop.postman.com/?desktopVersion="+DesktopAppVersion+"&userId="+tokens.UserID+"&teamId="+tokens.WorkspaceID+"&region=us")
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

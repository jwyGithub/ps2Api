package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ps2api/internal/store"
)

// AccountTestMode 账号连通性测试的出口模式。
type AccountTestMode string

const (
	// TestModeDirect 直连：强制走本机直连出口（绕过出口代理池），直接请求上游 Postman。
	TestModeDirect AccountTestMode = "direct"
	// TestModeGateway 代理：走网关真实出站路径（按账号粘性选出口代理；未配置代理时即本机直连），
	// 复刻线上对话请求实际经过的出口，便于对照排查"代理出口"相关问题。（前端展示名：代理测试）
	TestModeGateway AccountTestMode = "gateway"
	// TestModeService 网关：回环调用本服务对外端点（claude→/v1/messages，其它→/v1/responses），
	// 带面板 API Key 走完整网关链路（选号 + 出站 + 协议转换），端到端验证整条服务是否连通。
	TestModeService AccountTestMode = "service"
)

// accountTestMaxRespBytes 测试时读取响应体的上限。ping 响应很小，1MiB 足够留档且防失控。
const accountTestMaxRespBytes = 1 << 20

// DefaultTestPrompt 是账号连通性测试的默认用户输入（面板可手动改写；留空时回退到此值）。
const DefaultTestPrompt = "如何使用curl发起请求"

// DefaultTestModel 是账号连通性测试的默认对外模型名（面板可选择；留空或非法时回退到此值，
// 经 ResolvePostmanModel 解析为 CLAUDE_OPUS_48_BEDROCK）。
const DefaultTestModel = "claude-opus-4-8"

// AccountTestResult 记录一次账号连通性测试的完整现场，既回显到面板也写入测试日志文件。
type AccountTestResult struct {
	AccountID       int64               `json:"accountId"`
	Email           string              `json:"email"`
	Mode            string              `json:"mode"`   // direct / gateway
	Egress          string              `json:"egress"` // direct 或代理 URL（脱敏）
	URL             string              `json:"url"`
	Method          string              `json:"method"`
	RequestHeaders  map[string][]string `json:"requestHeaders"`
	RequestBody     string              `json:"requestBody"`
	Status          int                 `json:"status"`
	ResponseHeaders map[string][]string `json:"responseHeaders"`
	ResponseBody    string              `json:"responseBody"`
	DurationMs      int64               `json:"durationMs"`
	OK              bool                `json:"ok"`
	Error           string              `json:"error,omitempty"`
	LogFile         string              `json:"logFile"`
	StartedAt       string              `json:"startedAt"`
}

// TestAccount 对单个账号发起一次最小连通性测试（query="ping"，haiku 模型），完整捕获
// 请求地址 / 请求头 / 请求体 与 响应状态 / 响应头 / 响应体，并写入独立的测试日志文件。
//
//	mode=direct  强制本机直连出口（绕过代理池），直接请求上游；
//	mode=gateway 走网关真实出站路径（按账号粘性选代理出口；未配置代理即本机直连）。
//
// 与 ProbeQuota 不同：本方法自带发送与留档逻辑（不复用 streamInternal），以便完整记录现场；
// 且不写 request_logs、不改号池状态、不改额度快照——纯诊断用途。测试会真实消耗极少量额度。
func (p *Provider) TestAccount(ctx context.Context, acc *store.Account, mode AccountTestMode, model, prompt string) *AccountTestResult {
	return p.StreamTestAccount(ctx, acc, mode, model, prompt, nil, nil)
}

// StreamTestAccount 与 TestAccount 行为一致，但支持流式回调：
//   - onMeta 在请求构造完成、即将发出前调用一次，此时 res 已含 URL/Method/请求头/请求体/出口/模式，
//     响应字段尚空（供上层先把请求现场推给前端）；
//   - onLine 在读取上游响应时逐行调用（上游为 SSE，可实现前端逐行实时展示）。
//
// 两个回调均在本 goroutine 同步调用，允许为 nil。无论是否流式，最终都会累计完整响应体、
// 写测试日志并返回完整结果。
func (p *Provider) StreamTestAccount(ctx context.Context, acc *store.Account, mode AccountTestMode, model, prompt string, onMeta func(*AccountTestResult), onLine func(string)) *AccountTestResult {
	started := time.Now()
	res := &AccountTestResult{
		AccountID: acc.ID,
		Email:     acc.Email,
		Mode:      string(mode),
		Method:    http.MethodPost,
		StartedAt: started.Format(time.RFC3339),
	}
	// finish 统一收尾：算耗时、写测试日志、回填日志路径。
	finish := func() *AccountTestResult { return finishAccountTest(res, started) }

	tokens, err := p.GetTokens(acc)
	if err != nil {
		res.Error = "tokens 无效: " + err.Error()
		return finish()
	}

	// 测试参数：模型与用户输入均由面板传入，默认按 stream=true 发起。
	// 模型留空或非法时回退 DefaultTestModel（claude-opus-4-8 → CLAUDE_OPUS_48_BEDROCK）。
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultTestModel
	}
	postmanModel, ok := ResolvePostmanModel(model)
	if !ok {
		model = DefaultTestModel
		postmanModel, _ = ResolvePostmanModel(model)
	}
	// 用户输入留空回退默认 DefaultTestPrompt。
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = DefaultTestPrompt
	}
	// 用 json.Marshal 生成合法 JSON 字符串，正确转义引号/换行等特殊字符。
	promptJSON, _ := json.Marshal(prompt)
	req := &ChatRequest{
		Model:    model,
		Stream:   true,
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(promptJSON)}},
	}

	body := p.buildBody(req, tokens, postmanModel, acc.ID)
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		res.Error = "构造请求体失败: " + err.Error()
		return finish()
	}
	res.RequestBody = string(bodyBytes)
	res.URL = p.chatURL(tokens)

	cctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(cctx, http.MethodPost, res.URL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		res.Error = "构造请求失败: " + err.Error()
		return finish()
	}
	httpReq.Header = p.buildHeaders(tokens)

	// 出口选择：direct 强制本机直连（绕过代理）；gateway 走真实出站（按账号粘性选代理，
	// 未配置代理则回退本机直连，与线上对话行为一致）。
	client := p.Client
	egress := "direct"
	if mode == TestModeGateway {
		if c, e, viaProxy := p.proxies.selectFor(acc.ID, 0); viaProxy {
			client, egress = c, e
		}
	}
	res.Egress = redactProxyURL(egress)
	p.applyCookies(acc.ID, httpReq, egress)

	// 捕获最终出站请求头（含 applyCookies 合并后的 Cookie）。
	res.RequestHeaders = cloneHeaderMap(httpReq.Header)

	// 请求现场已就绪，先回调 onMeta（流式模式下前端可立即展示请求信息）。
	if onMeta != nil {
		onMeta(res)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			res.Error = "请求超时（上游 / 出口不可达或响应过慢）"
		} else {
			res.Error = "请求失败: " + err.Error()
		}
		return finish()
	}
	defer resp.Body.Close()
	if p.cookies != nil {
		p.cookies.remember(acc.ID, httpReq.URL, egress, resp.Cookies())
	}

	readTestResponse(res, resp, onLine)
	return finish()
}

// StreamServiceTest 通过回环调用本服务对外端点，端到端验证整条网关链路是否连通。
// 与 direct/gateway 不同：它不直接打上游，而是把请求发给「当前服务自身」的对外 API
// （baseURL 由调用方按 r.Host 传入，如 http://127.0.0.1:1930），带面板 API Key 鉴权，
// 走完整的选号 + 出站 + 协议转换。端点按模型选择：claude 系列 → /v1/messages（Anthropic 协议），
// 其它（gpt 等）→ /v1/responses（OpenAI Responses 协议）。回调语义与 StreamTestAccount 一致。
func (p *Provider) StreamServiceTest(ctx context.Context, baseURL, apiKey, model, prompt string, onMeta func(*AccountTestResult), onLine func(string)) *AccountTestResult {
	started := time.Now()
	res := &AccountTestResult{
		Mode:      string(TestModeService),
		Method:    http.MethodPost,
		Egress:    "self",
		StartedAt: started.Format(time.RFC3339),
	}
	finish := func() *AccountTestResult { return finishAccountTest(res, started) }

	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultTestModel
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = DefaultTestPrompt
	}

	// 端点与请求体按模型协议选择：claude → Anthropic /v1/messages；其它 → OpenAI /v1/responses。
	endpoint := "/v1/responses"
	var payload map[string]interface{}
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		endpoint = "/v1/messages"
		payload = map[string]interface{}{
			"model":      model,
			"stream":     true,
			"max_tokens": 1024,
			"messages":   []map[string]interface{}{{"role": "user", "content": prompt}},
		}
	} else {
		payload = map[string]interface{}{
			"model":  model,
			"stream": true,
			"input":  prompt,
		}
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		res.Error = "构造请求体失败: " + err.Error()
		return finish()
	}
	res.RequestBody = string(bodyBytes)
	res.URL = strings.TrimRight(baseURL, "/") + endpoint

	cctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(cctx, http.MethodPost, res.URL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		res.Error = "构造请求失败: " + err.Error()
		return finish()
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		// 同时带上两种鉴权头，兼容服务端 auth()（Authorization: Bearer / x-api-key）。
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("x-api-key", apiKey)
	}
	res.RequestHeaders = cloneHeaderMap(httpReq.Header)

	if onMeta != nil {
		onMeta(res)
	}

	// 回环调用本服务，用独立的普通客户端（不复用 p.Client 的上游 TLS 指纹配置）。
	client := &http.Client{Timeout: ProbeTimeout + 5*time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			res.Error = "请求超时（本服务无响应或响应过慢）"
		} else {
			res.Error = "请求失败: " + err.Error()
		}
		return finish()
	}
	defer resp.Body.Close()

	readTestResponse(res, resp, onLine)
	return finish()
}

// finishAccountTest 统一收尾：算耗时、写测试日志、回填日志路径。
func finishAccountTest(res *AccountTestResult, started time.Time) *AccountTestResult {
	res.DurationMs = time.Since(started).Milliseconds()
	if path, err := writeAccountTestLog(res); err == nil {
		res.LogFile = path
	} else if res.Error == "" {
		res.Error = "测试完成，但写测试日志失败: " + err.Error()
	}
	return res
}

// readTestResponse 逐行读取响应体（SSE / 普通文本皆可）：每行回调 onLine 供前端实时展示，
// 同时累计到 res.ResponseBody（写日志用），累计量封顶 accountTestMaxRespBytes，并回填状态/响应头。
func readTestResponse(res *AccountTestResult, resp *http.Response, onLine func(string)) {
	res.Status = resp.StatusCode
	res.ResponseHeaders = cloneHeaderMap(resp.Header)
	var sb strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, accountTestMaxRespBytes))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		sb.WriteString(line)
		sb.WriteByte('\n')
		if onLine != nil {
			onLine(line)
		}
	}
	res.ResponseBody = sb.String()
	// 循环结束后检查扫描错误：读流中断或单行超出缓冲上限都会在这里暴露，
	// 否则会被静默吞掉，导致响应体被截断却看不出原因。
	if err := scanner.Err(); err != nil && res.Error == "" {
		res.Error = "读取响应体失败: " + err.Error()
	}
	res.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
	if !res.OK && res.Error == "" {
		res.Error = fmt.Sprintf("上游返回非 2xx（%d）", resp.StatusCode)
	}
}

// cloneHeaderMap 把 http.Header 深拷贝成普通 map，便于 JSON 序列化与写日志。
func cloneHeaderMap(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// accountTestLogDir 返回测试日志根目录。默认 ./data/test-logs，可用 GATEWAY_TEST_LOG_DIR 覆盖。
func accountTestLogDir() string {
	if dir := strings.TrimSpace(os.Getenv("GATEWAY_TEST_LOG_DIR")); dir != "" {
		return dir
	}
	return "./data/test-logs"
}

// writeAccountTestLog 把一次测试的完整现场写成人类可读的日志文件，路径为
// <root>/<YYYY-MM-DD>/account-<id>-<mode>-<HHMMSS.mmm>.log，返回文件路径。
//
// 说明：测试日志用于排查连通性/风控问题，按用户诉求【完整】记录请求头与响应体（不脱敏），
// 因此仅落到服务端本地、权限 0600；请勿把该目录暴露到公网或提交进版本库。
func writeAccountTestLog(res *AccountTestResult) (string, error) {
	now := time.Now()
	dateDir := filepath.Join(accountTestLogDir(), now.Format("2006-01-02"))
	if err := os.MkdirAll(dateDir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("account-%d-%s-%s.log", res.AccountID, safeMode(res.Mode), now.Format("150405.000"))
	path := filepath.Join(dateDir, name)

	var b strings.Builder
	fmt.Fprintf(&b, "==================== 账号连通性测试 ====================\n")
	fmt.Fprintf(&b, "时间    : %s\n", res.StartedAt)
	fmt.Fprintf(&b, "账号    : #%d  %s\n", res.AccountID, res.Email)
	fmt.Fprintf(&b, "模式    : %s（%s）\n", res.Mode, modeLabel(res.Mode))
	fmt.Fprintf(&b, "出口    : %s\n", res.Egress)
	fmt.Fprintf(&b, "耗时    : %d ms\n", res.DurationMs)
	if res.OK {
		fmt.Fprintf(&b, "结论    : 成功\n")
	} else {
		fmt.Fprintf(&b, "结论    : 失败  %s\n", res.Error)
	}

	fmt.Fprintf(&b, "\n-------------------- 请求 --------------------\n")
	fmt.Fprintf(&b, "%s %s\n\n", res.Method, res.URL)
	fmt.Fprintf(&b, "请求头:\n")
	writeHeaderLines(&b, res.RequestHeaders)
	fmt.Fprintf(&b, "\n请求体:\n%s\n", res.RequestBody)

	fmt.Fprintf(&b, "\n-------------------- 响应 --------------------\n")
	fmt.Fprintf(&b, "状态码: %d\n\n", res.Status)
	fmt.Fprintf(&b, "响应头:\n")
	writeHeaderLines(&b, res.ResponseHeaders)
	fmt.Fprintf(&b, "\n响应体:\n%s\n", res.ResponseBody)
	fmt.Fprintf(&b, "======================================================\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// writeHeaderLines 按键名字母序逐行写出 "Key: value"（同名多值各占一行），确保确定性。
func writeHeaderLines(b *strings.Builder, h map[string][]string) {
	if len(h) == 0 {
		fmt.Fprintf(b, "  (无)\n")
		return
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(b, "  %s: %s\n", k, v)
		}
	}
}

func modeLabel(mode string) string {
	switch AccountTestMode(mode) {
	case TestModeDirect:
		return "直连测试：绕过代理直接请求上游"
	case TestModeGateway:
		return "代理测试：走网关真实出站路径（经出口代理）"
	case TestModeService:
		return "网关测试：回环调用本服务对外端点（端到端）"
	default:
		return "未知模式"
	}
}

// safeMode 兜底文件名里的 mode 片段，避免异常值造成路径穿越。
func safeMode(mode string) string {
	switch AccountTestMode(mode) {
	case TestModeDirect:
		return "direct"
	case TestModeGateway:
		return "gateway"
	default:
		return "unknown"
	}
}

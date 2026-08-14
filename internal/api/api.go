package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ps2api/internal/dashboard"
	"ps2api/internal/provider"
	"ps2api/internal/router"
	"ps2api/internal/store"
)

type Server struct {
	Store  *store.Store
	Router *router.Router
}

func New(s *store.Store) *Server {
	srv := &Server{Store: s, Router: router.New(s)}
	// 后台告警评估器：基于真实日志/额度统计定期落告警（见 metrics.go）
	go srv.runAlertEvaluator()
	return srv
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", traceChat(s.openAI))
	mux.HandleFunc("POST /v1/messages", traceChat(s.anthropic))
	mux.HandleFunc("GET /api/accounts", s.accounts)
	mux.HandleFunc("POST /api/accounts", s.addAccount)
	mux.HandleFunc("GET /api/accounts/export", s.exportAccounts)
	mux.HandleFunc("POST /api/accounts/import", s.importAccounts)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("PATCH /api/accounts/{id}", s.toggleAccount)
	mux.HandleFunc("GET /api/stats", s.stats)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/analytics", s.analytics)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("GET /api/alerts", s.alerts)
	mux.HandleFunc("POST /api/alerts/{id}/resolve", s.resolveAlert)
	mux.HandleFunc("POST /api/alerts/resolve-all", s.resolveAllAlerts)
	mux.HandleFunc("POST /api/refresh-quota", s.refreshQuota)
	mux.HandleFunc("GET /dashboard.js", s.dashboardAsset)
	mux.HandleFunc("GET /dashboard/", s.dashboardStatic)
	mux.HandleFunc("GET /", s.dashboard)
}

const maxChatBody = 16 << 20

func traceChat(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, traceID := provider.NewTraceContext(r.Context())
		if traceID == "" {
			next(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxChatBody+1))
		if err != nil {
			provider.Trace(ctx, "client.request.error", map[string]interface{}{"error": err.Error()})
			jsonError(w, 400, err.Error(), "invalid_request")
			return
		}
		if len(body) > maxChatBody {
			provider.Trace(ctx, "client.request.error", map[string]interface{}{"error": "request body too large", "bytes": len(body)})
			jsonError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request")
			return
		}
		var loggedBody interface{} = string(body)
		if json.Valid(body) {
			loggedBody = json.RawMessage(body)
		}
		r = r.WithContext(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		w.Header().Set("X-Postman2API-Trace-ID", traceID)
		provider.Trace(ctx, "client.request", map[string]interface{}{
			"method": r.Method, "path": r.URL.RequestURI(), "remote_addr": r.RemoteAddr,
			"headers": r.Header, "body": loggedBody,
		})
		next(&traceResponseWriter{ResponseWriter: w, ctx: ctx}, r)
	}
}

type traceResponseWriter struct {
	http.ResponseWriter
	ctx    context.Context
	status int
}

func (w *traceResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	provider.Trace(w.ctx, "client.response.headers", map[string]interface{}{"status": status, "headers": w.Header()})
	w.ResponseWriter.WriteHeader(status)
}

func (w *traceResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	provider.Trace(w.ctx, "client.response.body", map[string]interface{}{"body": string(body)})
	return w.ResponseWriter.Write(body)
}

func (w *traceResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (s *Server) auth(w http.ResponseWriter, r *http.Request) bool {
	key := s.apiKey()
	// 未设置 API Key 时不鉴权（首次进面板设置前的引导态）。
	if key == "" {
		return true
	}
	if r.Header.Get("Authorization") != "Bearer "+key {
		jsonError(w, 401, "Invalid API key", "invalid_api_key")
		return false
	}
	return true
}

// apiKey 从 SQLite settings 读取当前生效的客户端 Bearer Key（面板可动态修改，改后立即生效）。
func (s *Server) apiKey() string {
	v, _ := s.Store.GetSetting("api_key")
	return strings.TrimSpace(v)
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	jsonWrite(w, 200, map[string]interface{}{"status": "ok"})
}
func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"object": "list", "data": provider.PostmanModels})
}

func (s *Server) openAI(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var req provider.ChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil {
		jsonError(w, 400, err.Error(), "invalid_request")
		return
	}
	if req.Model == "" || len(req.Messages) == 0 {
		jsonError(w, 400, "model and messages are required", "invalid_request")
		return
	}
	req.Endpoint = "openai"
	if req.Stream {
		s.streamOpenAI(w, r, &req)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	if err != nil {
		jsonError(w, 503, err.Error(), "provider_error")
		return
	}
	jsonWrite(w, 200, openAIResponse(res, req.Model))
}
func (s *Server) streamOpenAI(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, 500, "stream unsupported", "internal_error")
		return
	}
	id := newID("chatcmpl-")
	created := nowUnix()
	emit := func(d provider.Delta) error {
		chunk := map[string]interface{}{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []interface{}{map[string]interface{}{"index": 0, "delta": deltaMap(d), "finish_reason": nil}}}
		if d.HasFinish {
			chunk["choices"] = []interface{}{map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": d.FinishReason}}
		}
		return sse(w, fl, chunk)
	}
	_, _, err := s.Router.Stream(r.Context(), req, emit)
	if err != nil {
		_ = sse(w, fl, map[string]interface{}{"error": map[string]string{"message": err.Error()}})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	fl.Flush()
}
func deltaMap(d provider.Delta) map[string]interface{} {
	m := map[string]interface{}{}
	if d.Content != "" {
		m["content"] = d.Content
	}
	if d.ReasoningContent != "" {
		m["reasoning_content"] = d.ReasoningContent
	}
	if len(d.ToolCalls) > 0 {
		m["tool_calls"] = d.ToolCalls
	}
	return m
}
func openAIResponse(res *provider.Result, model string) map[string]interface{} {
	// OpenAI 规范：存在 tool_calls 时 content 必须为 null。
	msg := map[string]interface{}{"role": "assistant"}
	if len(res.ToolCalls) == 0 {
		msg["content"] = res.Content
	} else {
		msg["content"] = nil
	}
	if res.ReasoningContent != "" {
		msg["reasoning_content"] = res.ReasoningContent
	}
	if len(res.ToolCalls) > 0 {
		msg["tool_calls"] = res.ToolCalls
	}
	finish := "stop"
	if len(res.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	return map[string]interface{}{"id": newID("chatcmpl-"), "object": "chat.completion", "created": nowUnix(), "model": model, "choices": []interface{}{map[string]interface{}{"index": 0, "message": msg, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": res.PromptTokens, "completion_tokens": res.CompletionTokens, "total_tokens": res.PromptTokens + res.CompletionTokens}}
}

type AnthropicReq struct {
	Model       string                   `json:"model"`
	Messages    []AnthropicMsg           `json:"messages"`
	System      interface{}              `json:"system"`
	MaxTokens   int                      `json:"max_tokens"`
	Stream      bool                     `json:"stream"`
	Temperature float64                  `json:"temperature"`
	Tools       []map[string]interface{} `json:"tools"`
	ToolChoice  interface{}              `json:"tool_choice"`
}
type AnthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var ar AnthropicReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&ar); err != nil {
		jsonError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	if ar.Model == "" || len(ar.Messages) == 0 {
		jsonError(w, 400, "model and messages are required", "invalid_request_error")
		return
	}
	req := anthropicToOpenAI(ar)
	req.Endpoint = "anthropic"
	if ar.Stream {
		req.Stream = true
		s.streamAnthropic(w, r, &req, ar)
		return
	}
	res, _, err := s.Router.Chat(r.Context(), &req)
	if err != nil {
		jsonError(w, 503, err.Error(), "api_error")
		return
	}
	jsonWrite(w, 200, openAIToAnthropic(res, ar.Model))
}
func anthropicToOpenAI(a AnthropicReq) provider.ChatRequest {
	msgs := []provider.ChatMessage{}
	if a.System != nil {
		b, _ := json.Marshal(a.System)
		msgs = append(msgs, provider.ChatMessage{Role: "system", Content: b})
	}
	for _, m := range a.Messages {
		msgs = append(msgs, anthropicMessageToOpenAI(m))
	}
	req := provider.ChatRequest{Model: normalizeModel(a.Model), Messages: msgs, Tools: mapsToInterfaces(a.Tools), ToolChoice: a.ToolChoice}
	if choice, ok := a.ToolChoice.(map[string]interface{}); ok {
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
			parallel := !disabled
			req.ParallelToolCalls = &parallel
		}
	}
	return req
}

// anthropicMessageToOpenAI preserves tool_use blocks in the internal
// tool_calls field so conversation fingerprints can reuse the Postman session
// when the next request carries the corresponding tool_result blocks.
func anthropicMessageToOpenAI(m AnthropicMsg) provider.ChatMessage {
	msg := provider.ChatMessage{Role: m.Role, Content: m.Content}
	if m.Role != "assistant" || len(m.Content) == 0 {
		return msg
	}
	var blocks []struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return msg
	}
	var calls []provider.ToolCall
	var text []string
	for _, block := range blocks {
		switch block.Type {
		case "tool_use":
			if block.ID == "" || block.Name == "" {
				continue
			}
			args := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				var input interface{}
				if json.Unmarshal(block.Input, &input) == nil {
					if b, err := json.Marshal(input); err == nil {
						args = string(b)
					}
				}
			}
			tc := provider.ToolCall{ID: block.ID, Type: "function"}
			tc.Function.Name = block.Name
			tc.Function.Arguments = args
			calls = append(calls, tc)
		case "text":
			if block.Text != "" {
				text = append(text, block.Text)
			}
		}
	}
	if len(calls) == 0 {
		return msg
	}
	if b, err := json.Marshal(calls); err == nil {
		msg.ToolCalls = b
	}
	textJSON, _ := json.Marshal(strings.Join(text, "\n"))
	msg.Content = textJSON
	return msg
}
func mapsToInterfaces(in []map[string]interface{}) []interface{} {
	out := make([]interface{}, len(in))
	for i := range in {
		out[i] = in[i]
	}
	return out
}
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if strings.HasPrefix(m, "claude-sonnet-4-20250514") {
		return "claude-sonnet-4-6"
	}
	if strings.HasPrefix(m, "claude-opus-4-20250514") {
		return "claude-opus-4-8"
	}
	if strings.HasPrefix(m, "claude-") && !strings.Contains(m, "4-") {
		return "claude-sonnet-4-6"
	}
	return m
}
func openAIToAnthropic(res *provider.Result, model string) map[string]interface{} {
	blocks := []map[string]interface{}{}
	if res.ReasoningContent != "" {
		blocks = append(blocks, map[string]interface{}{"type": "thinking", "thinking": res.ReasoningContent, "signature": ""})
	}
	if res.Content != "" {
		blocks = append(blocks, map[string]interface{}{"type": "text", "text": res.Content})
	}
	for _, tc := range res.ToolCalls {
		var input interface{}
		if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
			input = map[string]interface{}{}
		}
		blocks = append(blocks, map[string]interface{}{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input})
	}
	stop := "end_turn"
	if len(res.ToolCalls) > 0 {
		stop = "tool_use"
	}
	return map[string]interface{}{"id": newID("msg_"), "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": res.PromptTokens, "output_tokens": res.CompletionTokens}}
}
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest, ar AnthropicReq) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, 500, "stream unsupported", "internal_error")
		return
	}
	id := newID("msg_")
	writeEvent := func(name string, v interface{}) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, mustJSON(v))
		fl.Flush()
	}
	writeEvent("message_start", map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": id, "type": "message", "role": "assistant", "model": ar.Model, "content": []interface{}{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": provider.EstimateMessagesTokens(req.Messages), "output_tokens": 0}}})
	thinkingOpen := false
	thinkingIndex := -1
	textOpen := false
	textIndex := -1
	nextIndex := 0
	sawTools := false
	toolIndexes := map[int]int{}
	toolOrder := []int{}
	closeThinking := func() {
		if thinkingOpen {
			writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": thinkingIndex, "delta": map[string]string{"type": "signature_delta", "signature": ""}})
			writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": thinkingIndex})
			thinkingOpen = false
		}
	}
	closeText := func() {
		if textOpen {
			writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": textIndex})
			textOpen = false
		}
	}
	_, _, err := s.Router.Stream(r.Context(), req, func(d provider.Delta) error {
		if d.ReasoningContent != "" {
			closeText()
			if !thinkingOpen {
				thinkingIndex = nextIndex
				nextIndex++
				writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": thinkingIndex, "content_block": map[string]string{"type": "thinking", "thinking": "", "signature": ""}})
				thinkingOpen = true
			}
			writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": thinkingIndex, "delta": map[string]string{"type": "thinking_delta", "thinking": d.ReasoningContent}})
		}
		if d.Content != "" {
			closeThinking()
			if !textOpen {
				textIndex = nextIndex
				nextIndex++
				writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": textIndex, "content_block": map[string]string{"type": "text", "text": ""}})
				textOpen = true
			}
			writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": textIndex, "delta": map[string]string{"type": "text_delta", "text": d.Content}})
		}
		for _, tc := range d.ToolCalls {
			closeThinking()
			closeText()
			sawTools = true
			idx, exists := toolIndexes[tc.Index]
			if !exists {
				idx = nextIndex
				nextIndex++
				toolIndexes[tc.Index] = idx
				toolOrder = append(toolOrder, tc.Index)
			}
			name := ""
			args := ""
			if tc.Function != nil {
				name = tc.Function.Name
				args = tc.Function.Arguments
			}
			if !exists {
				writeEvent("content_block_start", map[string]interface{}{"type": "content_block_start", "index": idx, "content_block": map[string]interface{}{"type": "tool_use", "id": tc.ID, "name": name, "input": map[string]interface{}{}}})
			}
			if args != "" {
				writeEvent("content_block_delta", map[string]interface{}{"type": "content_block_delta", "index": idx, "delta": map[string]string{"type": "input_json_delta", "partial_json": args}})
			}
		}
		return nil
	})
	closeThinking()
	closeText()
	for _, toolIndex := range toolOrder {
		writeEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": toolIndexes[toolIndex]})
	}
	if err != nil {
		writeEvent("error", map[string]interface{}{"type": "error", "error": map[string]string{"type": "api_error", "message": err.Error()}})
		return
	}
	stop := "end_turn"
	if sawTools {
		stop = "tool_use"
	}
	writeEvent("message_delta", map[string]interface{}{"type": "message_delta", "delta": map[string]string{"stop_reason": stop}, "usage": map[string]int{"output_tokens": 0}})
	writeEvent("message_stop", map[string]string{"type": "message_stop"})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	all, err := s.Store.ListAccounts()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	for _, a := range all {
		a.Tokens = ""
		a.Password = ""
	}
	jsonWrite(w, 200, map[string]interface{}{"data": all})
}

type addAccountReq struct {
	Email  string          `json:"email"`
	Tokens provider.Tokens `json:"tokens"`
}

type accountFile struct {
	Version    int                  `json:"version"`
	ExportedAt string               `json:"exported_at"`
	Accounts   []accountFileAccount `json:"accounts"`
}

type accountFileAccount struct {
	Email    string          `json:"email"`
	Password string          `json:"password"`
	Source   string          `json:"source"`
	Enabled  *bool           `json:"enabled"`
	Tokens   provider.Tokens `json:"tokens"`
}

func (s *Server) addAccount(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var q addAccountReq
	if json.NewDecoder(r.Body).Decode(&q) != nil || q.Email == "" {
		jsonError(w, 400, "email and tokens required", "invalid_request")
		return
	}
	b, _ := json.Marshal(q.Tokens)
	a, err := s.Store.UpsertAccount(q.Email, "manual", string(b), "manual")
	if err != nil {
		jsonError(w, 400, err.Error(), "invalid_request")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"success": true, "account": a})
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	all, err := s.Store.ListAccounts()
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	out := accountFile{Version: 1, ExportedAt: time.Now().Format(time.RFC3339), Accounts: make([]accountFileAccount, 0, len(all))}
	for _, account := range all {
		var tokens provider.Tokens
		if err := json.Unmarshal([]byte(account.Tokens), &tokens); err != nil {
			jsonError(w, 500, "账号 "+account.Email+" 的登录凭据无效", "internal_error")
			return
		}
		enabled := account.Enabled
		out.Accounts = append(out.Accounts, accountFileAccount{
			Email: account.Email, Password: account.Password, Source: account.Source,
			Enabled: &enabled, Tokens: tokens,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="account.json"`)
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(out)
}

func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	var input accountFile
	if err := decoder.Decode(&input); err != nil {
		jsonError(w, 400, "无效的 account.json: "+err.Error(), "invalid_request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		jsonError(w, 400, "account.json 只能包含一个 JSON 对象", "invalid_request")
		return
	}
	if input.Version != 1 || len(input.Accounts) == 0 {
		jsonError(w, 400, "account.json 版本必须为 1 且至少包含一个账号", "invalid_request")
		return
	}
	seen := make(map[string]bool, len(input.Accounts))
	for i := range input.Accounts {
		account := &input.Accounts[i]
		account.Email = strings.TrimSpace(account.Email)
		if account.Email == "" || account.Enabled == nil {
			jsonError(w, 400, fmt.Sprintf("第 %d 个账号缺少 email 或 enabled", i+1), "invalid_request")
			return
		}
		if seen[account.Email] {
			jsonError(w, 400, "account.json 包含重复账号: "+account.Email, "invalid_request")
			return
		}
		seen[account.Email] = true
		if account.Tokens.UserID == "" || account.Tokens.WorkspaceID == "" || (account.Tokens.AccessToken == "" && account.Tokens.PostmanSID == "") {
			jsonError(w, 400, "账号 "+account.Email+" 的 tokens 不完整", "invalid_request")
			return
		}
		if account.Tokens.AccessToken == "" && account.Tokens.WorkspaceSubdomain == "" {
			jsonError(w, 400, "账号 "+account.Email+" 缺少 workspace_subdomain", "invalid_request")
			return
		}
	}
	for _, account := range input.Accounts {
		tokens, _ := json.Marshal(account.Tokens)
		if err := s.Store.ImportAccount(account.Email, account.Password, string(tokens), account.Source, *account.Enabled); err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
	}
	jsonWrite(w, 200, map[string]interface{}{"success": true, "imported": len(input.Accounts)})
}

// refreshQuota 对从未采集过额度的账号发起轻量探测调用并写库，
// 供额度管理页「刷新额度」按钮调用；已采集过额度的账号自动跳过。
func (s *Server) refreshQuota(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	results := s.Router.ProbeQuotas(r.Context())
	ok, failed := 0, 0
	for _, pr := range results {
		if pr.OK {
			ok++
		} else {
			failed++
		}
	}
	jsonWrite(w, 200, map[string]interface{}{"ok": ok, "failed": failed, "results": results})
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Store.DeleteAccount(id); err != nil {
		jsonError(w, 404, err.Error(), "not_found")
		return
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}
func (s *Server) toggleAccount(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var q struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&q)
	if err := s.Store.SetAccountEnabled(id, q.Enabled); err != nil {
		jsonError(w, 404, err.Error(), "not_found")
		return
	}
	// 重新启用账号时，其历史未处理告警自动解决（真实告警生命周期）
	if q.Enabled {
		_ = s.Store.ResolveAlertsBySource("account", "account_error", id)
		_ = s.Store.ResolveAlertsBySource("account", "quota_exhausted", id)
		_ = s.Store.ResolveAlertsBySource("account", "low_quota", id)
	}
	jsonWrite(w, 200, map[string]bool{"success": true})
}
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	v, e := s.Store.GetStats()
	if e != nil {
		jsonError(w, 500, e.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, v)
}
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v, _ := s.Store.GetSetting("log_retention"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	v, e := s.Store.RecentLogs(limit)
	if e != nil {
		jsonError(w, 500, e.Error(), "internal_error")
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"data": v})
}
func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	data, err := dashboard.Files.ReadFile("static/index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<h1>Dashboard not found. Place index.html in internal/dashboard/static/</h1>")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) dashboardAsset(w http.ResponseWriter, r *http.Request) {
	data, err := dashboard.Files.ReadFile("static/dashboard.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Write(data)
}

func (s *Server) dashboardStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/dashboard/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := dashboard.Files.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Write(data)
}
func jsonWrite(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func jsonError(w http.ResponseWriter, status int, msg, typ string) {
	jsonWrite(w, status, map[string]interface{}{"error": map[string]string{"message": msg, "type": typ}})
}
func sse(w http.ResponseWriter, fl http.Flusher, v interface{}) error {
	_, e := fmt.Fprintf(w, "data: %s\n\n", mustJSON(v))
	fl.Flush()
	return e
}
func mustJSON(v interface{}) string { b, _ := json.Marshal(v); return string(b) }
func newID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
func nowUnix() int64 { return time.Now().Unix() }

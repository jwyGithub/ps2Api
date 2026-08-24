package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ps2api/internal/store"
)

type Provider struct {
	Client    *http.Client
	proxies   *proxyPool
	cookies   *accountCookieJars
	convStore ConversationStore // 会话 ID / 归属账号 / 工具调用组映射（默认进程内内存，配置 REDIS_URL 时用 Redis）
}

func New() *Provider {
	// Client 为本机直连出口（用 ctx 控制超时，流式不能有总超时）；proxies 为可选出口代理池，
	// 未配置时所有请求走 Client 直连。
	return &Provider{
		Client:    &http.Client{Timeout: 0, Transport: newFingerprintTransport()},
		proxies:   newProxyPool(),
		cookies:   newAccountCookieJars(),
		convStore: newConversationStore(),
	}
}

// ConversationStorageMode 返回当前会话存储模式描述（内存 / Redis），供启动日志展示。
func (p *Provider) ConversationStorageMode() string {
	return p.convStore.Mode()
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
	res.CompletionTokens = EstimateCompletionTokens(res)
	p.RememberConversation(acc.ID, req.Messages, res)
	return res
}

// StreamChat 流式调用：上游每个增量通过 emit 回调吐给调用方。
func (p *Provider) StreamChat(ctx context.Context, acc *store.Account, req *ChatRequest, emit EmitFunc) *Result {
	res := &Result{}
	defer func() { Trace(ctx, "provider.result", res) }()
	// 后注册 → LIFO 先于上面的 Trace 执行，确保写 trace 前补齐 completion 估算。
	// 流式各 return 分支只设置了 PromptTokens，此处统一兜底 CompletionTokens。
	defer func() {
		if res.CompletionTokens == 0 {
			res.CompletionTokens = EstimateCompletionTokens(res)
		}
	}()
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
	// emittedText accumulates every plain-text byte already streamed to the
	// client via flushSafe, so we can reconstruct the full res.Content later.
	var emittedText strings.Builder
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
	// flushSafe streams the portion of the buffer that cannot be part of a
	// simulated tool-call marker, holding back only a possibly-partial marker
	// tail. This restores incremental streaming for plain-text replies while
	// still preventing a broken "<tool_call" fragment from leaking.
	flushSafe := func() error {
		safe, pending := splitStreamSafe(content.String())
		if safe == "" {
			return nil
		}
		content.Reset()
		content.WriteString(pending)
		emittedText.WriteString(safe)
		return emit(Delta{Content: safe})
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
			return flushSafe()
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
	// content now holds only the unflushed tail (a held marker region and/or a
	// trailing partial marker). Everything safe was already streamed via
	// flushSafe and recorded in emittedText.
	cleaned, sim := simulatedDeltas(content.String(), tools)
	content.Reset()
	if cleaned != "" {
		_ = emit(Delta{Content: cleaned})
	}
	res.Content = emittedText.String() + cleaned
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

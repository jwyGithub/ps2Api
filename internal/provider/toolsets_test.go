// toolsets_test.go —— ToolsetsProvider 透传的关键路径回归：
// 200 + error JSON（非 SSE）必须分类为 RateLimited 供 api 层换号重试，
// 而不是当成 Success 让客户端收到空流（2026-09-24 修复引入）。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ps2api/internal/store"
)

func toolsetsTestAccount(t *testing.T) *store.Account {
	t.Helper()
	tokens, _ := json.Marshal(map[string]string{
		"access_token": "x", "user_id": "u", "workspace_id": "w",
	})
	return &store.Account{ID: 88, Tokens: string(tokens), Enabled: true, Status: "active"}
}

func newToolsetsTestProvider(t *testing.T, handler http.HandlerFunc) *ToolsetsProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := New()
	p.Client = &http.Client{Transport: redirectTransport{base: http.DefaultTransport, target: srv.URL}}
	return NewToolsetsProvider(p)
}

// 流式收到 200 + upstream_unavailable JSON（非 SSE）时必须 RateLimited，不得 Success。
func TestToolsetsStreamNonSSEErrorClassified(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"upstream_unavailable","message":"The model provider is unavailable. Please try again."}}`)
	})
	var got []string
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0, func(event string, data []byte) error {
		got = append(got, event)
		return nil
	})
	if res.Success {
		t.Fatal("non-SSE error body must not be Success (client would hang on empty stream)")
	}
	if !res.RateLimited {
		t.Fatalf("upstream_unavailable must classify RateLimited for account failover, got error: %s", res.Error)
	}
	if len(got) != 0 {
		t.Fatalf("no events should be emitted, got %v", got)
	}
}

// 流式收到 rate_limited error JSON 时分类 RateLimited。
func TestToolsetsStreamRateLimitedJSON(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"Too many requests. Please try again shortly."}}`)
	})
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0, func(string, []byte) error { return nil })
	if res.Success || !res.RateLimited {
		t.Fatalf("rate_limited JSON must classify RateLimited, got success=%v rateLimited=%v err=%s", res.Success, res.RateLimited, res.Error)
	}
}

// 正常 SSE 流必须逐事件透传、model 已被改写为三段式路由名。
func TestToolsetsStreamPassthrough(t *testing.T) {
	var upstreamModel string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		json.Unmarshal(body["model"], &upstreamModel)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	var events []string
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0, func(event string, data []byte) error {
		events = append(events, event)
		return nil
	})
	if !res.Success {
		t.Fatalf("normal SSE stream must succeed, err=%s", res.Error)
	}
	if upstreamModel != "claude-opus-5/anthropic/anthropic-messages" {
		t.Fatalf("upstream model must be three-segment route, got %q", upstreamModel)
	}
	want := []string{"message_start", "content_block_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// 非流式 200 + error JSON 同样分类（原有 Chat 路径回归）。
func TestToolsetsChatNonSSEErrorClassified(t *testing.T) {
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":{"code":"rate_limited","message":"Too many requests."}}`)
	})
	res, _ := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(`{"model":"claude-opus-5","messages":[]}`), "claude-opus-5", 0)
	if res.Success || !res.RateLimited {
		t.Fatalf("rate_limited JSON must classify RateLimited, got success=%v err=%s", res.Success, res.Error)
	}
}

// 出站适配：max_tokens 夹紧到上游硬上限 8096，超长工具名（MCP 名 >64 字符）压缩改写。
func TestToolsetsUpstreamAdaptation(t *testing.T) {
	var got struct {
		MaxTokens int             `json:"max_tokens"`
		Tools     []map[string]any `json:"tools"`
	}
	longName := "mcp__plugin_chrome-devtools-mcp_chrome-devtools__performance_analyze_insight" // 74 字符
	var tp *ToolsetsProvider // handler 执行时（Chat 出站中）已赋值
	tp = newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"claude-opus-5","content":[{"type":"tool_use","id":"t1","name":%q,"input":{}}]}`,
			mappedNameForTest(t, tp, longName))
	})
	raw := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":32000,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":%q,"description":"","input_schema":{"type":"object"}}]}`, longName)
	res, body := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0)
	if !res.Success {
		t.Fatalf("chat must succeed, err=%s", res.Error)
	}
	if got.MaxTokens != 8096 {
		t.Fatalf("max_tokens must be clamped to 8096, got %d", got.MaxTokens)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools must survive rewrite, got %d", len(got.Tools))
	}
	upName := got.Tools[0]["name"].(string)
	if len(upName) > 64 || !toolsetsToolNameRe.MatchString(upName) {
		t.Fatalf("upstream tool name must be <=64 & legal, got %q", upName)
	}
	// 响应里的 tool_use.name 必须回写客户端原名。
	var resp struct {
		Content []struct {
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Content) != 1 {
		t.Fatalf("bad response body: %s", body)
	}
	if resp.Content[0].Name != longName {
		t.Fatalf("response tool name must be original %q, got %q", longName, resp.Content[0].Name)
	}
}

// mappedNameForTest 取出 provider 对 longName 的实际改写名（供 mock 返回上游视角）。
func mappedNameForTest(t *testing.T, tp *ToolsetsProvider, orig string) string {
	t.Helper()
	mapped := tp.mapToolName(orig)
	if mapped == orig || len(mapped) > 64 {
		t.Fatalf("mapToolName must compress, got %q", mapped)
	}
	return mapped
}

// 续聊：messages 历史里回传的 tool_use（客户端原名）出站也必须改写，否则第二轮 400。
func TestToolsetsUpstreamAdaptsHistoryToolUse(t *testing.T) {
	longName := strings.Repeat("b", 70)
	var upstreamName string
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, m := range body.Messages {
			for _, c := range m.Content {
				if c.Type == "tool_use" {
					upstreamName = c.Name
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"claude-opus-5","content":[{"type":"text","text":"ok"}]}`)
	})
	raw := fmt.Sprintf(`{"model":"claude-opus-5","max_tokens":1024,"messages":[`+
		`{"role":"user","content":[{"type":"text","text":"hi"}]},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":%q,"input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"done"}]}]}`, longName)
	res, _ := tp.Chat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0)
	if !res.Success {
		t.Fatalf("chat must succeed, err=%s", res.Error)
	}
	if upstreamName == "" || upstreamName == longName {
		t.Fatalf("history tool_use name must be rewritten upstream, got %q", upstreamName)
	}
	if !toolsetsToolNameRe.MatchString(upstreamName) || len(upstreamName) > 64 {
		t.Fatalf("rewritten name must be legal, got %q", upstreamName)
	}
}

// 流式 content_block_start 的 tool_use.name 回写原名。
func TestToolsetsStreamToolNameUnmapped(t *testing.T) {
	longName := strings.Repeat("a", 70) // 超长 → 必被改写
	tp := newToolsetsTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		var tools []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body["tools"], &tools)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":%q}}\n\n",
			tools[0].Name)
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	raw := fmt.Sprintf(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}],"tools":[{"name":%q}]}`, longName)
	var blockName string
	res := tp.StreamChat(context.Background(), toolsetsTestAccount(t), []byte(raw), "claude-opus-5", 0, func(event string, data []byte) error {
		if event == "content_block_start" {
			var ev struct {
				ContentBlock struct {
					Name string `json:"name"`
				} `json:"content_block"`
			}
			_ = json.Unmarshal(data, &ev)
			blockName = ev.ContentBlock.Name
		}
		return nil
	})
	if !res.Success {
		t.Fatalf("stream must succeed, err=%s", res.Error)
	}
	if blockName != longName {
		t.Fatalf("streamed tool name must be original (len %d), got %q", len(longName), blockName)
	}
}

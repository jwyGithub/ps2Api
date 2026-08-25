package provider

import (
	"encoding/json"
	"strings"
	"time"
)

// Delta 是从 Postman SSE 流解析出来的增量，直接对应 OpenAI chunk 的 delta 结构。
type Delta struct {
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []DeltaToolCall `json:"tool_calls,omitempty"`
	FinishReason     string          `json:"-"`
	HasFinish        bool            `json:"-"`
}

type DeltaToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	GroupID  string `json:"-"`
	Function *struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type Usage struct {
	Limit             float64          `json:"limit"`
	Usage             float64          `json:"usage"`
	Overage           float64          `json:"overage"`
	Spillage          float64          `json:"spillage"`
	UserType          string           `json:"userType"`
	UsageState        string           `json:"usageState"`
	AllowOverage      bool             `json:"allowOverage"`
	WarningThresholds []UsageThreshold `json:"warningThresholds"`
	UsageCycle        *UsageCycle      `json:"usageCycle"`
	IsTeamPooled      bool             `json:"isTeamPooled"`
}

type UsageThreshold struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

type UsageCycle struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type RateLimit struct {
	Limit         int        `json:"limit"`
	Remaining     int        `json:"remaining"`
	WindowSeconds int        `json:"windowSeconds"`
	ResetAt       *time.Time `json:"resetAt,omitempty"`
}

// StreamReader 逐行解析 Postman agent-mode SSE 流。
type StreamReader struct {
	finished      bool
	QuotaExceeded bool
	Usage         *Usage
	Err           string
	// RequestRejected 表示失败源于请求内容本身（坏请求、工具名冲突等）。
	RequestRejected bool
	// UpstreamFailure 表示失败发生在 Postman → 模型（Bedrock）这一段，与账号健康和本地请求
	// 格式都无关。见 isUpstreamModelFailure。
	UpstreamFailure bool
	ActualModel     string
	ConversationID  string
	sawToolCall     bool
	toolCallIndex   map[string]int
	toolCallGroup   map[string]string
	lastToolID      string
}

func NewStreamReader() *StreamReader {
	return &StreamReader{toolCallIndex: map[string]int{}, toolCallGroup: map[string]string{}}
}

type sseEvent struct {
	EventType string          `json:"eventType"`
	Data      json.RawMessage `json:"data"`
}

func (r *StreamReader) Feed(line string) []Delta {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "data: ") {
		return nil
	}
	payload := strings.TrimPrefix(trimmed, "data: ")
	if payload == "[DONE]" {
		return nil
	}
	var ev sseEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return nil
	}
	switch ev.EventType {
	case "usage":
		return r.handleUsage(ev.Data)
	case "conversation":
		return r.handleConversation(ev.Data)
	case "textChunk":
		return r.handleTextChunk(ev.Data)
	case "thinkingChunk":
		return r.handleThinkingChunk(ev.Data)
	case "toolCallChunk":
		return r.handleToolCallChunk(ev.Data)
	case "failure":
		return r.handleFailure(ev.Data)
	default:
		// info / ping / streamingFormat / thinkingComplete / planningChunk / progressUpdate ...
		return nil
	}
}

func (r *StreamReader) Finish() []Delta {
	if r.finished {
		return nil
	}
	r.finished = true
	fr := "stop"
	if r.sawToolCall {
		fr = "tool_calls"
	}
	return []Delta{{FinishReason: fr, HasFinish: true}}
}

func (r *StreamReader) handleUsage(data json.RawMessage) []Delta {
	var u Usage
	if err := json.Unmarshal(data, &u); err != nil {
		return nil
	}
	r.Usage = &u
	switch u.UsageState {
	case "EXCEEDED", "UNAVAILABLE", "BLOCKED":
		r.QuotaExceeded = true
	}
	return nil
}

func (r *StreamReader) handleConversation(data json.RawMessage) []Delta {
	var c struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &c); err == nil && c.ID != "" {
		r.ConversationID = c.ID
	}
	return nil
}

type chunkMeta struct {
	Model          string `json:"model"`
	ConversationID string `json:"conversationId"`
}

func (r *StreamReader) applyMeta(meta *chunkMeta) {
	if meta != nil && meta.Model != "" {
		r.ActualModel = meta.Model
	}
}

func (r *StreamReader) handleTextChunk(data json.RawMessage) []Delta {
	var d struct {
		TextContent string     `json:"textContent"`
		Metadata    *chunkMeta `json:"metadata"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	r.applyMeta(d.Metadata)
	if d.TextContent != "" {
		return []Delta{{Content: d.TextContent}}
	}
	return nil
}

func (r *StreamReader) handleThinkingChunk(data json.RawMessage) []Delta {
	var d struct {
		ThinkingContent string     `json:"thinkingContent"`
		Metadata        *chunkMeta `json:"metadata"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	r.applyMeta(d.Metadata)
	if d.ThinkingContent != "" {
		return []Delta{{ReasoningContent: d.ThinkingContent}}
	}
	return nil
}

func (r *StreamReader) handleToolCallChunk(data json.RawMessage) []Delta {
	var d struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			GroupID  string `json:"toolCallGroupId"`
			Function *struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"toolCalls"`
		Metadata *chunkMeta `json:"metadata"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	r.applyMeta(d.Metadata)
	var out []Delta
	for _, tc := range d.ToolCalls {
		id := tc.ID
		if id == "" {
			id = r.lastToolID
		}
		if id == "" {
			continue
		}
		r.lastToolID = id
		if tc.GroupID != "" {
			r.toolCallGroup[id] = tc.GroupID
		}
		r.sawToolCall = true
		idx, seen := r.toolCallIndex[id]
		if !seen {
			idx = len(r.toolCallIndex)
			r.toolCallIndex[id] = idx
		}
		dtc := DeltaToolCall{Index: idx, GroupID: r.toolCallGroup[id]}
		if !seen {
			dtc.ID = id
			dtc.Type = "function"
		}
		name := ""
		var args string
		if tc.Function != nil {
			name = tc.Function.Name
			args = normalizeArguments(tc.Function.Arguments)
		}
		if !seen {
			dtc.Function = &struct {
				Name      string `json:"name,omitempty"`
				Arguments string `json:"arguments,omitempty"`
			}{Name: name, Arguments: args}
		} else {
			dtc.Function = &struct {
				Name      string `json:"name,omitempty"`
				Arguments string `json:"arguments,omitempty"`
			}{Arguments: args}
		}
		out = append(out, Delta{ToolCalls: []DeltaToolCall{dtc}})
	}
	return out
}

// normalizeArguments 把上游可能以对象/数组/数字形式返回的 arguments 规整成 JSON 字符串，
// 符合 OpenAI function calling 对 arguments 必须是字符串的要求。
func normalizeArguments(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var v interface{}
	if json.Unmarshal(raw, &v) == nil && v != nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return ""
}

func (r *StreamReader) handleFailure(data json.RawMessage) []Delta {
	var d struct {
		ErrorType   string `json:"errorType"`
		UserMessage string `json:"userMessage"`
		Message     string `json:"message"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil
	}
	r.Err = d.UserMessage
	if r.Err == "" {
		r.Err = d.ErrorType
	}
	if r.Err == "" {
		r.Err = "Unknown Postman error"
	}
	// userMessage 是给终端用户看的套话（"That was unexpected :(. Try starting a new chat…"），
	// 真正的根因只在 message 里（如 "LLM stream error: … AI_APICallError: Policy Error"）。
	// 拼进错误串，否则日志/告警/trace 里只剩套话，排查时无从区分「上游模型故障」与「本地请求有问题」。
	if detail := strings.TrimSpace(d.Message); detail != "" && detail != r.Err {
		r.Err += " | " + detail
	}
	if d.ErrorType == "USAGE_LIMIT_EXCEEDED" {
		r.QuotaExceeded = true
	}
	if d.ErrorType == "INPUT_VALIDATION_ERROR" {
		r.RequestRejected = true
	}
	if isUpstreamModelFailure(d.ErrorType) {
		r.UpstreamFailure = true
	}
	return nil
}

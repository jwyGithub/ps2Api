// modelconfig.go —— Postman Agent Mode「/chat 端模型清单」实时发现。
// 接口（2026-10-22 桌面客户端 12.33.2 webview 逆向）：webview 启动时
// getAgentModeConfig() 发 GET {__WP_ISTIO_GATEWAY_URL__}/config?platform=<PLATFORM>
// （gateway.postman.com，不带 /_gw 前缀、不过 Cloudflare WAF），头与 /chat 同族
// （x-access-token + x-pstmn-req-service: agent-mode-service + x-app-version），返回：
//   { defaultModel, models:[{key,displayName,supportsModelSwitching,isDevMode,interactions}],
//     primaryModelKeys, agents, allowDevMode, usage, mcpPolicy, postmanSkills }
// models[].key 即 /chat 出站 devModeOptions.selectedModel 的合法值（GPT_61_SOL 等下划线
// 枚举，与 toolsets 端的三段式路由名是两套体系）。客户端不升级程序也能看到新模型，
// 就是因为清单实时下发——本文件把同一接口接进网关，让 PostmanModelMap 的静态映射
// 与上游保持同步（FetchAgentModeModels 拉取 → ApplyDynamicModels 合并）。

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ps2api/internal/store"
)

// agentModeConfigURL 是 /chat 端模型清单接口（webview 同款路径与平台参数）。
func agentModeConfigURL() string {
	return "https://gateway.postman.com/config?platform=DESKTOP_WINDOWS"
}

// AgentModeModel 是 /config 返回 models[] 里网关关心字段的子集。
type AgentModeModel struct {
	Key                    string `json:"key"`
	DisplayName            string `json:"displayName"`
	SupportsModelSwitching *bool  `json:"supportsModelSwitching,omitempty"`
	IsDevMode              bool   `json:"isDevMode"`
}

// agentModeConfig 是 /config 响应里网关消费的字段（其余忽略）。
type agentModeConfig struct {
	DefaultModel     string          `json:"defaultModel"`
	Models           []AgentModeModel `json:"models"`
	PrimaryModelKeys []string        `json:"primaryModelKeys"`
}

// dynamicModels 缓存最近一次 /config 拉取结果；零值（未拉取）时静态表兜底。
type dynamicModels struct {
	mu        sync.Mutex
	fetchedAt time.Time
	// keys 是 models[].key 全集（去空格、原大小写），primary 是主推子集。
	keys    []string
	primary []string
	// aliases 把对外模型名（小写）映射到 /chat 的 selectedModel 值。
	aliases map[string]string
}

var dynModels dynamicModels

// ApplyDynamicModels 把 /config 拉到的模型清单写入缓存。
// 对外模型名 = key 转小写（GPT_61_SOL → gpt-61-sol 会与既有 gpt-6.1-sol 冲突的
// 名字不覆盖静态映射——静态表优先，避免上游改名静默劫持既有路由）。
func ApplyDynamicModels(models []AgentModeModel, primary []string) {
	dynModels.mu.Lock()
	defer dynModels.mu.Unlock()
	dynModels.fetchedAt = time.Now()
	dynModels.keys = dynModels.keys[:0]
	dynModels.primary = append(dynModels.primary[:0], primary...)
	dynModels.aliases = map[string]string{}
	for _, m := range models {
		key := strings.TrimSpace(m.Key)
		if key == "" {
			continue
		}
		dynModels.keys = append(dynModels.keys, key)
		alias := strings.ToLower(key)
		if _, exists := dynModels.aliases[alias]; !exists {
			dynModels.aliases[alias] = key
		}
	}
}

// DynamicModelKeys 返回缓存的 /chat selectedModel 合法值全集（未拉取过返回 nil）。
func DynamicModelKeys() []string {
	dynModels.mu.Lock()
	defer dynModels.mu.Unlock()
	if len(dynModels.keys) == 0 {
		return nil
	}
	out := make([]string, len(dynModels.keys))
	copy(out, dynModels.keys)
	sort.Strings(out)
	return out
}

// DynamicPrimaryModelKeys 返回主推模型子集（primaryModelKeys，未拉取过返回 nil）。
func DynamicPrimaryModelKeys() []string {
	dynModels.mu.Lock()
	defer dynModels.mu.Unlock()
	if len(dynModels.primary) == 0 {
		return nil
	}
	out := make([]string, len(dynModels.primary))
	copy(out, dynModels.primary)
	return out
}

// resolveDynamicModel 把客户端模型名（已过 PostmanModelMap 未命中）解析为 /chat 的
// selectedModel 值。命中动态别名返回 (key, true)。静态表优先由调用方保证（先查静态表）。
func resolveDynamicModel(model string) (string, bool) {
	dynModels.mu.Lock()
	defer dynModels.mu.Unlock()
	if dynModels.aliases == nil {
		return "", false
	}
	v, ok := dynModels.aliases[strings.ToLower(strings.TrimSpace(model))]
	return v, ok
}

// FetchAgentModeModels 用账号 token 实时拉 /config 模型清单。出口策略与业务流量一致
// （代理池轮换）；成功后写入动态缓存并返回解析结果。
func (p *Provider) FetchAgentModeModels(ctx context.Context, acc *store.Account) (*agentModeConfig, error) {
	tokens, err := p.GetTokens(acc)
	if err != nil {
		return nil, err
	}
	client, egress, viaProxy := p.proxies.selectFor(acc.ID, 0)
	if !viaProxy {
		client = p.Client
		egress = "direct"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", agentModeConfigURL(), nil)
	if err != nil {
		return nil, err
	}
	h := p.buildHeaders(tokens)
	// /config 与 /chat 同族（webview same-origin fetch），但非 SSE 探测请求：
	// Accept 收敛为 application/json，去掉 SSE 专属语义头避免指纹突兀。
	h.Set("Accept", "application/json")
	h.Del("Accept-Encoding")
	req.Header = h
	_ = egress
	p.applyCookies(acc.ID, req, egress)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return nil, fmt.Errorf("agent-mode config %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var cfg agentModeConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("bad agent-mode config body: %w", err)
	}
	ApplyDynamicModels(cfg.Models, cfg.PrimaryModelKeys)
	return &cfg, nil
}

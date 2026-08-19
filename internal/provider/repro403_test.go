package provider

// 403 最小复现实验（内容签名 vs 长度）。
//
// 目的：验证 Cloudflare 403 是被「出站 body 里的标记/脚本特征」触发（内容签名），
// 而非单纯的请求体过大（长度）。做法是构造若干 **字节数尽量相等** 的请求体，
// 只改变「标记密度」这一个变量，对每种变体重复发起真实上游请求，统计各自的
// 分类结果（成功 / 网关拦截 GatewayBlocked / 内容被拒 RequestRejected / 其他）
// 与 Cloudflare Ray ID。若「含 <script>/<template> 的变体」显著更容易被拦、
// 而「等长的纯文本变体」几乎不被拦，则证明是内容签名而非长度。
//
// 这是一个会 **真实消耗少量额度、并打到线上 Cloudflare** 的实验，因此默认跳过，
// 只有显式设置 REPRO_403=1 才运行：
//
//	REPRO_403=1 \
//	DATABASE_PATH=./data/gateway.db \
//	REPRO_403_REPS=3 \
//	REPRO_403_SIZE=4000 \
//	REPRO_403_MODEL=claude-haiku-4-5 \
//	REPRO_403_PROXY=0 \
//	go test ./internal/provider -run TestRepro403ContentSignature -v -count=1 -timeout 20m
//
// 说明：
//   - 403 与出口 IP/评分有关，从本机直连的结果不一定等同线上。设 REPRO_403_PROXY=1
//     可让实验走 proxy_urls 设置里的出口（与线上一致），更贴近真实诱因。
//   - 每个变体每次都重置会话并带唯一 nonce，保证是「全新对话、无历史复用」，
//     从而把变量隔离在「本轮 body 的内容」上。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ps2api/internal/store"
)

// resolveExistingDB 把 DATABASE_PATH 解析成一个「确实已存在」的库文件路径。
// 关键：`go test ./internal/provider` 的工作目录是包目录(internal/provider)，
// 不是仓库根目录。若直接把相对路径 ./data/gateway.db 交给 store.Open，SQLite 会
// 在包目录下**新建一个空库**（有 schema、零账号），于是 ActiveAccounts 返回空，
// 误报「没有活跃账号」。这里对相对路径逐级向上查找真实库，找不到就报错而非新建。
func resolveExistingDB(t *testing.T, dbPath string) string {
	t.Helper()
	if filepath.IsAbs(dbPath) {
		if _, err := os.Stat(dbPath); err != nil {
			t.Fatalf("DATABASE_PATH 指向的库不存在: %s", dbPath)
		}
		return dbPath
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取工作目录失败: %v", err)
	}
	for {
		cand := filepath.Join(dir, dbPath)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("从工作目录逐级向上都找不到已存在的库文件 %q（拒绝新建空库以免误报无账号）。"+
		"请用绝对路径设置 DATABASE_PATH，例如 DATABASE_PATH=$(pwd)/data/gateway.db", dbPath)
	return ""
}

// repro403Variant 是一个实验变体：name 用于报告，build(nonce,targetBytes) 产出该轮的用户消息文本。
type repro403Variant struct {
	name  string
	build func(nonce string, targetBytes int) string
}

// padTo 用一段良性注释把 s 填充到约 targetBytes 字节（UTF-8 近似按字节），
// 保证各变体长度可比。填充内容是纯散文，不含任何标记特征。
func padTo(s string, targetBytes int) string {
	const filler = " The quick brown fox jumps over the lazy dog while nearby a calm river flows past green fields."
	var b strings.Builder
	b.WriteString(s)
	for b.Len() < targetBytes {
		b.WriteString(filler)
	}
	out := b.String()
	if len(out) > targetBytes {
		out = out[:targetBytes]
	}
	return out
}

func repro403Variants() []repro403Variant {
	return []repro403Variant{
		{
			name: "A_plain_prose", // 对照组：等长纯文本，无任何标记
			build: func(nonce string, n int) string {
				return padTo("Please summarize this note in one short sentence. nonce="+nonce+". ", n)
			},
		},
		{
			name: "B_script_tag", // 裸 <script> 标签 + JS 事件/DOM（WAF XSS 规则最敏感）
			build: func(nonce string, n int) string {
				body := "Review this snippet. nonce=" + nonce + ". " +
					"<script>document.addEventListener('keydown',function(e){if(e.key==='Enter'){ws.close();}});" +
					"var x=document.querySelector('#app');x.innerHTML='<b>hi</b>';eval('1+1');</script> "
				return padTo(body, n)
			},
		},
		{
			name: "C_vue_sfc", // Vue 单文件组件：<template>/<style>/<script setup>
			build: func(nonce string, n int) string {
				body := "Explain this component. nonce=" + nonce + ". " +
					"<template><div class=\"url-input\" @keydown=\"onKeydown\"><el-input v-model=\"url\"/></div></template>" +
					"<script setup lang=\"ts\">import {ref} from 'vue';const url=ref('');function onKeydown(e){}</script>" +
					"<style scoped>.url-input :deep(.el-input__wrapper){box-shadow:none !important;}</style> "
				return padTo(body, n)
			},
		},
		{
			name: "D_json_script_source", // 携带「可执行脚本源码」字段的 JSON（易命中注入/RCE 签名）
			build: func(nonce string, n int) string {
				body := "Validate this request config. nonce=" + nonce + ". " +
					"{\"authType\":\"bearer\",\"authConfig\":{\"token\":\"abc\"}," +
					"\"script\":{\"enabled\":true,\"source\":\"pm.test('ok',()=>{const r=pm.response.json();eval(r.code);});\"}} "
				return padTo(body, n)
			},
		},
	}
}

// classify 把一次 Chat 结果归类成一个短标签，并抽出 Cf-Ray（若有）。
func classify(res *Result) (label, ray string) {
	switch {
	case res == nil:
		label = "nil"
	case res.Success:
		label = "success"
	case res.GatewayBlocked:
		label = "GATEWAY_BLOCKED(403)"
	case res.RequestRejected:
		label = "request_rejected"
	case res.QuotaExhausted:
		label = "quota_exhausted"
	case res.RateLimited:
		label = "rate_limited"
	case res.AuthFailed:
		label = "auth_failed"
	default:
		label = "other_error"
	}
	if res != nil && res.RejectionDetail != "" {
		for _, ln := range strings.Split(res.RejectionDetail, "\n") {
			if strings.HasPrefix(strings.TrimSpace(ln), "Cf-Ray:") {
				ray = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ln), "Cf-Ray:"))
			}
		}
	}
	return
}

func TestRepro403ContentSignature(t *testing.T) {
	if os.Getenv("REPRO_403") != "1" {
		t.Skip("设置 REPRO_403=1 运行该实验（会真实消耗额度并打到线上 Cloudflare）")
	}

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./data/gateway.db"
	}
	dbPath = resolveExistingDB(t, dbPath)
	reps := envInt("REPRO_403_REPS", 3)
	size := envInt("REPRO_403_SIZE", 4000)
	model := os.Getenv("REPRO_403_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("打开 store 失败 (%s): %v", dbPath, err)
	}
	defer s.Close()

	accs, err := s.ActiveAccounts()
	if err != nil {
		t.Fatalf("读取活跃账号失败: %v", err)
	}
	if len(accs) == 0 {
		t.Fatal("没有活跃账号，无法发起真实请求")
	}
	acc := accs[0]

	p := New()
	if os.Getenv("REPRO_403_PROXY") == "1" {
		p.SetProxyList(func() []string {
			raw, _ := s.GetSetting("proxy_urls")
			return []string{raw}
		})
		t.Logf("出口：走 proxy_urls 设置的代理（与线上一致）")
	} else {
		t.Logf("出口：本机直连（403 与出口 IP 相关，结果不一定等同线上；REPRO_403_PROXY=1 走代理）")
	}

	if _, ok := ResolvePostmanModel(model); !ok {
		t.Fatalf("模型无法解析: %s", model)
	}

	t.Logf("账号=%s  模型=%s  目标体积≈%d 字节/条  每变体重复=%d 次", acc.Email, model, size, reps)

	type agg struct {
		counts   map[string]int
		bytesMin int
		bytesMax int
		rays     []string
	}
	results := map[string]*agg{}

	for _, v := range repro403Variants() {
		a := &agg{counts: map[string]int{}, bytesMin: 1 << 30}
		for i := 0; i < reps; i++ {
			// 全新对话：重置会话 + 唯一 nonce，隔离变量到「本轮内容」。
			p.ResetConversation(acc.ID)
			nonce := v.name + "-" + strconv.Itoa(i) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			text := v.build(nonce, size)
			content, _ := json.Marshal(text) // 正确转义成 JSON 字符串
			req := &ChatRequest{
				Model:    model,
				Messages: []ChatMessage{{Role: "user", Content: content}},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			res := p.Chat(ctx, acc, req)
			cancel()

			label, ray := classify(res)
			a.counts[label]++
			if res.RequestBytes < a.bytesMin {
				a.bytesMin = res.RequestBytes
			}
			if res.RequestBytes > a.bytesMax {
				a.bytesMax = res.RequestBytes
			}
			if ray != "" {
				a.rays = append(a.rays, ray)
			}
			t.Logf("[%s #%d] -> %-20s bytes=%d ray=%s err=%.80s",
				v.name, i+1, label, res.RequestBytes, ray, oneLine(res.Error))

			time.Sleep(1500 * time.Millisecond) // 轻微退避，避免把结果污染成纯速率限制
		}
		results[v.name] = a
	}

	// ── 汇总报告 ──────────────────────────────────────────────
	t.Log("================ 403 内容签名实验 汇总 ================")
	for _, v := range repro403Variants() {
		a := results[v.name]
		blocked := a.counts["GATEWAY_BLOCKED(403)"]
		t.Logf("变体 %-22s 出站字节≈[%d..%d]  403拦截=%d/%d  明细=%v  rays=%v",
			v.name, a.bytesMin, a.bytesMax, blocked, reps, a.counts, a.rays)
	}
	t.Log("解读：若 B/C/D（含标记/脚本）的 403 明显多于等长的 A（纯文本），")
	t.Log("即证明触发因子是【内容签名】而非【请求体长度】。")
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

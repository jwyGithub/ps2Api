package provider

// 上游 input.query 长度硬上限的真实探测（桌面端 vs web 端）。
//
// 背景：MaxUpstreamQueryRunes=10000 是 2026-08-25 在 web 端二分实测出来的
// （10000 过、10001 即 INPUT_VALIDATION_ERROR）。本实验验证桌面端上游
// （workspace_localmode_v12）是否存在同样的 10000 rune 限制、边界在哪。
//
// 关键：必须用 WafProbe:true 出站——它同时旁路 capUpstreamQuery 截断（否则
// 网关自己先把 query 截到 9900 rune，永远到不了上游、测不出上游真实上限）
// 与 WAF 中和（纯文本 padding 不含特征，无需中和，且中和会改 rune 数）。
//
// 会真实消耗额度、打到线上。默认跳过，PROBE_LIMIT=1 才运行：
//
//	PROBE_LIMIT=1 \
//	DATABASE_PATH=./data/gateway.db \
//	PROBE_LIMIT_MODEL=claude-opus-4-8 \
//	PROBE_LIMIT_PROXY=0 \
//	go test ./internal/provider -run TestProbeUpstreamQueryLimit -v -count=1 -timeout 20m
//
// 可选：
//   - PROBE_LIMIT_SIZES=9900,10001,15000 逗号分隔的 rune 梯度，覆盖默认梯度（省额度）。
//   - PROBE_LIMIT_ACCOUNT=<id或邮箱片段> 指定探针账号（默认取第一个活跃账号）。

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"ps2api/internal/store"
)

func TestProbeUpstreamQueryLimit(t *testing.T) {
	if os.Getenv("PROBE_LIMIT") != "1" {
		t.Skip("设置 PROBE_LIMIT=1 运行该实验（会真实消耗额度并打到线上上游）")
	}

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./data/gateway.db"
	}
	dbPath = resolveExistingDB(t, dbPath)

	model := os.Getenv("PROBE_LIMIT_MODEL")
	if model == "" {
		model = "claude-opus-4-8"
	}

	// 默认长度梯度（rune）：贴着 10000 边界密集取样，两侧各留观察点。
	sizes := []int{8000, 9900, 10000, 10001, 11000, 12000, 15000, 20000, 30000, 50000}
	if raw := os.Getenv("PROBE_LIMIT_SIZES"); raw != "" {
		sizes = sizes[:0]
		for _, p := range strings.Split(raw, ",") {
			if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
				sizes = append(sizes, n)
			}
		}
	}
	sort.Ints(sizes)
	if len(sizes) == 0 {
		t.Fatal("无有效长度梯度")
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
	if want := os.Getenv("PROBE_LIMIT_ACCOUNT"); want != "" {
		found := false
		for _, a := range accs {
			if strings.Contains(a.Email, want) || strconv.FormatInt(a.ID, 10) == want {
				acc = a
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("PROBE_LIMIT_ACCOUNT=%q 在活跃账号中未找到", want)
		}
	}

	p := New()
	if os.Getenv("PROBE_LIMIT_PROXY") == "1" {
		p.SetProxyList(func() []string {
			raw, _ := s.GetSetting("proxy_urls")
			return []string{raw}
		})
		t.Logf("出口：走 proxy_urls 设置的代理（与线上一致）")
	} else {
		t.Logf("出口：本机直连（结果不一定等同线上；PROBE_LIMIT_PROXY=1 走代理）")
	}

	if _, ok := ResolvePostmanModel(model); !ok {
		t.Fatalf("模型无法解析: %s", model)
	}

	t.Logf("账号=%s  模型=%s  梯度(rune)=%v", acc.Email, model, sizes)
	t.Logf("网关本地 cap=%d rune（WafProbe 已旁路截断，原样出站测上游真实上限）", MaxUpstreamQueryRunes)

	type row struct {
		runes  int
		label  string
		errTxt string
	}
	var rows []row
	maxOK := 0
	minReject := 0

	for _, n := range sizes {
		p.ResetConversation(acc.ID)
		nonce := "limit-" + strconv.Itoa(n) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		// 纯 ASCII → rune 数 == 字节数，query 精确等于 n 个 rune。
		text := padTo("Please reply with the single word OK. nonce="+nonce+". ", n)
		gotRunes := len([]rune(text))
		content, _ := json.Marshal(text)

		req := &ChatRequest{
			Model:    model,
			Messages: []ChatMessage{{Role: "user", Content: content}},
			WafProbe: true, // 旁路 cap + 中和：原样出站，测上游真实长度校验
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		res := p.Chat(ctx, acc, req)
		cancel()

		label, ray := classify(res)
		errTxt := ""
		if res != nil {
			errTxt = oneLine(res.Error)
		}
		rows = append(rows, row{runes: gotRunes, label: label, errTxt: errTxt})
		t.Logf("[size=%-6d runes=%-6d] -> %-22s ray=%s err=%.120s", n, gotRunes, label, ray, errTxt)

		if res != nil && res.Success {
			if gotRunes > maxOK {
				maxOK = gotRunes
			}
		} else if res != nil && res.RequestRejected {
			if minReject == 0 || gotRunes < minReject {
				minReject = gotRunes
			}
		}
	}

	t.Logf("================ 上游 query 长度上限探测 汇总 ================")
	for _, r := range rows {
		t.Logf("rune=%-6d -> %-22s %.100s", r.runes, r.label, r.errTxt)
	}
	t.Logf("最大通过 rune=%d  最小被拒 rune=%d（0=该类未出现）", maxOK, minReject)
	t.Logf("解读：若「≤10000 通过、>10000 被 request_rejected(INPUT_VALIDATION_ERROR)」，")
	t.Logf("      则桌面端与 web 端同为 10000 rune 硬限；若超大长度仍全部通过，则桌面端无此限制（或更高）。")
}

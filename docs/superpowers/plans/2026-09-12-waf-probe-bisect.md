# WAF 检测 Phase B（在线探针二分）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 repro403 实验的七轮手工二分固化为面板「WAF 检测」页的后台探针 job：叶子轮逐个验证差异叶子，命中的叶子按行二分收敛到触发行，页面轮询展示进度与结论。

**Architecture:** 新增 `internal/api/waf_probe.go` 承载探针 job 管理器（内存态、单飞、可中止）与两个纯算法函数（行级二分、良性 padding）；发送器抽象成函数字段供测试注入假实现，生产实现经 `Server.Router.Provider.Chat` 直发（绕过 router：不占重试预算、不触发账号冷却、不写 request_logs）。provider 侧加 `ChatRequest.WafProbe` 旁路标志，让探针请求的出站 query 跳过 WAF 中和与长度截断（探针的存在意义就是原样复现可疑内容）。

**Tech Stack:** Go 1.22（`http.ServeMux` 方法+路径模式、`r.PathValue`）、SQLite（modernc.org/sqlite，复用 Phase A 的 store 查询）、原生 ES5 dashboard（无框架无构建）。

**Spec:** `docs/superpowers/specs/2026-09-11-waf-detection-design.md` §3（已批准）。

## Global Constraints

- 全部代码注释与用户可见文案用中文，风格对齐现有代码（注释讲「为什么」）。
- dashboard 是原生 ES5 单文件 `internal/dashboard/static/dashboard.js`：不用框架、不用构建步骤、不用 ES6+ 语法（`var`/`function`/字符串拼接）。
- 所有测试**不得打网络**：探针发送器经 `probeManager.newSender` 注入假实现；provider 侧旁路测试直接调 `buildBody`。
- Go 路由用 1.22 模式语法（先例：`mux.HandleFunc("DELETE /api/accounts/{id}", ...)`）。
- 提交信息风格对齐近期历史：短前缀（`provider:` / `api:` / `dashboard:` / `docs:`）+ 中文要点，结尾带 Co-Authored-By。
- 每个任务独立可绿：commit 前跑 `go vet ./... && go test ./...` 全量。

---

### Task 1: provider 探针旁路标志（跳过中和与截断）

**Files:**
- Modify: `internal/provider/types.go`（ChatRequest 尾部，OutputConfig 字段后）
- Modify: `internal/provider/request.go:36-46`
- Test: `internal/provider/waf_test.go`（文件尾追加）

**Interfaces:**
- Produces: `ChatRequest.WafProbe bool`（`json:"-"`）。Task 4 的生产发送器构造探针请求时置 true；其余代码路径不受影响。

**背景（写进代码注释的「为什么」）**：探针要复现 403 必须原样发出可疑内容。出站中和会把已知签名（如 `<script`）掐灭导致叶子轮假阴性；`capUpstreamQuery` 对超 10000 rune 的 query 掐头去尾，会把二分切片的「等长 padding」破坏掉（切片必须 pad 到与原叶子等长，这是 repro403 方法论：唯一变量是内容形状，不是长度）。

- [ ] **Step 1: 写失败测试**

在 `internal/provider/waf_test.go` 文件尾追加（文件已 import `encoding/json`、`strings`、`testing`）：

```go
// TestBuildBodyWafProbeBypass 钉住探针旁路：WafProbe 请求的出站 query 原样保留——
// 不中和（中和会掐灭待验证的已知特征，叶子轮必然假阴性）、不截断（二分切片必须
// padding 到与原叶子等长，截断破坏等长方法论）。非探针请求行为不变。
func TestBuildBodyWafProbeBypass(t *testing.T) {
	p := New()
	tokens := &Tokens{AccessToken: "tok", UserID: "u", WorkspaceID: "ws"}
	// 大于 10000 rune 验证截断旁路；含 <script> 验证中和旁路。
	long := "<script>alert(1)</script>" + strings.Repeat("x", 10100)

	probeReq := &ChatRequest{Model: "claude-haiku-4-5", WafProbe: true,
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, long)}}}
	probeQuery := p.buildBody(probeReq, tokens, "CLAUDE_HAIKU", 1)["input"].(map[string]interface{})["query"].(string)
	if probeQuery != long {
		t.Fatalf("probe query must be verbatim (no neutralize, no cap): got %d bytes, want %d", len(probeQuery), len(long))
	}

	normalReq := &ChatRequest{Model: "claude-haiku-4-5",
		Messages: []ChatMessage{{Role: "user", Content: rawText(t, long)}}}
	normalQuery := p.buildBody(normalReq, tokens, "CLAUDE_HAIKU", 1)["input"].(map[string]interface{})["query"].(string)
	if len(normalQuery) >= len(long) {
		t.Fatalf("normal request should stay capped, got %d bytes", len(normalQuery))
	}
	if n := WafSignatureHitCount(normalQuery); n != 0 {
		t.Fatalf("normal request should stay neutralized, got %d hits", n)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider -run TestBuildBodyWafProbeBypass -v`
Expected: FAIL（`WafProbe` 字段不存在，编译错误 `unknown field 'WafProbe'`）

- [ ] **Step 3: 最小实现**

`internal/provider/types.go` ChatRequest 结构体尾部（`OutputConfig` 字段之后）加：

```go
	// WafProbe 标记本请求来自面板「WAF 检测」的在线探针：出站 query 跳过 WAF 中和
	// 与 capUpstreamQuery 截断。探针的存在意义是原样复现可疑内容验证是否触发
	// Cloudflare 拦截——中和会掐灭已知特征（叶子轮假阴性），截断会破坏二分切片的
	// 等长 padding（方法论：唯一变量是内容形状，不是长度）。
	WafProbe bool `json:"-"`
```

`internal/provider/request.go` 把第 36-46 行的中和与 input 构造改为：

```go
	// 出站 query 统一过 WAF 中和（覆盖 tool-tail/折叠历史/普通 query 三条路径）：
	// 前端源码里的 HTML/JS 标记确定性触发 Cloudflare 403，在特征内插零宽空格破坏形态。
	// 先中和再 cap，保证中和后的长度仍受 10000 rune 上限约束。
	// 探针请求（req.WafProbe）两条都跳过：见 ChatRequest.WafProbe 注释。
	upstreamQuery := split.Query
	if wafNeutralizeEnabled() && !req.WafProbe {
		upstreamQuery = wafNeutralize(upstreamQuery)
	}
	query := capUpstreamQuery(upstreamQuery)
	if req.WafProbe {
		query = upstreamQuery
	}

	input := map[string]interface{}{
		"chatType": "USER_QUERY",
		"query":    query,
		"toolResponse": "",
		"useCase":      nil,
		"agent":        nil,
	}
```

（`input` 里只有 `"query"` 一行的值从 `capUpstreamQuery(upstreamQuery)` 换成 `query`，其余行原样保留。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider -run 'TestBuildBody|TestWaf' -v`
Expected: 全 PASS（含既有的 TestBuildBodyNeutralizesOutboundQuery、TestNativeToolResponseNeutralizesWrappedPayload——旁路只对 WafProbe=true 生效）

- [ ] **Step 5: 全量回归 + 提交**

```bash
go vet ./... && go test ./...
git add internal/provider/types.go internal/provider/request.go internal/provider/waf_test.go
git commit -m "provider: WafProbe request flag bypasses outbound neutralization and cap

探针请求必须原样复现可疑内容：中和会掐灭已知特征（叶子轮假阴性），
截断会破坏二分切片的等长 padding。只对显式置位的探针请求生效。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: api 层按路径取叶子全文（leafValue）

**Files:**
- Modify: `internal/api/waf.go`（文件尾追加，紧跟 truncateStr 之后）
- Test: `internal/api/waf_test.go`（文件尾追加）

**Interfaces:**
- Consumes: `leafString(v interface{}) string`、`leafDiff` 的路径格式（`"input.query"`、`"toolResponses[0].content.message"`）——Task 5 前已存在于 waf.go。
- Produces: `leafValue(body, path string) (string, bool)`；`parseLeafPath(p string) []leafSeg`。Task 4 的 POST 校验用它提取选中叶子的完整目标值（leafDiff 的 preview 截 500 字符，探针需要全文）。

- [ ] **Step 1: 写失败测试**

在 `internal/api/waf_test.go` 文件尾追加：

```go
// TestParseLeafPathAndLeafValue 钉住路径解析与叶子全文提取：对象键/数组下标/
// 混合路径、路径不存在、畸形路径、非 JSON body。
func TestParseLeafPathAndLeafValue(t *testing.T) {
	body := `{"input":{"query":"./bin/catpaw2api -config x"},"arr":["a","b"],"nested":[{"msg":"hello"}]}`
	cases := []struct{ path, want string }{
		{"input.query", "./bin/catpaw2api -config x"},
		{"arr[1]", "b"},
		{"nested[0].msg", "hello"},
		{"input", `{"query":"./bin/catpaw2api -config x"}`}, // 中间节点：leafString 走 json.Marshal
	}
	for _, c := range cases {
		got, ok := leafValue(body, c.path)
		if !ok || got != c.want {
			t.Fatalf("leafValue(%q) = %q, %v; want %q", c.path, got, ok, c.want)
		}
	}
	for _, bad := range []string{"input.missing", "arr[5]", "nested[0].gone", "arr[x]", "input["} {
		if got, ok := leafValue(body, bad); ok {
			t.Fatalf("leafValue(%q) should miss, got %q", bad, got)
		}
	}
	if _, ok := leafValue("not-json{", "input.query"); ok {
		t.Fatal("malformed body should miss")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api -run TestParseLeafPathAndLeafValue -v`
Expected: FAIL（`undefined: leafValue`）

- [ ] **Step 3: 最小实现**

在 `internal/api/waf.go` 文件尾追加（waf.go 已 import `encoding/json`、`strconv`、`strings`——`strings` 若未 import 需补）：

```go
// ── 探针支撑：按路径取叶子全文 ──────────────────────────────

// leafSeg 是 leafDiff 路径的一段：对象键或数组下标。
type leafSeg struct {
	key     string
	index   int
	isIndex bool
}

// parseLeafPath 把 leafDiff 产出的路径（"input.query"、"toolResponses[0].content.message"）
// 解析成段序列，供 leafValue 定位叶子。畸形路径截断到已解析部分（调用方会因取不到值而失败）。
func parseLeafPath(p string) []leafSeg {
	var segs []leafSeg
	var key strings.Builder
	flush := func() {
		if key.Len() > 0 {
			segs = append(segs, leafSeg{key: key.String()})
			key.Reset()
		}
	}
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '.':
			flush()
		case '[':
			flush()
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return segs
			}
			n, err := strconv.Atoi(strings.TrimSpace(p[i+1 : i+j]))
			if err != nil {
				return segs
			}
			segs = append(segs, leafSeg{index: n, isIndex: true})
			i += j
		default:
			key.WriteByte(p[i])
		}
	}
	flush()
	return segs
}

// leafValue 返回 JSON 出站体中该路径叶子的完整字符串值（leafDiff 的 preview 截 500
// 字符，在线探针需要全文来构造等长变体）。路径不存在或 body 非法 JSON 时 ok=false。
func leafValue(body, path string) (string, bool) {
	var v interface{}
	if json.Unmarshal([]byte(body), &v) != nil {
		return "", false
	}
	for _, seg := range parseLeafPath(path) {
		if seg.isIndex {
			arr, ok := v.([]interface{})
			if !ok || seg.index < 0 || seg.index >= len(arr) {
				return "", false
			}
			v = arr[seg.index]
			continue
		}
		m, ok := v.(map[string]interface{})
		if !ok {
			return "", false
		}
		nv, ok := m[seg.key]
		if !ok {
			return "", false
		}
		v = nv
	}
	return leafString(v), true
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api -v`
Expected: 全 PASS

- [ ] **Step 5: 提交**

```bash
go vet ./... && go test ./...
git add internal/api/waf.go internal/api/waf_test.go
git commit -m "api: extract full leaf value by diff path (probe groundwork)

leafDiff 的 preview 截 500 字符，在线探针构造等长变体需要叶子全文。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: 探针纯算法（padding / 归类 / 行级二分）

**Files:**
- Create: `internal/api/waf_probe.go`
- Test: `internal/api/waf_probe_test.go`

**Interfaces:**
- Consumes: `provider.Result`（字段 `Success`、`GatewayBlocked`、`RequestBytes`、`Error`、`RejectionDetail`）。
- Produces（Task 4 依赖）: `probePad(s string, targetBytes int) string`；`type probeOutcome struct{ Label, Ray string; Bytes int; Detail string }`；`classifyProbeOutcome(res *provider.Result) probeOutcome`；`bisectDescend(n int, hit func(lo, hi int) bool) (lo, hi int, ambiguous bool, rounds int)`。

- [ ] **Step 1: 写失败测试**

创建 `internal/api/waf_probe_test.go`：

```go
package api

import (
	"strings"
	"testing"

	"ps2api/internal/provider"
)

// TestProbePad 钉住等长 padding：不足补良性散文、已达标原样返回、绝不截断
// （截断会切掉待验证特征）。
func TestProbePad(t *testing.T) {
	short := probePad("abc", 200)
	if len(short) != 200 {
		t.Fatalf("probePad should pad to 200, got %d", len(short))
	}
	if !strings.HasPrefix(short, "abc") {
		t.Fatalf("padding must preserve original prefix, got %q", short[:10])
	}
	exact := probePad(strings.Repeat("x", 300), 200)
	if exact != strings.Repeat("x", 300) {
		t.Fatal("probePad must never truncate")
	}
}

// TestClassifyProbeOutcome 归类口径同 repro403 实验的 classify：成功=pass、
// 网关拦截=403、其余=other；Cf-Ray 从 RejectionDetail 行提取。
func TestClassifyProbeOutcome(t *testing.T) {
	if o := classifyProbeOutcome(&provider.Result{Success: true, RequestBytes: 42}); o.Label != "pass" || o.Bytes != 42 {
		t.Fatalf("success should be pass, got %+v", o)
	}
	o := classifyProbeOutcome(&provider.Result{GatewayBlocked: true, RequestBytes: 10,
		Error: "(403, Cloudflare)",
		RejectionDetail: "HTTP 状态: 403\nCf-Ray: 8b2c1d3e4f5a6b7c-SJC\n出站请求体: 10 字节"})
	if o.Label != "403" || o.Ray != "8b2c1d3e4f5a6b7c-SJC" {
		t.Fatalf("gateway blocked misclassified: %+v", o)
	}
	if o := classifyProbeOutcome(&provider.Result{Error: "boom"}); o.Label != "other" || o.Detail != "boom" {
		t.Fatalf("other misclassified: %+v", o)
	}
	if o := classifyProbeOutcome(nil); o.Label != "other" {
		t.Fatalf("nil should be other: %+v", o)
	}
}

// TestBisectDescend 行级二分收敛：左命中向左收、右命中向右收、两半都放行=歧义、
// 单行区间直接终止。
func TestBisectDescend(t *testing.T) {
	// 6 行，第 4 行（下标 3）触发：前几轮左半放行、右半命中后向左收
	lo, hi, amb, rounds := bisectDescend(6, func(a, b int) bool { return a <= 3 && 3 < b })
	if lo != 3 || hi != 4 || amb || rounds == 0 {
		t.Fatalf("want [3,4) not ambiguous, got [%d,%d) amb=%v rounds=%d", lo, hi, amb, rounds)
	}
	// 第 0 行触发：一直向左收
	lo, hi, amb, _ = bisectDescend(8, func(a, b int) bool { return a == 0 })
	if lo != 0 || hi != 1 || amb {
		t.Fatalf("want [0,1), got [%d,%d) amb=%v", lo, hi, amb)
	}
	// 全放行：歧义
	_, _, amb, _ = bisectDescend(4, func(a, b int) bool { return false })
	if !amb {
		t.Fatal("all-pass halves should be ambiguous")
	}
	// 单行：不测试直接返回
	lo, hi, amb, rounds = bisectDescend(1, func(a, b int) bool { t.Fatal("single line must not be tested"); return false })
	if lo != 0 || hi != 1 || amb || rounds != 0 {
		t.Fatalf("single line: got [%d,%d) amb=%v rounds=%d", lo, hi, amb, rounds)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api -run 'TestProbePad|TestClassifyProbeOutcome|TestBisectDescend' -v`
Expected: FAIL（`undefined: probePad`）

- [ ] **Step 3: 最小实现**

创建 `internal/api/waf_probe.go`：

```go
// waf_probe.go —— 面板「WAF 检测」的在线探针二分：把 repro403 实验的七轮手工二分
// 固化为后台 job。叶子轮逐个验证差异叶子，命中的叶子按行二分收敛到触发行。
// 铁律：显式人工发起（页面确认弹窗）、同一时刻仅一个 job、不持久化（重启即丢）、
// 不走 router（不占重试预算、不触发账号冷却、不写 request_logs）。
package api

import (
	"context"
	"strings"

	"ps2api/internal/provider"
)

// probePad 用良性散文把 s 填充到约 targetBytes 字节，保证各变体长度可比（方法论同
// repro403 实验的 padTo：唯一变量是内容形状，不是长度）。填充不含标记特征。
// 不做截断：探针内容必须完整，截断会切掉待验证特征。
func probePad(s string, targetBytes int) string {
	const filler = " The quick brown fox jumps over the lazy dog while nearby a calm river flows past green fields."
	var b strings.Builder
	b.WriteString(s)
	for b.Len() < targetBytes {
		b.WriteString(filler)
	}
	return b.String()
}

// probeOutcome 是一次探针请求的归类结果。
type probeOutcome struct {
	Label  string // pass | 403 | other
	Ray    string
	Bytes  int
	Detail string
}

// classifyProbeOutcome 把 provider.Result 归类（口径同 repro403 实验的 classify），
// 并从 RejectionDetail 的行里抽 Cf-Ray 供证据表展示。
func classifyProbeOutcome(res *provider.Result) probeOutcome {
	if res == nil {
		return probeOutcome{Label: "other", Detail: "nil result"}
	}
	out := probeOutcome{Bytes: res.RequestBytes, Detail: oneLineProbe(res.Error)}
	switch {
	case res.Success:
		out.Label = "pass"
	case res.GatewayBlocked:
		out.Label = "403"
	default:
		out.Label = "other"
	}
	for _, ln := range strings.Split(res.RejectionDetail, "\n") {
		if ray, ok := strings.CutPrefix(strings.TrimSpace(ln), "Cf-Ray:"); ok {
			out.Ray = strings.TrimSpace(ray)
		}
	}
	return out
}

func oneLineProbe(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// bisectDescend 在 n 行上做二分收敛：hit(lo,hi) 报告 lines[lo:hi)（padding 到等长后
// 发出）是否触发 403。先测左半，命中向左收；左半放行再测右半，命中向右收；两半都
// 放行 → 歧义（触发是跨半的组合特征），终止。区间收敛到单行即结论。
func bisectDescend(n int, hit func(lo, hi int) bool) (lo, hi int, ambiguous bool, rounds int) {
	lo, hi = 0, n
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		rounds++
		if hit(lo, mid) {
			hi = mid
			continue
		}
		if hit(mid, hi) {
			lo = mid
			continue
		}
		return lo, hi, true, rounds
	}
	return lo, hi, false, rounds
}

// 供 Go 编译器确认 context 会被后续任务使用而保留的占位引用将在 Task 4 删除。
var _ = context.Background
```

注意：Task 3 阶段 `context` 尚无使用方，用文件尾 `var _ = context.Background` 占位避免 unused import；**Task 4 会删掉这行**。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api -run 'TestProbePad|TestClassifyProbeOutcome|TestBisectDescend' -v`
Expected: 全 PASS

- [ ] **Step 5: 提交**

```bash
go vet ./... && go test ./...
git add internal/api/waf_probe.go internal/api/waf_probe_test.go
git commit -m "api: probe primitives (equal-length padding, outcome classification, line bisect)

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: 探针 job 管理器与端点（POST/GET/DELETE）

**Files:**
- Modify: `internal/api/waf_probe.go`（追加 job 管理器、runner、三个 handler；删除 context 占位行）
- Modify: `internal/api/api.go`（Server 结构体加 probe 字段；New 初始化；Register 注册路由；包注释补一行）
- Test: `internal/api/waf_probe_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 `ChatRequest.WafProbe`；Task 2 `leafValue`、`leafDiff`、`LeafDiff`；Task 3 `probePad`、`probeOutcome`、`classifyProbeOutcome`、`bisectDescend`；store 的 `GetRequestLog`、`GetAccount`、`ActiveAccounts`；`s.Router.Provider.Chat(ctx, acc, req)`。
- Produces: `Server.probe`（`probeManager`，字段 `newSender func(acc *store.Account, model string) probeSender` 可被测试覆写）；`type probeSender func(ctx context.Context, text string) probeOutcome`；HTTP 契约：
  - `POST /api/waf/probe` `{log_id, baseline_id, paths[], account_id?, model?}` → `200 {job_id}`；运行中重复发起 → 409。
  - `GET /api/waf/probe/{job_id}` → job 快照 `{id, status(running|done|aborted|error), phase(control|leaf|bisect), logId, baselineId, account, model, variants[{name,outcome(pass|403|other),rays[],bytes,detail}], hits[], conclusions[{path,lines[],ambiguous,rounds}], summary}`；未知 id → 404。
  - `DELETE /api/waf/probe/{job_id}` → 中止运行中的 job。

- [ ] **Step 1: 写失败测试**

在 `internal/api/waf_probe_test.go` 追加（同时给文件补 import：`context`、`fmt`、`net/http`、`net/http/httptest`、`path/filepath`、`time`、`ps2api/internal/store`）：

```go
// newFakeProbeTestServer 构造带假发送器的测试服务：blocked(text) 决定一条探针文本
// 是否「触发 403」，其余一律 pass。零网络。
func newFakeProbeTestServer(t *testing.T, blocked func(text string) bool) (*http.ServeMux, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var acc int64 = 7
	st.LogRequest(&store.RequestLog{Status: "success", AccountID: &acc, RequestBytes: 10,
		UpstreamBody: `{"input":{"query":"clean"}}`, ConversationID: "c1", CreatedAt: time.Now()})
	st.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)", AccountID: &acc,
		RequestBytes: 20, UpstreamBody: `{"input":{"query":"padding one\n./bin/catpaw2api -config config.json\npadding two\npadding three\npadding four\npadding five\npadding six"}}`,
		ConversationID: "c1", CreatedAt: time.Now()})
	s := New(st)
	s.probe.newSender = func(acc *store.Account, model string) probeSender {
		return func(ctx context.Context, text string) probeOutcome {
			if blocked(text) {
				return probeOutcome{Label: "403", Ray: "fake-ray", Bytes: len(text)}
			}
			return probeOutcome{Label: "pass", Bytes: len(text)}
		}
	}
	mux := http.NewServeMux()
	s.Register(mux)
	return mux, s
}

// waitProbeDone 轮询 GET 直到 job 结束（假发送器零延迟，5s 上限足够）。
func waitProbeDone(t *testing.T, mux *http.ServeMux, jobID string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/probe/"+jobID, nil))
		if rec.Code != 200 {
			t.Fatalf("probe status GET = %d: %s", rec.Code, rec.Body.String())
		}
		var j map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j["status"] != "running" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("probe job did not finish in 5s")
	return nil
}

func postProbe(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/waf/probe", strings.NewReader(body)))
	return rec
}

// TestWafProbeBisectEndToEnd 端到端（假发送器）：叶子轮命中 → 行级二分收敛到
// bin/cat 行；对照变体放行；结论与证据齐全。
func TestWafProbeBisectEndToEnd(t *testing.T) {
	probePause = 0 // 测试不发真实请求，退避归零
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, _ := newFakeProbeTestServer(t, func(text string) bool { return strings.Contains(text, "bin/cat") })

	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "done" {
		t.Fatalf("status = %v, summary = %v", j["status"], j["summary"])
	}
	// 命中叶子与收敛行
	hits := j["hits"].([]interface{})
	if len(hits) != 1 || hits[0] != "input.query" {
		t.Fatalf("hits = %v", hits)
	}
	cs := j["conclusions"].([]interface{})
	if len(cs) != 1 {
		t.Fatalf("conclusions = %v", cs)
	}
	c := cs[0].(map[string]interface{})
	lines := c["lines"].([]interface{})
	if len(lines) != 1 || lines[0] != "./bin/catpaw2api -config config.json" {
		t.Fatalf("converged lines = %v", lines)
	}
	if c["ambiguous"] != false {
		t.Fatalf("should not be ambiguous: %v", c)
	}
	// 变体序列：对照在前且 pass；叶子轮 403；二分轮 ≥1
	variants := j["variants"].([]interface{})
	if len(variants) < 3 {
		t.Fatalf("variants too few: %v", variants)
	}
	if variants[0].(map[string]interface{})["outcome"] != "pass" {
		t.Fatalf("control variant should pass: %v", variants[0])
	}
	saw403 := false
	for _, v := range variants[1:] {
		if v.(map[string]interface{})["outcome"] == "403" {
			saw403 = true
		}
	}
	if !saw403 {
		t.Fatalf("no 403 variant recorded: %v", variants)
	}
}

// TestWafProbeAllPass 全放行：结论提示组合特征/换对照（spec 原文案）。
func TestWafProbeAllPass(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, _ := newFakeProbeTestServer(t, func(string) bool { return false })
	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var started struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "done" || !strings.Contains(fmt.Sprint(j["summary"]), "组合特征") {
		t.Fatalf("all-pass summary wrong: %v", j["summary"])
	}
}

// TestWafProbeControlBlocked 对照被拦：中止并提示风控窗口。
func TestWafProbeControlBlocked(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, _ := newFakeProbeTestServer(t, func(string) bool { return true })
	rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`)
	var started struct {
		JobID string `json:"job_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &started)
	j := waitProbeDone(t, mux, started.JobID)
	if j["status"] != "aborted" || !strings.Contains(fmt.Sprint(j["summary"]), "风控窗口") {
		t.Fatalf("control-blocked should abort with hint: %v / %v", j["status"], j["summary"])
	}
}

// TestWafProbeValidation 校验分支：未知路径 400、单飞 409、缺参 400、日志不存在 404。
func TestWafProbeValidation(t *testing.T) {
	probePause = 0
	t.Cleanup(func() { probePause = 1500 * time.Millisecond })
	mux, s := newFakeProbeTestServer(t, func(string) bool { return false })

	if rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.absent"]}`); rec.Code != 400 {
		t.Fatalf("unknown path should 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := postProbe(t, mux, `{"log_id":0,"baseline_id":1,"paths":["input.query"]}`); rec.Code != 400 {
		t.Fatalf("bad log_id should 400, got %d", rec.Code)
	}
	if rec := postProbe(t, mux, `{"log_id":999,"baseline_id":1,"paths":["input.query"]}`); rec.Code != 404 {
		t.Fatalf("missing log should 404, got %d", rec.Code)
	}
	// 单飞：手工塞一个 running job（无需真跑）
	s.probe.mu.Lock()
	s.probe.current = &probeJob{id: "probe-x", status: "running"}
	s.probe.mu.Unlock()
	if rec := postProbe(t, mux, `{"log_id":2,"baseline_id":1,"paths":["input.query"]}`); rec.Code != 409 {
		t.Fatalf("second job while running should 409, got %d: %s", rec.Code, rec.Body.String())
	}
	// 未知 job id
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/probe/nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown job should 404, got %d", rec.Code)
	}
}
```

同时给 waf_probe_test.go 的 import 块补 `encoding/json`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api -run 'TestWafProbe' -v`
Expected: FAIL（`s.probe` / `probeJob` / `probeSender` 未定义）

- [ ] **Step 3: 实现**

**3a.** `internal/api/api.go`：Server 结构体加字段、New 初始化、Register 注册路由、包注释补一行：

```go
type Server struct {
	Store  *store.Store
	Router *router.Router
	Vision *provider.MediaResolver
	// probe 是「WAF 检测」在线探针的 job 管理器（内存态、单飞，见 waf_probe.go）。
	probe probeManager
}
```

New 里 `srv := &Server{...}` 之后加：

```go
	// 在线探针发送器：经共享 Provider 直发（出口配置与业务流量一致；绕过 router，
	// 不占重试预算、不触发账号冷却、不写 request_logs）。测试覆写 newSender 注入假实现。
	srv.probe.newSender = func(acc *store.Account, model string) probeSender {
		return func(ctx context.Context, text string) probeOutcome {
			content, _ := json.Marshal(text)
			// 不 ResetConversation：探针消息带唯一 nonce，指纹必然未命中 → 冷启动
			// USER_QUERY；Reset 会清掉该账号全部业务会话映射，干扰线上续聊。
			req := &provider.ChatRequest{
				Model:    model,
				Messages: []provider.ChatMessage{{Role: "user", Content: content}},
				WafProbe: true, // 原样出站：不中和（掐灭待验证特征）、不截断（破坏等长 padding）
			}
			cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			return classifyProbeOutcome(srv.Router.Provider.Chat(cctx, acc, req))
		}
	}
```

（api.go 需补 import：`context`、`time`；`encoding/json` 若未 import 需补。）

Register 的 WAF 区块加：

```go
	mux.HandleFunc("POST /api/waf/probe", s.wafProbeStart)
	mux.HandleFunc("GET /api/waf/probe/{job_id}", s.wafProbeStatus)
	mux.HandleFunc("DELETE /api/waf/probe/{job_id}", s.wafProbeAbort)
```

包注释文件清单加一行（waf.go 行后）：

```go
//   - waf_probe.go  WAF 在线探针二分（后台 job：发起/进度/中止）
```

**3b.** `internal/api/waf_probe.go` 追加（并删除文件尾的 `var _ = context.Background` 占位行、追加相应 import：`encoding/json`、`fmt`、`net/http`、`sync`、`time`、`ps2api/internal/store`）：

```go
// ── 探针 job ───────────────────────────────────────────────

// probeSender 发一次探针请求并归类。抽象成函数类型：生产实现经 Router.Provider
// 直发（见 api.go New），测试注入假发送器（零网络）。
type probeSender func(ctx context.Context, text string) probeOutcome

// probeReps 每变体重复次数：403 内容签名实测为确定性拦截，2 次足够确认。
const probeReps = 2

// probePause 相邻两次探针请求的退避，避免把结果污染成纯速率限制（同 repro403 实验）。
// var 而非 const：测试归零加速。
var probePause = 1500 * time.Millisecond

// probePrefix 是每个探针变体的固定前缀：给模型一个自然语言指令 + 唯一 nonce，
// 与 repro403 实验的变体构造完全一致。
func probePrefix(nonce string) string {
	return "Check this content. nonce=" + nonce + ". "
}

// probeVariantResult 是一个已完成变体（probeReps 次重复的归并结果）。
type probeVariantResult struct {
	Name    string   `json:"name"`
	Outcome string   `json:"outcome"` // pass | 403 | other
	Rays    []string `json:"rays,omitempty"`
	Bytes   int      `json:"bytes"`
	Detail  string   `json:"detail,omitempty"`
}

// probeConclusion 是一个命中叶子的二分收敛结论。
type probeConclusion struct {
	Path      string   `json:"path"`
	Lines     []string `json:"lines"`
	Ambiguous bool     `json:"ambiguous"`
	Rounds    int      `json:"rounds"`
}

// probeJob 是一个探针 job 的全部状态（内存态，不持久化，服务重启即丢）。
type probeJob struct {
	mu          sync.Mutex
	id          string
	status      string // running | done | aborted | error
	phase       string // control | leaf | bisect
	logID       int64
	baselineID  int64
	account     string
	model       string
	variants    []probeVariantResult
	hits        []string
	conclusions []probeConclusion
	summary     string
	cancel      context.CancelFunc
}

func (j *probeJob) setPhase(p string) {
	j.mu.Lock()
	j.phase = p
	j.mu.Unlock()
}

func (j *probeJob) finish(status, summary string) {
	j.mu.Lock()
	j.status = status
	if summary != "" {
		j.summary = summary
	}
	j.mu.Unlock()
}

// snapshot 在锁内导出 JSON 友好视图（GET /api/waf/probe/{id} 的响应体）。
func (j *probeJob) snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	variants := make([]probeVariantResult, len(j.variants))
	copy(variants, j.variants)
	hits := make([]string, len(j.hits))
	copy(hits, j.hits)
	conclusions := make([]probeConclusion, len(j.conclusions))
	copy(conclusions, j.conclusions)
	return map[string]interface{}{
		"id": j.id, "status": j.status, "phase": j.phase,
		"logId": j.logID, "baselineId": j.baselineID,
		"account": j.account, "model": j.model,
		"variants": variants, "hits": hits, "conclusions": conclusions,
		"summary": j.summary,
	}
}

// probeManager 管理当前探针 job（单飞：同一时刻仅一个）。
type probeManager struct {
	mu      sync.Mutex
	current *probeJob
	// newSender 产出绑定账号与模型的发送器；测试覆写注入假实现。
	newSender func(acc *store.Account, model string) probeSender
}

// probeLeaf 是 POST 校验后提取的一个待测叶子：路径 + 目标体全文。
type probeLeaf struct {
	path  string
	value string
}

// wafProbeStart 发起探针 job：校验参数 → 提取叶子全文 → 单飞检查 → 后台运行。
func (s *Server) wafProbeStart(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	var body struct {
		LogID      int64    `json:"log_id"`
		BaselineID int64    `json:"baseline_id"`
		Paths      []string `json:"paths"`
		AccountID  int64    `json:"account_id"`
		Model      string   `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, 400, "请求体不是合法 JSON: "+err.Error(), "invalid_request_error")
		return
	}
	if body.LogID <= 0 || body.BaselineID <= 0 {
		jsonError(w, 400, "log_id 与 baseline_id 必须为正整数", "invalid_request_error")
		return
	}
	if len(body.Paths) == 0 {
		jsonError(w, 400, "paths 不能为空（至少勾选一个差异叶子）", "invalid_request_error")
		return
	}
	if body.Model == "" {
		body.Model = "claude-haiku-4-5"
	}
	target, err := s.Store.GetRequestLog(body.LogID)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	if target == nil {
		jsonError(w, 404, "日志不存在", "invalid_request_error")
		return
	}
	baseline, err := s.Store.GetRequestLog(body.BaselineID)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	if baseline == nil {
		jsonError(w, 404, "对照日志不存在", "invalid_request_error")
		return
	}
	// 重算 diff 校验路径（同时取全文：leafDiff 的 preview 截 500 字符，探针要全文）。
	byPath := map[string]LeafDiff{}
	for _, d := range leafDiff(baseline.UpstreamBody, target.UpstreamBody) {
		byPath[d.Path] = d
	}
	leaves := make([]probeLeaf, 0, len(body.Paths))
	for _, p := range body.Paths {
		d, ok := byPath[p]
		if !ok {
			jsonError(w, 400, "路径不在差异列表中: "+p, "invalid_request_error")
			return
		}
		if d.Kind == "removed" {
			jsonError(w, 400, "removed 叶子没有发送到上游的内容，不可探测: "+p, "invalid_request_error")
			return
		}
		v, ok := leafValue(target.UpstreamBody, p)
		if !ok {
			jsonError(w, 400, "无法从目标出站体提取叶子全文（JSON 解析失败的分析不支持探针）: "+p, "invalid_request_error")
			return
		}
		leaves = append(leaves, probeLeaf{path: p, value: v})
	}
	// 账号：显式指定或首个活跃号。不走 pool（探针不占预留/在飞统计）。
	var acc *store.Account
	if body.AccountID > 0 {
		acc, err = s.Store.GetAccount(body.AccountID)
		if err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
		if acc == nil {
			jsonError(w, 404, "账号不存在", "invalid_request_error")
			return
		}
	} else {
		accs, err := s.Store.ActiveAccounts()
		if err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
		if len(accs) == 0 {
			jsonError(w, 400, "没有活跃账号，无法发起探针", "invalid_request_error")
			return
		}
		acc = accs[0]
	}
	// 单飞：运行中直接拒绝。
	s.probe.mu.Lock()
	if s.probe.current != nil && s.probe.current.status == "running" {
		s.probe.mu.Unlock()
		jsonError(w, 409, "已有探针 job 在运行，请等它结束或先中止", "invalid_request_error")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &probeJob{
		id: fmt.Sprintf("probe-%d", time.Now().UnixNano()), status: "running", phase: "control",
		logID: body.LogID, baselineID: body.BaselineID,
		account: acc.Email, model: body.Model, cancel: cancel,
	}
	s.probe.current = j
	s.probe.mu.Unlock()

	go s.runProbeJob(ctx, j, leaves, s.probe.newSender(acc, body.Model))
	jsonWrite(w, 200, map[string]interface{}{"job_id": j.id})
}

// wafProbeStatus 返回 job 快照（轮询端点）。job 不持久化：重启后 404。
func (s *Server) wafProbeStatus(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	s.probe.mu.Lock()
	j := s.probe.current
	s.probe.mu.Unlock()
	if j == nil || j.id != r.PathValue("job_id") {
		jsonError(w, 404, "探针 job 不存在（可能服务已重启）", "invalid_request_error")
		return
	}
	jsonWrite(w, 200, j.snapshot())
}

// wafProbeAbort 中止运行中的 job（发送器经 ctx 传播取消；已结束的 job 是 no-op）。
func (s *Server) wafProbeAbort(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	s.probe.mu.Lock()
	j := s.probe.current
	s.probe.mu.Unlock()
	if j == nil || j.id != r.PathValue("job_id") {
		jsonError(w, 404, "探针 job 不存在", "invalid_request_error")
		return
	}
	j.mu.Lock()
	running := j.status == "running"
	j.mu.Unlock()
	if running {
		j.cancel()
	}
	jsonWrite(w, 200, map[string]interface{}{"status": "aborted"})
}

// runProbeJob 是探针主流程：对照 → 叶子轮 → 行级二分。任何一步出错置 error 并终止。
func (s *Server) runProbeJob(ctx context.Context, j *probeJob, leaves []probeLeaf, send probeSender) {
	defer func() {
		if err := recover(); err != nil {
			j.finish("error", fmt.Sprintf("探针内部错误: %v", err))
		}
	}()
	// trySend 发一个变体（probeReps 次重复），归并结果并记录。返回是否命中 403。
	// 403 内容签名是确定性拦截：任一次 403 即命中；偶发风控型 403 会先在对照变体暴露。
	trySend := func(name, text string) (hit bool) {
		v := probeVariantResult{Name: name, Bytes: len(text)}
		blocked, passed, other := 0, 0, 0
		for i := 0; i < probeReps; i++ {
			select {
			case <-ctx.Done():
				j.finish("aborted", "探针已中止")
				return false
			case <-time.After(probePause):
			}
			out := send(ctx, text)
			if out.Ray != "" {
				v.Rays = append(v.Rays, out.Ray)
			}
			if out.Bytes > 0 {
				v.Bytes = out.Bytes
			}
			if out.Detail != "" {
				v.Detail = out.Detail
			}
			switch out.Label {
			case "403":
				blocked++
			case "pass":
				passed++
			default:
				other++
			}
		}
		switch {
		case blocked > 0:
			v.Outcome = "403"
		case other > 0:
			v.Outcome = "other"
		default:
			v.Outcome = "pass"
		}
		j.mu.Lock()
		j.variants = append(j.variants, v)
		j.mu.Unlock()
		return blocked > 0
	}
	// nonce 序号：每个变体唯一（会话隔离由指纹天然冷启动保证，见 New 的发送器注释）。
	seq := 0
	nextNonce := func() string {
		seq++
		return fmt.Sprintf("%s-%d-%d", j.id, seq, time.Now().UnixNano())
	}

	// 1) 对照变体：等长纯文本（repro403 实验 A_plain_prose 的角色）。被拦说明
	//    当前账号/出口在风控窗口，后续结果全部不可信，直接中止。
	j.setPhase("control")
	maxLen := 0
	for _, l := range leaves {
		if n := len(probePrefix(nextNonce()) + l.value); n > maxLen {
			maxLen = n
		}
	}
	if _, hit := trySend("对照(等长纯文本)", probePad("Please summarize this note in one short sentence. nonce="+nextNonce()+". ", maxLen)); hit {
		j.finish("aborted", "对照变体（等长纯文本）也被 403——当前账号/出口处于风控窗口，探针结果不可信。请稍后重试或换账号。")
		return
	}

	// 2) 叶子轮：每个勾选叶子单独成变体（基于对照，只注入该叶子内容）。
	j.setPhase("leaf")
	var hitLeaves []probeLeaf
	for _, l := range leaves {
		text := probePad(probePrefix(nextNonce())+l.value, len(probePrefix(nextNonce())+l.value))
		if trySend("叶子 "+l.path, text) {
			hitLeaves = append(hitLeaves, l)
			j.mu.Lock()
			j.hits = append(j.hits, l.path)
			j.mu.Unlock()
		}
	}
	if len(hitLeaves) == 0 {
		j.finish("done", "全部叶子放行——触发可能是组合特征或对照本身已含特征，建议换对照或转手工（repro403 实验）。")
		return
	}

	// 3) 行级二分：对每个命中叶子按行二分，~log2(行数) 轮收敛到触发行。
	j.setPhase("bisect")
	for _, l := range hitLeaves {
		lines := strings.Split(l.value, "\n")
		if len(lines) == 1 {
			// 整叶触发且只有一行：无需二分，这行就是结论。
			j.mu.Lock()
			j.conclusions = append(j.conclusions, probeConclusion{Path: l.path, Lines: lines})
			j.mu.Unlock()
			continue
		}
		target := len(probePrefix(nextNonce()) + l.value)
		lo, hi, amb, rounds := bisectDescend(len(lines), func(a, b int) bool {
			slice := strings.Join(lines[a:b], "\n")
			name := fmt.Sprintf("二分 %s 行[%d:%d]", l.path, a, b)
			return trySend(name, probePad(probePrefix(nextNonce())+slice, target))
		})
		j.mu.Lock()
		j.conclusions = append(j.conclusions, probeConclusion{
			Path: l.path, Lines: lines[lo:hi], Ambiguous: amb, Rounds: rounds,
		})
		j.mu.Unlock()
	}
	j.finish("done", "探针完成：命中 "+fmt.Sprint(len(hitLeaves))+" 个叶子，结论见触发行列表。")
}
```

注意两处实现细节：
- `trySend` 的退避用 `select { case <-ctx.Done(): ... case <-time.After(probePause): }` —— 中止能立刻打断退避等待。
- 单行叶子跳过二分直接出结论（`bisectDescend(1, ...)` 语义上也是 no-op，但省两次冗余请求）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api -v`
Expected: 全 PASS（含既有的 TestWafSignaturesEndpoint 等）

- [ ] **Step 5: 全量回归 + 提交**

```bash
go vet ./... && go test ./...
git add internal/api/waf_probe.go internal/api/waf_probe_test.go internal/api/api.go
git commit -m "api: WAF probe job manager and endpoints (start/status/abort)

后台 job 单飞、可中止、不持久化；绕过 router 不占重试预算/冷却/日志。
发送器抽象成函数字段，测试注入假发送器零网络验证二分收敛。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: 面板前端（勾选叶子 + 发起探针 + 轮询展示）

**Files:**
- Modify: `internal/dashboard/static/fragments/page-waf.html`
- Modify: `internal/dashboard/static/dashboard.js`

**Interfaces:**
- Consumes: Task 4 的 HTTP 契约（POST/GET/DELETE `/api/waf/probe*` 与 job 快照 JSON：`status/phase/variants[{name,outcome,rays,bytes}]/conclusions[{path,lines,ambiguous,rounds}]/summary/account/model`）；既有 `state.waf.analysis.diff[{path,kind}]`、`api()`/`esc()`/`fmt()`/`toast()` helper。
- Produces: 全局函数 `wafProbe`/`wafToggleLeaf`/`wafProbeAbort`；`state.waf.probePaths`（与 diff 数组对齐的布尔数组）。

- [ ] **Step 1: page-waf.html 加探针面板与发起按钮**

在分析结果卡片（`<div class="card p-6" id="wafAnalysis">`）**之前**的头部行之后、`</div>` 结束分析卡片之前不动；做两处修改：

（a）分析卡片头部的对照下拉之后追加发起按钮（`<select id="wafBaselineSel" ...></select>` 同行之后）：

```html
            <button class="btn btn-ghost text-[12px]" id="wafProbeBtn" onclick="wafProbe()" style="display:none;">发起探针</button>
```

（b）分析卡片 `</div>` 结束之后、`</div><!-- p-8 -->` 之前追加探针面板：

```html
      <div class="card p-6 mt-4" id="wafProbePanel" style="display:none;">
        <div class="flex items-center justify-between mb-4 flex-wrap gap-3">
          <h3 class="font-display text-[16px] font-medium">在线探针 <span class="font-mono text-[13px]" id="wafProbeMeta" style="color: var(--muted);"></span></h3>
          <div class="flex items-center gap-2">
            <input id="wafProbeAccount" class="input" style="width:110px;" placeholder="账号ID(可选)">
            <input id="wafProbeModel" class="input" style="width:170px;" value="claude-haiku-4-5" placeholder="模型">
            <button class="btn btn-ghost text-[12px]" onclick="wafProbeAbort()">中止</button>
          </div>
        </div>
        <div id="wafProbeBody"></div>
      </div>
```

- [ ] **Step 2: dashboard.js 接线 state 与探针函数**

（a）`state.waf` 初始化行改为：

```js
    waf: { list: [], page: 1, total: 0, currentId: 0, baselineId: '', analysis: null, probePaths: [], probeJob: '', probeTimer: null },
```

（b）`wafRunAnalyze` 成功回调里（`state.waf.analysis = data;` 之后）重置勾选为全选（removed 叶子不可探测，禁用）：

```js
      state.waf.probePaths = (data.diff || []).map(function (x) { return x.kind !== 'removed'; });
```

（c）`renderWafAnalysis` 中 diff 渲染的 `<details>` 行加勾选框并控制按钮可见性。把现有：

```js
      html += diffs.map(function (x) {
        return '<details class="card p-3 mb-2"><summary class="cursor-pointer font-mono text-[12px]">' +
```

改为（用下标引用勾选状态，避免路径做 JS 字符串注入）：

```js
      html += diffs.map(function (x, i) {
        var ck = x.kind === 'removed'
          ? '<input type="checkbox" disabled> '
          : '<input type="checkbox" ' + (state.waf.probePaths[i] ? 'checked' : '') + ' onchange="wafToggleLeaf(' + i + ', this.checked)" onclick="event.stopPropagation()"> ';
        return '<details class="card p-3 mb-2"><summary class="cursor-pointer font-mono text-[12px]">' + ck +
```

（该 map 的 return 语句其余部分原样保留，只替换开头串接。）

（d）`renderWafAnalysis` 函数末尾（`if (el) el.innerHTML = html;` 之后）追加按钮可见性控制：

```js
    var probeBtn = document.getElementById('wafProbeBtn');
    if (probeBtn) probeBtn.style.display = (d.baseline && diffs.length) ? '' : 'none';
```

（e）WAF 区块末尾（`renderWafAnalysis` 函数之后）追加探针函数：

```js
  // ─── WAF 在线探针（后台 job 轮询）─────────────────────────
  window.wafToggleLeaf = function (i, on) { state.waf.probePaths[i] = on; };
  window.wafProbe = function () {
    var d = state.waf.analysis;
    if (!d || !d.baseline) { toast('无对照记录，不能发起探针'); return; }
    var paths = [];
    (d.diff || []).forEach(function (x, i) {
      if (x.kind !== 'removed' && state.waf.probePaths[i]) paths.push(x.path);
    });
    if (!paths.length) { toast('未勾选任何差异叶子'); return; }
    var accEl = document.getElementById('wafProbeAccount');
    var modelEl = document.getElementById('wafProbeModel');
    var accId = accEl ? accEl.value.trim() : '';
    var model = modelEl ? modelEl.value.trim() : '';
    if (!model) model = 'claude-haiku-4-5';
    if (!confirm('发起在线探针：' + (1 + paths.length) + ' 个变体（对照 1 + 叶子 ' + paths.length +
      '），每变体 2 次真实请求；命中后另有行级二分（约 log2(行数) 轮）。\n账号：' +
      (accId ? '#' + accId : '自动（首个活跃号）') + '\n模型：' + model +
      '\n将真实消耗额度并打到线上 Cloudflare，确认继续？')) return;
    api('/api/waf/probe', { method: 'POST', body: JSON.stringify({
      log_id: state.waf.currentId, baseline_id: d.baseline.id, paths: paths,
      account_id: accId ? Number(accId) : 0, model: model
    }) }).then(function (r) {
      state.waf.probeJob = r.job_id;
      var panel = document.getElementById('wafProbePanel');
      if (panel) panel.style.display = '';
      wafPollProbe();
    }).catch(function (e) { toast('探针发起失败：' + e.message); });
  };
  window.wafProbeAbort = function () {
    if (!state.waf.probeJob) return;
    api('/api/waf/probe/' + state.waf.probeJob, { method: 'DELETE' }).catch(function () {});
  };
  function wafPollProbe() {
    if (state.waf.probeTimer) clearInterval(state.waf.probeTimer);
    var tick = function () {
      api('/api/waf/probe/' + state.waf.probeJob).then(function (j) {
        renderWafProbe(j);
        if (j.status !== 'running') {
          clearInterval(state.waf.probeTimer);
          state.waf.probeTimer = null;
        }
      }).catch(function (e) {
        clearInterval(state.waf.probeTimer);
        state.waf.probeTimer = null;
        toast('探针状态获取失败：' + e.message);
      });
    };
    state.waf.probeTimer = setInterval(tick, 2000);
    tick();
  }
  function renderWafProbe(j) {
    var meta = document.getElementById('wafProbeMeta');
    if (meta) meta.textContent = '#' + (j.id || '') + ' · ' + (j.account || '') + ' · ' + (j.model || '') + ' · ' + (j.status || '');
    var html = '';
    if (j.phase === 'running' || j.status === 'running') html += '<div class="mb-3 text-[12px]" style="color:var(--muted);">进行中：' + esc(j.phase || '') + '（' + ((j.variants || []).length) + ' 个变体已完成）</div>';
    if (j.summary) html += '<div class="mb-3 text-[13px]" style="color:var(--fg-2);">' + esc(j.summary) + '</div>';
    var vs = j.variants || [];
    if (vs.length) {
      html += '<table class="data-table"><thead><tr><th>变体</th><th>结果</th><th>出站</th><th>Ray</th></tr></thead><tbody>' +
        vs.map(function (v) {
          return '<tr><td class="font-mono text-[12px]">' + esc(v.name) + '</td>' +
            '<td><span class="tag ' + (v.outcome === '403' ? 'tag-red' : v.outcome === 'pass' ? 'tag-green' : 'tag-gray') + '">' + esc(v.outcome) + '</span></td>' +
            '<td class="font-mono">' + fmt(v.bytes) + 'B</td>' +
            '<td class="font-mono text-[11px]">' + esc((v.rays || []).join(', ')) + '</td></tr>';
        }).join('') + '</tbody></table>';
    }
    var cs = j.conclusions || [];
    if (cs.length) {
      html += '<div class="mt-4"><div class="text-[12px] font-semibold mb-2" style="color:var(--muted);">触发行（行级二分收敛）</div>' +
        cs.map(function (c) {
          return '<details class="card p-3 mb-2" open><summary class="cursor-pointer font-mono text-[12px]">' +
            '<span class="tag tag-red">命中</span> ' + esc(c.path) +
            (c.ambiguous ? ' <span class="tag tag-amber">歧义（跨行组合特征）</span>' : '') +
            ' <span style="color:var(--muted);">' + (c.rounds || 0) + ' 轮</span></summary>' +
            '<pre class="text-[11px] mt-2 p-2 overflow-x-auto" style="background:var(--bg-2);border-radius:6px;">' +
            esc((c.lines || []).join('\n')) + '</pre></details>';
        }).join('') + '</div>';
    }
    var el = document.getElementById('wafProbeBody');
    if (el) el.innerHTML = html;
  }
```

- [ ] **Step 3: 语法与全量检查**

```bash
node --check internal/dashboard/static/dashboard.js && echo JS_OK
go vet ./... && go test ./...
```
Expected: `JS_OK` + 全 PASS

- [ ] **Step 4: 提交**

```bash
git add internal/dashboard/static/dashboard.js internal/dashboard/static/fragments/page-waf.html
git commit -m "dashboard: WAF probe launch (leaf checkboxes, confirm, polling, conclusion)

差异叶子勾选（removed 不可选）、确认弹窗显示变体数/账号/模型、
2s 轮询展示变体证据表与二分收敛触发行。

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: 文档与收尾回归

**Files:**
- Modify: `docs/403-waf-neutralization.md`（新增第 10 节）

**Interfaces:**
- Consumes: 全部前序任务的交付物。

- [ ] **Step 1: 文档新增第 10 节**

在 `docs/403-waf-neutralization.md` 文件末尾追加（承接第 9 节 bin/cat 手工二分）：

```markdown
## 10. 在线探针二分（面板化，2026-09-12）

第 9 节的七轮手工二分已固化为面板「WAF 检测」页的在线探针：

1. 分析面板勾选差异叶子（默认全选；removed 叶子不可选——没有发送到上游的内容）；
2. 「发起探针」→ 确认弹窗（变体数 = 1 对照 + N 叶子，每变体 2 次请求；可指定账号/模型，
   默认首个活跃号 + claude-haiku-4-5）；
3. 流程：对照变体（等长纯文本，被拦即中止——账号/出口在风控窗口）→ 叶子轮（逐叶验证）
   → 行级二分（~log2(行数) 轮收敛到触发行，每片 padding 到与原叶子等长）；
4. 页面 2s 轮询展示变体证据表（结果/Ray/出站字节）与触发行结论。

实现要点（`internal/api/waf_probe.go`）：

- 探针请求带 `ChatRequest.WafProbe` 标志：出站 query 跳过 WAF 中和与 capUpstreamQuery
  截断——中和会掐灭已知特征（叶子轮假阴性），截断会破坏二分切片的等长 padding；
- 不 ResetConversation：探针消息带唯一 nonce，指纹必然未命中 → 天然冷启动 USER_QUERY；
  Reset 会清掉该账号全部业务会话映射，干扰线上续聊；
- 绕过 router：不占重试预算、不触发账号冷却、不写 request_logs；
- 同一时刻仅一个 job（重复发起 409），可中止（DELETE），不持久化（重启即丢）。

验收基准（bin/cat 案例）：对 2026-09-11 b577d7cb 型 403，叶子轮应命中 README 叶子，
行级二分应收敛到 `./bin/catpaw2api -config config.json` 一行。
```

- [ ] **Step 2: 全量回归**

```bash
go vet ./... && go test ./... && node --check internal/dashboard/static/dashboard.js && echo ALL_OK
```
Expected: 全绿 + `ALL_OK`

- [ ] **Step 3: 提交**

```bash
git add docs/403-waf-neutralization.md
git commit -m "docs: WAF probe bisect runbook (section 10)

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## 自审记录

- **Spec 覆盖**：§3 入口交互（勾选/确认弹窗/变体预估/账号/模型）→ Task 5；叶子轮 + 2 次重复 + 全放行提示 → Task 4 runProbeJob；行级二分 + padTo 等长 + log2 收敛 → Task 3/4；执行模型（POST/GET/{job_id}、后台 job、单飞、不持久化）→ Task 4；实现位置（provider 层 Chat + 不走 router）→ Task 4 发送器；安全边界（显式人工、单飞拒绝）→ Task 4/5。spec 未列 DELETE 中止端点，但「单飞 + 90s×N 超时的 job 会阻塞后续探针」需要逃生门，10 行成本，符合「保证无阻塞异常」的项目约束。
- **占位符扫描**：无 TBD/TODO；所有代码步骤给出完整代码。
- **类型一致性**：`probeSender(ctx, text)` 在 api.go New 与测试注入处签名一致；`probeManager.newSender(acc, model)` 一致；`bisectDescend(n, hit)` 与调用处一致；`leafValue(body, path)` 与 POST 校验一致；前端消费的 JSON 键与 `snapshot()` 输出一致（variants[].name/outcome/rays/bytes、conclusions[].path/lines/ambiguous/rounds、summary、account、model、status、phase、id）。
- **已知取舍**：探针叶子轮的变体是「前缀 + 叶子全文」而非逐字节复刻原出站体（原体含 fold 包装/中和痕迹，逐字节复刻不可达也不必要——diff 已隔离出唯一变量是叶子内容，等长 padding 保证长度可比）。歧义分支（两半都放行）如实标注 ambiguous 并展示当前区间，不强行继续二分。

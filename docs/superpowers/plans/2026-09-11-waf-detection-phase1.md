# WAF 检测第一期（签名统一 + 403 离线分析）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 403 排查手册（docs/403-waf-neutralization.md）里的手工流程固化为面板功能：签名表统一到单一事实源 + 「WAF 检测」页面（403 列表 → 签名扫描 → 与成功请求逐叶 diff → 体积画像）。

**Architecture:** 签名列表唯一事实源在 `internal/provider/errors.go`（`wafSignatureProbes`），经新导出函数 `WafSignatureProbes()` 注入给 store（避免 store→provider 循环依赖）与 `/api/waf-signatures`；离线分析逻辑（逐叶 diff、对照组挑选）放在 `internal/api/waf.go` 纯函数 + 一个 store 查询方法；前端新增 page-waf.html 页面与 dashboard.js 渲染逻辑。零出站请求、零额度消耗。

**Tech Stack:** Go 1.22+（stdlib `net/http`、`encoding/json`、`database/sql`）、SQLite、原生 JS 单文件 dashboard（无框架、无构建步骤）。

**Spec:** `docs/superpowers/specs/2026-09-11-waf-detection-design.md`（本期覆盖 §1 + §2；§3 Phase B 探针在第一期验收后另写计划）

## Global Constraints

- 指纹铁律：任何代码不得改写已存储的 `request_logs.upstream_body` / `client_body` 原文；分析只读。
- 签名唯一事实源：`provider.wafSignatureProbes`（internal/provider/errors.go）。加新特征只改这一处 + waf.go 中和规则。
- `internal/store` 不得 import `internal/provider`（provider 已依赖 store，会循环）；store 需要签名列表时由调用方注入。
- 面板端点走 `s.auth(w, r)` 鉴权 + `jsonWrite`/`jsonError`（见 internal/api/helpers.go），与既有 `/api/*` 端点一致。
- 前端：原生 JS（ES5 风格，与 dashboard.js 现有代码一致），无新依赖；XSS 一律经 `esc()` 转义。
- 每个任务 TDD：先写失败测试，再实现，`go vet` + 相关包 `go test` 全绿后提交。

---

### Task 1: provider 导出签名列表与逐特征计数

**Files:**
- Modify: `internal/provider/errors.go`（wafSignatureProbes 定义之后）
- Test: `internal/provider/waf_test.go`（追加用例）

**Interfaces:**
- Produces: `WafSignatureProbes() []string`（返回 wafSignatureProbes 的副本，外部不可变）；`WafSignatureCounts(outboundBody string) map[string]int`（key=签名子串、value=归一化后出现次数，仅含 >0 项；空串/无命中返回空 map）。Task 2/4/5 依赖这两个函数名。

- [ ] **Step 1: 写失败测试**

在 `internal/provider/waf_test.go` 末尾追加：

```go
// TestWafSignatureProbesAndCounts 钉住导出接口：列表与内部表一致（外部拿到的副本
// 不可被改写），逐特征计数只含命中项且对 json.Marshal 转义形归一化后计数。
func TestWafSignatureProbesAndCounts(t *testing.T) {
	probes := WafSignatureProbes()
	if len(probes) == 0 {
		t.Fatal("WafSignatureProbes should not be empty")
	}
	// 副本不可变：改外部切片不得影响内部表
	probes[0] = "mutated"
	if WafSignatureProbes()[0] == "mutated" {
		t.Fatal("WafSignatureProbes must return a copy")
	}
	// 计数：bin/cat ×1、onerror= ×1，其余不出现；转义形 <script 也计数
	body := `{"q":"./bin/catpaw2api -config x","h":"<img onerror=alert(1)>","e":"\\u003cscript\\u003e"}`
	got := WafSignatureCounts(body)
	if got["bin/cat"] != 1 {
		t.Fatalf("bin/cat count = %d, want 1: %v", got["bin/cat"], got)
	}
	if got["onerror="] != 1 {
		t.Fatalf("onerror= count = %d, want 1: %v", got["onerror="], got)
	}
	if got["<script"] != 1 {
		t.Fatalf("escaped <script should count as 1, got %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("only nonzero probes expected, got %v", got)
	}
	if n := len(WafSignatureCounts("干净文本，无特征")); n != 0 {
		t.Fatalf("clean body should give empty counts, got %d", n)
	}
	if n := len(WafSignatureCounts("")); n != 0 {
		t.Fatalf("empty body should give empty counts, got %d", n)
	}
	// 与既有总数口径一致：各特征计数之和 == WafSignatureHitCount
	if total := WafSignatureHitCount(body); got["bin/cat"]+got["onerror="]+got["<script"] != total {
		t.Fatalf("per-probe sum should equal WafSignatureHitCount: %v vs %d", got, total)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/provider -run TestWafSignatureProbesAndCounts -count=1`
Expected: FAIL，`undefined: WafSignatureProbes`

- [ ] **Step 3: 实现**

在 `internal/provider/errors.go` 的 `WafSignatureHitCount` 之后追加：

```go
// WafSignatureProbes 返回 WAF 特征子串表的副本（单一事实源：面板 SQL 预设、store
// 统计、/api/waf-signatures 都从这里取，加新特征只改 wafSignatureProbes 一处）。
// 返回副本防止外部改写内部表。
func WafSignatureProbes() []string {
	out := make([]string, len(wafSignatureProbes))
	copy(out, wafSignatureProbes)
	return out
}

// WafSignatureCounts 逐特征统计出站请求体里的出现次数（归一化后、大小写不敏感），
// 只返回 >0 的项。供面板「WAF 检测」页展示"HTML 特征 ×N / bin/cat ×N / 零特征"。
func WafSignatureCounts(outboundBody string) map[string]int {
	counts := map[string]int{}
	if outboundBody == "" {
		return counts
	}
	normalized := normalizeWafBody(outboundBody)
	for _, probe := range wafSignatureProbes {
		if n := strings.Count(normalized, probe); n > 0 {
			counts[probe] = n
		}
	}
	return counts
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/provider -run 'TestWafSignatureProbesAndCounts|TestWafNeutralize' -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/provider/errors.go internal/provider/waf_test.go
git commit -m "provider: export WAF signature probes and per-probe counts"
```

---

### Task 2: store 统计 SQL 从签名列表程序化生成（消除手写漂移）

**Files:**
- Modify: `internal/store/store.go:614-653`（Cloudflare403SignatureSummary）
- Modify: `internal/router/observability.go`（调用点注入签名列表）
- Test: `internal/store/store_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `provider.WafSignatureProbes() []string`
- Produces: `WafSignatureLikeSQL() string`（store 包内函数，返回 `LIKE '%<p1>%' OR ...` 片段，无参数占位）；`Cloudflare403SignatureSummary(window time.Duration, probes []string) (string, error)`（签名从参数注入）。router 层 `observability.go` 依赖此新签名。

- [ ] **Step 1: 写失败测试**

先看 `internal/store/store_test.go` 里已有的临时库构造模式（搜 `func newTestStore` 或 `t.TempDir`），复用之。追加：

```go
// TestWafSignatureLikeSQL 钉住 SQL 片段生成：每个签名一条 LIKE，含 `<` 的签名
// 自动补转义形变体（json.Marshal 把 < 存成 <，SQL 子串匹配用 u003c）。
func TestWafSignatureLikeSQL(t *testing.T) {
	sql := WafSignatureLikeSQL([]string{"<script", "bin/cat"})
	want := `(upstream_body LIKE '%<script%' OR upstream_body LIKE '%u003cscript%'
		OR upstream_body LIKE '%bin/cat%')`
	if sql != want {
		t.Fatalf("WafSignatureLikeSQL =\n%s\nwant\n%s", sql, want)
	}
	// 空列表：恒 false 片段，保证拼进 SQL 不炸
	if got := WafSignatureLikeSQL(nil); got != "(0)" {
		t.Fatalf("nil probes should give '(0)', got %s", got)
	}
}

// TestCloudflare403SignatureSummaryInjected 统计口径沿用注入的签名表：
// 403 且命中签名的行计入 blockedHit，成功行计入 okHit。
func TestCloudflare403SignatureSummaryInjected(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	s.LogRequest(&RequestLog{Status: "error", ErrorMessage: "Postman gateway rejected request (403, Cloudflare)", UpstreamBody: `{"a":"./bin/cat x"}`, RequestBytes: 100})
	s.LogRequest(&RequestLog{Status: "success", UpstreamBody: `{"a":"clean"}`, RequestBytes: 100})
	out, err := s.Cloudflare403SignatureSummary(time.Hour, []string{"bin/cat"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1/1") || !strings.Contains(out, "0/1") {
		t.Fatalf("summary should report 1/1 blocked hit and 0/1 ok hit, got: %s", out)
	}
}
```

注意：`newTestStore` 若不存在，按 store_test.go 现有临时库构造方式写一个（`store.Open(filepath.Join(t.TempDir(), "t.db"))`）。`LogRequest` 的必填列见 store.go:457 的 INSERT（`CreatedAt` 为零值时由 SQL 默认填充，确认 INSERT 语句是否带 DEFAULT CURRENT_TIMESTAMP，不带则测试里显式设 `CreatedAt: time.Now()`）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store -run 'TestWafSignatureLikeSQL|TestCloudflare403SignatureSummaryInjected' -count=1`
Expected: FAIL，`undefined: WafSignatureLikeSQL` / 参数数量不匹配编译错

- [ ] **Step 3: 实现**

`internal/store/store.go`：把 `Cloudflare403SignatureSummary` 里的 `const sig = ...`（626-627 行）删掉，函数签名加 `probes []string` 参数，函数体开头生成：

```go
// WafSignatureLikeSQL 把签名子串表拼成 request_logs.upstream_body 的 LIKE 匹配
// 片段。签名里含 `<` 时自动补一条转义形变体：Go json.Marshal 把 `<` 存成六字符
// 字面量 <，SQL 子串匹配用 `u003c`（不写反斜杠避免多层转义歧义，先例见旧
// 手写 SQL）。空表返回恒 false 的 `(0)`，拼进 WHERE 不炸。
func WafSignatureLikeSQL(probes []string) string {
	var pats []string
	for _, p := range probes {
		if p == "" {
			continue
		}
		pats = append(pats, fmt.Sprintf("upstream_body LIKE '%%%s%%'", p))
		if esc := strings.ReplaceAll(p, "<", "u003c"); esc != p {
			pats = append(pats, fmt.Sprintf("upstream_body LIKE '%%%s%%'", esc))
		}
	}
	if len(pats) == 0 {
		return "(0)"
	}
	return "(" + strings.Join(pats, " OR\n\t\t") + ")"
}
```

`Cloudflare403SignatureSummary` 开头加 `sig := WafSignatureLikeSQL(probes)`，其余逻辑不动。

`internal/router/observability.go:27` 调用点改为：

```go
if sig, err := r.Store.Cloudflare403SignatureSummary(60*time.Minute, provider.WafSignatureProbes()); err == nil && sig != "" {
```

（router 包已 import provider；若没有则补。）

- [ ] **Step 4: 全量验证（改了函数签名，必须全包跑）**

Run: `go vet ./... && go test ./internal/store ./internal/router -count=1`
Expected: PASS。若 router 的既有测试 mock 了 `Cloudflare403SignatureSummary` 调用，同步补参数。

- [ ] **Step 5: 提交**

```bash
git add internal/store/store.go internal/store/store_test.go internal/router/observability.go
git commit -m "store: build 403 signature SQL from injected probe list (single source of truth)"
```

---

### Task 3: store 新增 WAF 分析查询方法

**Files:**
- Modify: `internal/store/store.go`（Cloudflare403SignatureSummary 之后集中追加）
- Test: `internal/store/store_test.go`（追加）

**Interfaces:**
- Produces（Task 5 的 handler 全靠这几个方法）:
  - `GetRequestLog(id int64) (*RequestLog, error)`——按主键取单条，含全部列；不存在返回 `(nil, nil)`。
  - `PageCloudflare403Logs(offset, limit int) ([]*RequestLog, error)`——`status='error' AND error_message LIKE '%Cloudflare%'` 按 `id DESC` 分页（复用 `requestLogColumns` + `scanRequestLogs`）。
  - `CountCloudflare403Logs() (int64, error)`
  - `FindWafBaseline(target *RequestLog) (baseline *RequestLog, tier string, err error)`——三级回退：同 `conversation_id`（非空）最近成功 → 同 `account_id` 最近成功 → 全局最近成功；都要求 `status='success'` 且 `id < target.ID`；tier 取值 `"conversation" | "account" | "global" | ""`（空 = 无对照）。返回 `(nil, "", nil)` 表示无对照。
  - `WafBaselineCandidates(target *RequestLog, limit int) ([]*RequestLog, error)`——三个层级各取 `limit/3` 条合并（供手动换对照的下拉），按与 target 相关度排序（同会话在前）。
  - `WafSizeBuckets(day string) ([]map[string]interface{}, error)`——当天 `(request_bytes/10000)*10` 分桶的 ok/fail/max_ok（SQL 见下）。

- [ ] **Step 1: 写失败测试**

```go
// TestWafAnalysisQueries 覆盖 403 列表分页、baseline 三级回退、候选与分桶。
func TestWafAnalysisQueries(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	now := time.Now()
	mk := func(status, errmsg, conv string, accID int64, bytes int) *RequestLog {
		return &RequestLog{Status: status, ErrorMessage: errmsg, ConversationID: conv,
			AccountID: &accID, RequestBytes: bytes, UpstreamBody: `{"a":1}`,
			CreatedAt: now.Add(-time.Duration(rand.Intn(60)) * time.Second)}
	}
	var acc int64 = 7
	// 1: 成功(同会话) 2: 403 3: 成功(同账号无会话) 4: 403 5: 成功(他会话)
	s.LogRequest(mk("success", "", "convA", acc, 40000))                 // id 1
	s.LogRequest(mk("error", "(403, Cloudflare)", "convA", acc, 41000))  // id 2
	s.LogRequest(mk("success", "", "", acc, 42000))                      // id 3（无会话键 → 独立组）
	s.LogRequest(mk("error", "(403, Cloudflare)", "convB", acc, 43000))  // id 4
	s.LogRequest(mk("success", "", "convC", acc+1, 44000))               // id 5

	n, err := s.CountCloudflare403Logs()
	if err != nil || n != 2 {
		t.Fatalf("CountCloudflare403Logs = %d, %v; want 2", n, err)
	}
	logs, err := s.PageCloudflare403Logs(0, 10)
	if err != nil || len(logs) != 2 || logs[0].ID < logs[1].ID {
		t.Fatalf("PageCloudflare403Logs should be id DESC, got %v (%v)", logs, err)
	}

	// 单条查询
	l2, err := s.GetRequestLog(2)
	if err != nil || l2 == nil || l2.ConversationID != "convA" {
		t.Fatalf("GetRequestLog(2) = %+v, %v", l2, err)
	}
	if miss, _ := s.GetRequestLog(999); miss != nil {
		t.Fatal("GetRequestLog(999) should be nil, nil")
	}

	// baseline 三级回退：同会话成功(id1) → tier=conversation
	b, tier, err := s.FindWafBaseline(l2)
	if err != nil || b == nil || b.ID != 1 || tier != "conversation" {
		t.Fatalf("FindWafBaseline(convA) = id %d tier %q (%v); want id 1 conversation", bID(b), tier, err)
	}
	// convB 无同会话成功 → 同账号最近成功(id3) → tier=account
	l4, _ := s.GetRequestLog(4)
	b, tier, _ = s.FindWafBaseline(l4)
	if b == nil || b.ID != 3 || tier != "account" {
		t.Fatalf("FindWafBaseline(convB) = id %d tier %q; want id 3 account", bID(b), tier)
	}
	// 候选下拉：同会话/同账号/全局混合
	cands, err := s.WafBaselineCandidates(l4, 9)
	if err != nil || len(cands) == 0 {
		t.Fatalf("WafBaselineCandidates = %v (%v)", cands, err)
	}
	// 分桶：40-50K 桶 ok=3 fail=2
	buckets, err := s.WafSizeBuckets(now.Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, bk := range buckets {
		if bk["bucket"] == "40K" {
			found = true
			if bk["ok"].(int64) != 3 || bk["fail"].(int64) != 2 {
				t.Fatalf("bucket 40K = %v; want ok=3 fail=2", bk)
			}
		}
	}
	if !found {
		t.Fatalf("bucket 40K missing in %v", buckets)
	}
}

func bID(l *RequestLog) int64 { if l == nil { return 0 }; return l.ID }
```

（`rand` 若未导入改用固定秒偏移；分桶断言前先打印 buckets 调整键型——SQLite 聚合可能返回 int64。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store -run TestWafAnalysisQueries -count=1`
Expected: FAIL，`undefined: s.CountCloudflare403Logs` 等编译错

- [ ] **Step 3: 实现**

在 `Cloudflare403SignatureSummary` 之后追加：

```go
// GetRequestLog 按主键取单条请求日志（WAF 分析页用）。不存在返回 (nil, nil)。
func (s *Store) GetRequestLog(id int64) (*RequestLog, error) {
	rows, err := s.db.Query(`SELECT `+requestLogColumns+`
		FROM request_logs rl LEFT JOIN accounts a ON a.id = rl.account_id
		WHERE rl.id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs, err := scanRequestLogs(rows)
	if err != nil || len(logs) == 0 {
		return nil, err
	}
	return logs[0], nil
}

// PageCloudflare403Logs 分页返回 Cloudflare 网关拒绝(403)的日志，id 倒序（WAF 检测页列表）。
func (s *Store) PageCloudflare403Logs(offset, limit int) ([]*RequestLog, error) {
	rows, err := s.db.Query(`SELECT `+requestLogColumns+`
		FROM request_logs rl LEFT JOIN accounts a ON a.id = rl.account_id
		WHERE rl.status='error' AND rl.error_message LIKE '%Cloudflare%'
		ORDER BY rl.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRequestLogs(rows)
}

// CountCloudflare403Logs 返回 403 日志总数（分页用）。
func (s *Store) CountCloudflare403Logs() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs
		WHERE status='error' AND error_message LIKE '%Cloudflare%'`).Scan(&n)
	return n, err
}

// wafBaselineQuery 构造三级回退的对照查询：tier 依次为同会话 → 同账号 → 全局，
// 都要求 success 且 id 早于目标（时间上在前的成功请求才有 diff 意义）。
func (s *Store) wafBaselineQuery(target *RequestLog, cond string, args ...interface{}) (*RequestLog, error) {
	q := `SELECT ` + requestLogColumns + `
		FROM request_logs rl LEFT JOIN accounts a ON a.id = rl.account_id
		WHERE rl.status='success' AND rl.id < ? ` + cond + `
		ORDER BY rl.id DESC LIMIT 1`
	args = append([]interface{}{target.ID}, args...)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs, err := scanRequestLogs(rows)
	if err != nil || len(logs) == 0 {
		return nil, err
	}
	return logs[0], nil
}

// FindWafBaseline 为一条 403 日志挑对照成功请求：同 conversation_id 最近成功 →
// 同 account_id 最近成功 → 全局最近成功。无对照返回 (nil, "", nil)。
func (s *Store) FindWafBaseline(target *RequestLog) (baseline *RequestLog, tier string, err error) {
	if target.ConversationID != "" {
		if b, e := s.wafBaselineQuery(target, `AND rl.conversation_id = ?`, target.ConversationID); e != nil {
			return nil, "", e
		} else if b != nil {
			return b, "conversation", nil
		}
	}
	if target.AccountID != nil {
		if b, e := s.wafBaselineQuery(target, `AND rl.account_id = ?`, *target.AccountID); e != nil {
			return nil, "", e
		} else if b != nil {
			return b, "account", nil
		}
	}
	if b, e := s.wafBaselineQuery(target, ``); e != nil {
		return nil, "", e
	} else if b != nil {
		return b, "global", nil
	}
	return nil, "", nil
}

// WafBaselineCandidates 返回手动换对照的候选成功请求：同会话/同账号/全局三层各
// 取 limit/3 条，同层按 id 倒序，层间按 相关度（conversation > account > global）排列。
func (s *Store) WafBaselineCandidates(target *RequestLog, limit int) ([]*RequestLog, error) {
	if limit < 3 {
		limit = 3
	}
	var out []*RequestLog
	seen := map[int64]bool{}
	appendTier := func(cond string, args ...interface{}) {
		q := `SELECT ` + requestLogColumns + `
			FROM request_logs rl LEFT JOIN accounts a ON a.id = rl.account_id
			WHERE rl.status='success' AND rl.id < ? ` + cond + `
			ORDER BY rl.id DESC LIMIT ?`
		args = append([]interface{}{target.ID}, args...)
		args = append(args, limit/3)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return
		}
		defer rows.Close()
		logs, _ := scanRequestLogs(rows)
		for _, l := range logs {
			if !seen[l.ID] {
				seen[l.ID] = true
				out = append(out, l)
			}
		}
	}
	if target.ConversationID != "" {
		appendTier(`AND rl.conversation_id = ?`, target.ConversationID)
	}
	if target.AccountID != nil {
		appendTier(`AND rl.account_id = ?`, *target.AccountID)
	}
	appendTier(``)
	return out, nil
}

// WafSizeBuckets 按天返回出站体积分桶的成功/失败分布（10KB 一档，手册第 7 节口径），
// 供分析页判断本条体积在当天分布里的位置。
func (s *Store) WafSizeBuckets(day string) ([]map[string]interface{}, error) {
	rows, err := s.db.Query(`SELECT ((request_bytes/10000)*10) || 'K' AS bucket,
			SUM(status='success') AS ok, SUM(status!='success') AS fail,
			MAX(request_bytes) FILTER (WHERE status='success') AS max_ok
		FROM request_logs
		WHERE substr(created_at,1,10) = ?
		GROUP BY bucket ORDER BY bucket`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var bucket string
		var ok, fail, maxOk sql.NullInt64
		if err := rows.Scan(&bucket, &ok, &fail, &maxOk); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"bucket": bucket, "ok": ok.Int64, "fail": fail.Int64, "maxOk": maxOk.Int64,
		})
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store -run 'TestWafAnalysisQueries' -count=1 -v`
Expected: PASS（若分桶键型/时间断言不匹配，按实际输出微调测试断言，不改实现）

- [ ] **Step 5: 提交**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: WAF analysis queries (403 page, baseline fallback, size buckets)"
```

---

### Task 4: API 端点（/api/waf-signatures + /api/waf/baselines）

**Files:**
- Create: `internal/api/waf.go`
- Modify: `internal/api/api.go`（路由注册，插在 sql-query 之后）
- Test: `internal/api/waf_test.go`

**Interfaces:**
- Consumes: Task 1 `provider.WafSignatureProbes()`；Task 3 `WafBaselineCandidates`
- Produces:
  - `GET /api/waf-signatures` → `{"probes": ["<script", ..., "bin/cat"]}`
  - `GET /api/waf/baselines?log_id=N` → `{"data": [{"id","createdAt","requestBytes","accountEmail","conversationId"}...]}`
  - handler 方法 `(s *Server) wafSignatures / wafBaselines`（Task 5 的 wafAnalyze 与它们同文件）

- [ ] **Step 1: 写失败测试**

先看 `internal/api/login_test.go` 的 Server 构造方式（httptest + `s.Register(mux)`），复用之。鉴权：测试里 settings 无 api_key 时 `s.auth` 直接放行。

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ps2api/internal/store"
)

// TestWafSignaturesEndpoint 签名表经 API 暴露给前端（单一事实源的对外出口）。
func TestWafSignaturesEndpoint(t *testing.T) {
	s := New(store.MustOpenTest(t))
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf-signatures", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct{ Probes []string `json:"probes"` }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Probes) == 0 {
		t.Fatal("probes should not be empty")
	}
}

// TestWafBaselinesEndpoint 候选对照列表：有成功记录时返回候选，无则空数组。
func TestWafBaselinesEndpoint(t *testing.T) {
	st := store.MustOpenTest(t)
	var acc int64 = 7
	st.LogRequest(&store.RequestLog{Status: "success", AccountID: &acc, RequestBytes: 100, UpstreamBody: `{}`, ConversationID: "c1", CreatedAt: time.Now()})
	st.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)", AccountID: &acc, RequestBytes: 120, UpstreamBody: `{}`, ConversationID: "c1", CreatedAt: time.Now()})
	s := New(st)
	mux := http.NewServeMux()
	s.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/baselines?log_id=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct{ Data []map[string]interface{} `json:"data"` }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) == 0 {
		t.Fatal("expected baseline candidates")
	}
	if got.Data[0]["id"].(float64) != 1 {
		t.Fatalf("first candidate should be id 1, got %v", got.Data[0]["id"])
	}
}
```

（`store.MustOpenTest` 若不存在，在 store_test.go 同步加一个 `func MustOpenTest(t *testing.T) *Store { s, err := Open(filepath.Join(t.TempDir(), "t.db")); if err != nil { t.Fatal(err) }; return s }`——注意放非 _test 文件会进产物，改放 `store_test.go` 仅供测试包用；api 是另一个包引用不到 —— 所以这个 helper 必须放 `export_test.go`？不行，跨包测试引用不到。直接在 api 的测试里手写 `store.Open(filepath.Join(t.TempDir(), "t.db"))` 三行即可，不抽 helper。同时 Task 3 的 `newTestStore` 同理只在 store 包内用。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api -run 'TestWafSignaturesEndpoint|TestWafBaselinesEndpoint' -count=1`
Expected: FAIL，404（路由未注册）

- [ ] **Step 3: 实现**

`internal/api/waf.go`（新文件）：

```go
// waf.go —— 面板「WAF 检测」页的端点：签名表暴露（单一事实源出口）、
// 对照候选列表、离线分析（analyze，见 Task 5）。全部只读、零出站请求。
package api

import (
	"net/http"
	"strconv"

	"ps2api/internal/provider"
)

// wafSignatures 返回 WAF 特征子串表（前端 SQL 预设等从这里取，单一事实源）。
func (s *Server) wafSignatures(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	jsonWrite(w, 200, map[string]interface{}{"probes": provider.WafSignatureProbes()})
}

// wafBaselines 返回某条 403 日志的对照候选成功请求（手动换对照的下拉数据源）。
func (s *Server) wafBaselines(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	logID, _ := strconv.ParseInt(r.URL.Query().Get("log_id"), 10, 64)
	if logID <= 0 {
		jsonError(w, 400, "log_id 必须为正整数", "invalid_request_error")
		return
	}
	target, err := s.Store.GetRequestLog(logID)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	if target == nil {
		jsonError(w, 404, "日志不存在", "invalid_request_error")
		return
	}
	cands, err := s.Store.WafBaselineCandidates(target, 9)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	data := make([]map[string]interface{}, 0, len(cands))
	for _, c := range cands {
		data = append(data, map[string]interface{}{
			"id": c.ID, "createdAt": c.CreatedAt, "requestBytes": c.RequestBytes,
			"accountEmail": c.AccountEmail, "conversationId": c.ConversationID,
		})
	}
	jsonWrite(w, 200, map[string]interface{}{"data": data})
}
```

`internal/api/api.go` 路由区（`POST /api/sql-query` 之后）：

```go
	// 管理类端点（/api/*）——WAF 检测（见 waf.go）
	mux.HandleFunc("GET /api/waf-signatures", s.wafSignatures)
	mux.HandleFunc("GET /api/waf/baselines", s.wafBaselines)
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/api -run 'TestWafSignaturesEndpoint|TestWafBaselinesEndpoint' -count=1`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/waf.go internal/api/waf_test.go internal/api/api.go
git commit -m "api: WAF signatures and baseline candidates endpoints"
```

---

### Task 5: 逐叶 diff 引擎 + GET /api/waf/analyze

**Files:**
- Modify: `internal/api/waf.go`（追加纯函数与 handler）
- Test: `internal/api/waf_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 `WafSignatureCounts`；Task 3 `GetRequestLog / FindWafBaseline / WafSizeBuckets`
- Produces:
  - `type LeafDiff struct { Path string; Kind string; BaselineLen, TargetLen int; BaselinePreview, TargetPreview string }`（JSON 标签 `path/kind/baselineLen/targetLen/baselinePreview/targetPreview`；Kind ∈ `added|changed|removed`；Preview 截 500 字符）
  - `func leafDiff(baseline, target string) []LeafDiff`——JSON 解析失败时整串视为单叶子（Path="(body)"，Kind 按是否相等）；解析成功时递归走 map/array/string 与标量叶
  - `GET /api/waf/analyze?log_id=N&baseline_id=Y`（baseline_id 可选）→

```json
{
  "log": {"id","createdAt","accountEmail","model","requestBytes","errorMessage","conversationId"},
  "signatureCounts": {"bin/cat": 1},
  "baseline": {"id","createdAt","requestBytes","accountEmail","tier"} | null,
  "diff": [LeafDiff...],
  "sizeBuckets": [{"bucket","ok","fail","maxOk"}...]
}
```

- [ ] **Step 1: 写失败测试（diff 纯函数）**

```go
// TestLeafDiff 钉住逐叶 diff 语义：只报差异叶子；字符串/标量/数组/删除/新增全覆盖；
// JSON 解析失败退化为整串单叶子。
func TestLeafDiff(t *testing.T) {
	base := `{"input":{"query":"hello","max":1},"arr":["a","b"],"keep":"same"}`
	// changed + added + removed 三类
	target := `{"input":{"query":"hello world","max":1},"arr":["a","c"],"keep":"same","new":"x"}`
	diffs := leafDiff(base, target)
	byPath := map[string]LeafDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	if len(diffs) != 3 {
		t.Fatalf("want 3 diffs, got %d: %+v", len(diffs), diffs)
	}
	q := byPath["input.query"]
	if q.Kind != "changed" || q.BaselineLen != 5 || q.TargetLen != 11 {
		t.Fatalf("input.query diff wrong: %+v", q)
	}
	if q.TargetPreview != "hello world" {
		t.Fatalf("preview = %q", q.TargetPreview)
	}
	if byPath["new"].Kind != "added" {
		t.Fatalf("new leaf should be added: %+v", byPath["new"])
	}
	if byPath["arr[1]"].Kind != "changed" {
		t.Fatalf("arr[1] should be changed: %+v", byPath["arr[1]"])
	}
	// removed：target 删掉 keep
	diffs = leafDiff(base, `{"input":{"query":"hello","max":1},"arr":["a","b"]}`)
	if len(diffs) != 1 || diffs[0].Path != "keep" || diffs[0].Kind != "removed" {
		t.Fatalf("removed leaf wrong: %+v", diffs)
	}
	// 相同 body 零差异
	if got := leafDiff(base, base); len(got) != 0 {
		t.Fatalf("identical bodies should give no diffs, got %+v", got)
	}
	// 非法 JSON：退化整串
	diffs = leafDiff("not-json{", "not-json{2")
	if len(diffs) != 1 || diffs[0].Path != "(body)" || diffs[0].Kind != "changed" {
		t.Fatalf("malformed bodies should degrade to single leaf: %+v", diffs)
	}
	// 长文本截断到 500
	long := strings.Repeat("x", 800)
	diffs = leafDiff(`{"a":"`+long+`"}`, `{"a":"`+long+`y"}`)
	if len(diffs[0].TargetPreview) != 500 {
		t.Fatalf("preview should cap at 500, got %d", len(diffs[0].TargetPreview))
	}
}
```

- [ ] **Step 2: 写失败测试（analyze handler）**

```go
// TestWafAnalyzeEndpoint 端到端：403 行 + 同会话成功行 → 签名计数、diff、
// baseline tier、分桶齐返回；无 baseline_id 时自动挑选；显式 baseline_id 覆盖自动挑选。
func TestWafAnalyzeEndpoint(t *testing.T) {
	st, s, mux := newWafTestServer(t)
	var acc int64 = 7
	st.LogRequest(&store.RequestLog{Status: "success", AccountID: &acc, RequestBytes: 100,
		UpstreamBody: `{"input":{"query":"./bin/cat ok"}}`, ConversationID: "c1", CreatedAt: time.Now()})
	st.LogRequest(&store.RequestLog{Status: "error", ErrorMessage: "(403, Cloudflare)", AccountID: &acc,
		RequestBytes: 120, UpstreamBody: `{"input":{"query":"./bin/catpaw2api -config x"}}`,
		ConversationID: "c1", CreatedAt: time.Now()})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/analyze?log_id=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Log              map[string]interface{}            `json:"log"`
		SignatureCounts  map[string]int                    `json:"signatureCounts"`
		Baseline         map[string]interface{}            `json:"baseline"`
		Diff             []LeafDiff                        `json:"diff"`
		SizeBuckets      []map[string]interface{}          `json:"sizeBuckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SignatureCounts["bin/cat"] != 1 {
		t.Fatalf("signatureCounts = %v", got.SignatureCounts)
	}
	if got.Baseline["id"].(float64) != 1 || got.Baseline["tier"] != "conversation" {
		t.Fatalf("baseline = %v", got.Baseline)
	}
	if len(got.Diff) != 1 || got.Diff[0].Path != "input.query" || got.Diff[0].Kind != "changed" {
		t.Fatalf("diff = %+v", got.Diff)
	}
	if len(got.SizeBuckets) == 0 {
		t.Fatal("sizeBuckets should not be empty")
	}
	// 显式 baseline_id：无对照可选时 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/analyze?log_id=2&baseline_id=999", nil))
	if rec.Code != 404 {
		t.Fatalf("missing baseline should 404, got %d", rec.Code)
	}
	// log_id 非法 400
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/waf/analyze?log_id=abc", nil))
	if rec.Code != 400 {
		t.Fatalf("bad log_id should 400, got %d", rec.Code)
	}
}

// newWafTestServer 构造带临时库的 Server + mux（Task 4 的测试可改用这个）。
func newWafTestServer(t *testing.T) (*store.Store, *Server, *http.ServeMux) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st)
	mux := http.NewServeMux()
	s.Register(mux)
	return st, s, mux
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/api -run 'TestLeafDiff|TestWafAnalyzeEndpoint' -count=1`
Expected: FAIL，`undefined: leafDiff` / 404

- [ ] **Step 4: 实现**

`internal/api/waf.go` 追加：

```go
// ── 逐叶 diff：把两个出站体（JSON 文本）解析后按叶子路径比较，只返回差异叶子。──

const leafPreviewMax = 500

// LeafDiff 是一个差异叶子。Kind: added（对照无、目标有）/ changed（都有但不同）/
// removed（对照有、目标无）。Preview 截 500 字符（全文可从请求日志页取）。
type LeafDiff struct {
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	BaselineLen     int    `json:"baselineLen"`
	TargetLen       int    `json:"targetLen"`
	BaselinePreview string `json:"baselinePreview"`
	TargetPreview   string `json:"targetPreview"`
}

// leafDiff 比较 baseline 与 target 两个 JSON 出站体，返回差异叶子列表。
// 解析失败（或为空）时退化为整串单叶子比较（Path="(body)"），保证脏数据不拦死分析。
func leafDiff(baseline, target string) []LeafDiff {
	var bv, tv interface{}
	berr, terr := json.Unmarshal([]byte(baseline), &bv), json.Unmarshal([]byte(target), &tv)
	if baseline == "" {
		return []LeafDiff{{Path: "(body)", Kind: "added", TargetLen: len(target), TargetPreview: truncateStr(target)}}
	}
	if target == "" {
		return []LeafDiff{{Path: "(body)", Kind: "removed", BaselineLen: len(baseline), BaselinePreview: truncateStr(baseline)}}
	}
	if berr != nil || terr != nil {
		kind := "changed"
		if baseline == target {
			return nil
		}
		return []LeafDiff{{Path: "(body)", Kind: kind, BaselineLen: len(baseline), TargetLen: len(target),
			BaselinePreview: truncateStr(baseline), TargetPreview: truncateStr(target)}}
	}
	var diffs []LeafDiff
	walk(bv, tv, "", &diffs)
	return diffs
}

// walk 递归比较两棵 JSON 树：map 按键、array 按下标、标量与字符串直接比较。
// 键集合不对称时按 added / removed 记录。
func walk(b, t interface{}, path string, diffs *[]LeafDiff) {
	switch bv := b.(type) {
	case map[string]interface{}:
		tv, ok := t.(map[string]interface{})
		if !ok {
			scalarDiff(b, t, path, diffs)
			return
		}
		keys := make([]string, 0, len(bv)+len(tv))
		seen := map[string]bool{}
		for k := range bv {
			keys = append(keys, k)
			seen[k] = true
		}
		for k := range tv {
			if !seen[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := k
			if path != "" {
				child = path + "." + k
			}
			bvHave, bh := bv[k]
			tvHave, th := tv[k]
			switch {
			case bh && !th:
				*diffs = append(*diffs, leafRemoved(child, bvHave))
			case !bh && th:
				*diffs = append(*diffs, leafAdded(child, tvHave))
			default:
				walk(bvHave, tvHave, child, diffs)
			}
		}
	case []interface{}:
		tv, ok := t.([]interface{})
		if !ok {
			scalarDiff(b, t, path, diffs)
			return
		}
		n := len(bv)
		if len(tv) > n {
			n = len(tv)
		}
		for i := 0; i < n; i++ {
			child := fmt.Sprintf("%s[%d]", path, i)
			switch {
			case i >= len(bv):
				*diffs = append(*diffs, leafAdded(child, tv[i]))
			case i >= len(tv):
				*diffs = append(*diffs, leafRemoved(child, bv[i]))
			default:
				walk(bv[i], tv[i], child, diffs)
			}
		}
	default:
		scalarDiff(b, t, path, diffs)
	}
}

// scalarDiff 比较两个标量/字符串叶子（类型不同也算 changed）。
func scalarDiff(b, t interface{}, path string, diffs *[]LeafDiff) {
	if reflect.DeepEqual(b, t) {
		return
	}
	bs, ts := leafString(b), leafString(t)
	*diffs = append(*diffs, LeafDiff{Path: path, Kind: "changed",
		BaselineLen: len(bs), TargetLen: len(ts),
		BaselinePreview: truncateStr(bs), TargetPreview: truncateStr(ts)})
}

func leafAdded(path string, v interface{}) LeafDiff {
	s := leafString(v)
	return LeafDiff{Path: path, Kind: "added", TargetLen: len(s), TargetPreview: truncateStr(s)}
}

func leafRemoved(path string, v interface{}) LeafDiff {
	s := leafString(v)
	return LeafDiff{Path: path, Kind: "removed", BaselineLen: len(s), BaselinePreview: truncateStr(s)}
}

func leafString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func truncateStr(s string) string {
	r := []rune(s)
	if len(r) <= leafPreviewMax {
		return s
	}
	return string(r[:leafPreviewMax]) + "…"
}

// ── 离线分析端点 ────────────────────────────────────────────

// wafAnalyze 对一条 403 日志做离线分析：签名计数、对照 diff（baseline_id 缺省
// 三级回退自动挑）、当天体积分桶。零出站请求。
func (s *Server) wafAnalyze(w http.ResponseWriter, r *http.Request) {
	if !s.auth(w, r) {
		return
	}
	logID, _ := strconv.ParseInt(r.URL.Query().Get("log_id"), 10, 64)
	if logID <= 0 {
		jsonError(w, 400, "log_id 必须为正整数", "invalid_request_error")
		return
	}
	target, err := s.Store.GetRequestLog(logID)
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	if target == nil {
		jsonError(w, 404, "日志不存在", "invalid_request_error")
		return
	}
	var baseline *store.RequestLog
	tier := ""
	if v, _ := strconv.ParseInt(r.URL.Query().Get("baseline_id"), 10, 64); v > 0 {
		baseline, err = s.Store.GetRequestLog(v)
		if err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
		if baseline == nil {
			jsonError(w, 404, "对照日志不存在", "invalid_request_error")
			return
		}
		tier = "manual"
	} else {
		baseline, tier, err = s.Store.FindWafBaseline(target)
		if err != nil {
			jsonError(w, 500, err.Error(), "internal_error")
			return
		}
	}
	var diffs []LeafDiff
	if baseline != nil {
		diffs = leafDiff(baseline.UpstreamBody, target.UpstreamBody)
	}
	buckets, err := s.Store.WafSizeBuckets(target.CreatedAt.Format("2006-01-02"))
	if err != nil {
		jsonError(w, 500, err.Error(), "internal_error")
		return
	}
	resp := map[string]interface{}{
		"log": map[string]interface{}{
			"id": target.ID, "createdAt": target.CreatedAt, "accountEmail": target.AccountEmail,
			"model": target.Model, "requestBytes": target.RequestBytes,
			"errorMessage": target.ErrorMessage, "conversationId": target.ConversationID,
		},
		"signatureCounts": provider.WafSignatureCounts(target.UpstreamBody),
		"baseline":        nil,
		"diff":            diffs,
		"sizeBuckets":     buckets,
	}
	if baseline != nil {
		resp["baseline"] = map[string]interface{}{
			"id": baseline.ID, "createdAt": baseline.CreatedAt, "requestBytes": baseline.RequestBytes,
			"accountEmail": baseline.AccountEmail, "tier": tier,
		}
	}
	jsonWrite(w, 200, resp)
}
```

import 块补 `"encoding/json"`、`"fmt"`、`"reflect"`、`"sort"`、`"ps2api/internal/store"`。路由（api.go，Task 4 那组之后）：

```go
	mux.HandleFunc("GET /api/waf/analyze", s.wafAnalyze)
```

- [ ] **Step 5: 跑测试确认通过 + 全量回归**

Run: `go vet ./... && go test ./... -count=1`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/api/waf.go internal/api/waf_test.go internal/api/api.go
git commit -m "api: leaf-diff engine and /api/waf/analyze offline 403 analysis"
```

---

### Task 6: 面板「WAF 检测」页

**Files:**
- Create: `internal/dashboard/static/fragments/page-waf.html`
- Modify: `internal/dashboard/static/fragments/sidebar.html`（「监控」组，「数据查询」之后加条目）
- Modify: `internal/dashboard/static/dashboard.js`（bootstrap 片段清单、switchPage 映射、页面状态与渲染函数）

**Interfaces:**
- Consumes: Task 4/5 的三个端点（`/api/waf-signatures`、`/api/waf/baselines`、`/api/waf/analyze`）
- Produces: 页面路由名 `waf`；全局函数 `wafRefresh()`、`wafAnalyze(logId)`、`wafPage(page)`、`wafSetBaseline(value)`（dashboard.js 的 onclick 约定需要挂 window）

- [ ] **Step 1: page-waf.html**

```html
<section class="page" id="page-waf">
    <div class="p-8">
      <div class="mb-8">
        <div class="text-[11px] font-semibold tracking-wider uppercase mb-2" style="color: var(--muted);">WAF Detection</div>
        <h1 class="hero-title">WAF <em>检测</em></h1>
        <p class="mt-2 text-[14px]" style="color: var(--fg-2);">Cloudflare 403 离线分析：签名扫描 + 与成功请求逐叶对照，定位触发内容。零出站请求。</p>
      </div>

      <div class="card p-6 mb-4">
        <div class="flex items-center justify-between mb-4">
          <h3 class="font-display text-[16px] font-medium">403 记录</h3>
          <div class="text-[12px] font-mono" id="wafListMeta" style="color: var(--muted);"></div>
        </div>
        <div class="overflow-x-auto">
          <table class="data-table">
            <thead><tr><th>ID</th><th>时间</th><th>账号</th><th>出站体</th><th>错误</th><th></th></tr></thead>
            <tbody id="wafListBody"></tbody>
          </table>
        </div>
        <div class="flex items-center gap-2 mt-4" id="wafPager"></div>
      </div>

      <div class="card p-6" id="wafAnalysis" style="display:none;">
        <div class="flex items-center justify-between mb-4 flex-wrap gap-3">
          <h3 class="font-display text-[16px] font-medium">分析结果 <span class="font-mono text-[13px]" id="wafLogMeta" style="color: var(--muted);"></span></h3>
          <div class="flex items-center gap-2">
            <span class="text-[12px]" style="color: var(--muted);">对照</span>
            <select id="wafBaselineSel" class="input" style="width:auto;" onchange="wafSetBaseline(this.value)"></select>
          </div>
        </div>
        <div id="wafAnalysisBody"></div>
      </div>
    </div>
  </section>
```

- [ ] **Step 2: sidebar 条目**

`sidebar.html` 「数据查询」条目（`data-page="sql"` 的 div）之后插入：

```html
      <div class="sidebar-item" data-page="waf" onclick="switchPage('waf')">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="m9 12 2 2 4-4"/></svg>
        WAF 检测
      </div>
```

- [ ] **Step 3: dashboard.js 接线（四处）**

1. `bootstrapDashboard` 的 names 数组：`'fragments/page-sql.html'` 后插入 `'fragments/page-waf.html'`；
2. `switchPage` 的 names 映射加 `waf:'WAF 检测'`；
3. `switchPage` 函数体加 `if (page === 'waf') wafRefresh();`；
4. `state` 加 `waf: { list: [], page: 1, total: 0, currentId: 0, baselineId: '', analysis: null }`。

- [ ] **Step 4: dashboard.js 渲染逻辑（文件末尾 SQL 控制台区块之前插入）**

```js
  // ─── WAF 检测（403 离线分析）─────────────────────────────
  var WAF_PAGE_SIZE = 15;
  window.wafRefresh = function () {
    var body = document.getElementById('wafListBody'); if (!body) return;
    // 403 列表用 SQL 只读通道取（复用 store 的过滤条件，列表只拉元数据不拉 body）
    api('/api/sql-query', { method: 'POST', body: JSON.stringify({ sql:
      "SELECT id, datetime(substr(created_at,1,19)) AS t, COALESCE((SELECT email FROM accounts WHERE id=account_id),'') AS email, " +
      "request_bytes, error_message FROM request_logs " +
      "WHERE status='error' AND error_message LIKE '%Cloudflare%' ORDER BY id DESC LIMIT " + WAF_PAGE_SIZE + " OFFSET " + ((state.waf.page - 1) * WAF_PAGE_SIZE)
    }) }).then(function (data) {
      renderWafList(data.rows || []);
    }).catch(function (e) { toast('403 列表加载失败：' + e.message); });
  };
  function renderWafList(rows) {
    var body = document.getElementById('wafListBody'); if (!body) return;
    body.innerHTML = rows.map(function (r) {
      return '<tr><td class="font-mono">' + esc(r.id) + '</td><td class="font-mono">' + esc(r.t) + '</td><td>' + esc(r.email) + '</td>' +
        '<td class="font-mono">' + fmt(r.request_bytes) + 'B</td>' +
        '<td style="max-width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="' + esc(r.error_message) + '">' + esc(r.error_message) + '</td>' +
        '<td><button class="btn btn-ghost text-[12px]" onclick="wafAnalyze(' + r.id + ')">分析</button></td></tr>';
    }).join('');
    var meta = document.getElementById('wafListMeta');
    if (meta) meta.textContent = '第 ' + state.waf.page + ' 页';
  }
  window.wafPage = function (p) { state.waf.page = Math.max(1, p); wafRefresh(); };
  window.wafAnalyze = function (logId) {
    state.waf.currentId = logId;
    state.waf.baselineId = '';
    wafRunAnalyze();
    api('/api/waf/baselines?log_id=' + logId).then(function (data) {
      var sel = document.getElementById('wafBaselineSel'); if (!sel) return;
      sel.innerHTML = '<option value="">自动挑选</option>' + (data.data || []).map(function (c) {
        return '<option value="' + c.id + '">#' + c.id + ' · ' + fmt(c.requestBytes) + 'B · ' + esc(c.accountEmail || '') + '</option>';
      }).join('');
    }).catch(function () {});
  };
  window.wafSetBaseline = function (v) { state.waf.baselineId = v; wafRunAnalyze(); };
  function wafRunAnalyze() {
    var url = '/api/waf/analyze?log_id=' + state.waf.currentId + (state.waf.baselineId ? '&baseline_id=' + state.waf.baselineId : '');
    api(url).then(function (data) {
      state.waf.analysis = data;
      renderWafAnalysis(data);
    }).catch(function (e) { toast('分析失败：' + e.message); });
  }
  function renderWafAnalysis(d) {
    var box = document.getElementById('wafAnalysis'); if (!box) return;
    box.style.display = '';
    var meta = document.getElementById('wafLogMeta');
    if (meta) meta.textContent = '#' + d.log.id + ' · ' + fmt(d.log.requestBytes) + 'B · ' + esc(d.log.accountEmail || '');
    var html = '';
    // 1. 签名扫描
    var sigs = d.signatureCounts || {};
    var sigKeys = Object.keys(sigs);
    html += '<div class="mb-5"><div class="text-[12px] font-semibold mb-2" style="color:var(--muted);">签名扫描</div>';
    html += sigKeys.length
      ? '<div class="flex gap-2 flex-wrap">' + sigKeys.map(function (k) {
          return '<span class="tag tag-red font-mono">' + esc(k) + ' ×' + sigs[k] + '</span>';
        }).join('') + '</div>'
      : '<span class="tag tag-gray">零特征（已知签名表未命中——可能是未知特征，看下方 diff）</span>';
    html += '</div>';
    // 2. 对照 diff
    html += '<div class="mb-5"><div class="text-[12px] font-semibold mb-2" style="color:var(--muted);">对照 diff' +
      (d.baseline ? '（对照 #' + d.baseline.id + ' · ' + esc(d.baseline.tier || '') + '）' : '（无对照可选）') + '</div>';
    var diffs = d.diff || [];
    if (!d.baseline) {
      html += '<div class="text-[13px]" style="color:var(--muted);">没有可用的成功对照记录（该请求之前无成功请求），可在上方下拉手动指定对照。</div>';
    } else if (!diffs.length) {
      html += '<div class="text-[13px]" style="color:var(--muted);">与对照逐叶一致——内容形状排除，指向 IP/账号/速率维度。</div>';
    } else {
      html += diffs.map(function (x) {
        return '<details class="card p-3 mb-2"><summary class="cursor-pointer font-mono text-[12px]">' +
          '<span class="tag ' + (x.kind === 'added' ? 'tag-red' : x.kind === 'removed' ? 'tag-gray' : 'tag-amber') + '">' + esc(x.kind) + '</span> ' +
          esc(x.path) + ' <span style="color:var(--muted);">' + (x.baselineLen || 0) + 'B → ' + (x.targetLen || 0) + 'B</span></summary>' +
          '<pre class="text-[11px] mt-2 p-2 overflow-x-auto" style="background:var(--bg-2);border-radius:6px;">' +
          (x.baselinePreview ? '对照: ' + esc(x.baselinePreview) + '\n\n' : '') +
          '目标: ' + esc(x.targetPreview || '(删除)') + '</pre></details>';
      }).join('');
    }
    html += '</div>';
    // 3. 体积画像
    var buckets = d.sizeBuckets || [];
    if (buckets.length) {
      html += '<div><div class="text-[12px] font-semibold mb-2" style="color:var(--muted);">当天体积分布（10KB 分桶）</div>' +
        '<table class="data-table"><thead><tr><th>桶</th><th>成功</th><th>失败</th><th>最大成功</th></tr></thead><tbody>' +
        buckets.map(function (b) {
          return '<tr><td class="font-mono">' + esc(b.bucket) + '</td><td>' + fmt(b.ok) + '</td><td>' + fmt(b.fail) + '</td><td class="font-mono">' + (b.maxOk ? fmt(b.maxOk) + 'B' : '-') + '</td></tr>';
        }).join('') + '</tbody></table></div>';
    }
    var el = document.getElementById('wafAnalysisBody');
    if (el) el.innerHTML = html;
  }
```

注意：列表页故意不显示签名计数——算签名要拉整条 upstream_body（60KB+），列表只做入口；签名块在分析面板里完整呈现。

- [ ] **Step 5: 手动验证（浏览器）**

Run: `go run . &`（或项目现有启动方式），浏览器开面板 → 「WAF 检测」：
- 列表显示 403 行（本地库若无 Cloudflare 行，用 SQL 控制台造一条：`INSERT INTO request_logs (...)`——只读通道造不了，改用 curl 打一次 `/v1/messages` 触发真实 403，或临时改 `newTestStore` 风格的单测已覆盖）
- 点「分析」→ 三块内容渲染、下拉可换对照、差异叶子可展开

Expected: 页面无 JS 报错（控制台干净）、分析块完整渲染

- [ ] **Step 6: 提交**

```bash
git add internal/dashboard/static/fragments/page-waf.html internal/dashboard/static/fragments/sidebar.html internal/dashboard/static/dashboard.js
git commit -m "dashboard: WAF detection page (403 list, signature scan, leaf diff, size buckets)"
```

---

### Task 7: 真实案例端到端验收 + 手册更新

**Files:**
- Modify: `docs/403-waf-neutralization.md`（第 6 节开头加一句指引）
- Test: 用 2026-09-11 的真实数据验收（不新增自动化测试——数据在生产库，验收靠运行中的实例）

**Interfaces:**
- Consumes: 全部前序任务

- [ ] **Step 1: 用真实 bin/cat 案例验收**

部署新构建后（或本地起服务连生产库副本），面板 → WAF 检测：
1. 列表找到 2026-09-11 14:15 的 b577d7cb 对应日志行（error_message 含 Cloudflare）；
2. 点「分析」：
   - 签名扫描显示 `bin/cat ×1`（若中和未上线时存的是原始 body）或零特征（中和已上线、body 里是 ZWSP 形态）；
   - 对照 diff：自动对照应选到 14:15:31 的成功请求（同会话），diff 结果应只有 4 个叶子，其中 `input.toolResponses[0].content.message` 一行 `93B → 4136B`；
   - 体积分布表渲染正常。
3. 对照下拉手动换一条成功记录，diff 刷新。

Expected: 与本文档开头人工排查的结论一致（差异锁定在 toolResponses[0].content.message）。

- [ ] **Step 2: 手册加指引**

`docs/403-waf-neutralization.md` 第 6 节标题下（`面板 → SQL 查询控制台。` 一行）改为：

```markdown
面板 → 「WAF 检测」页（离线分析：签名扫描 + 对照 diff + 体积画像，自动执行本节第一、二步）；
细节排查仍可用 SQL 查询控制台。
```

- [ ] **Step 3: 全量回归 + 提交**

Run: `go vet ./... && go test ./... -count=1`
Expected: 全绿

```bash
git add docs/403-waf-neutralization.md
git commit -m "docs: point 403 runbook at the new WAF detection page"
```

---

## 验收清单（对照 spec §1/§2）

- [ ] 加新签名只改 `wafSignatureProbes` 一处：面板 SQL 预设若需引用，前端从 `/api/waf-signatures` 取（Task 4 完成后，dashboard.js 的 403 预设可后续跟进改造——本期不强制，spec §1 的硬性要求是 store 统计不再手写，已由 Task 2 保证）
- [ ] 403 列表 + 签名扫描 + 逐叶 diff + 体积画像四块全部可用
- [ ] 对照组自动三级回退 + 手动覆盖
- [ ] 零出站请求（分析全程只读 SQLite）
- [ ] 真实 bin/cat 案例验收通过

## Phase B（§3）说明

探针二分（叶子轮 + 行级二分轮 + 后台 job + 轮询）在本期验收通过后另写计划 `docs/superpowers/plans/2026-09-11-waf-probe-bisect.md`，依赖本期的 `leafDiff`/`LeafDiff` 与分析页的叶子勾选 UI。

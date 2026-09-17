# 排查端点开放 API Key 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让任何有效 API Key 能调用 WAF 检测/SQL 查询等排查端点，外部排查代理（Claude）持 Key 自助取数。

**Architecture:** 在 `auth()` 的既有 `accountsAPI` 分流旁加 `opsAPI` 分流（先 Key 后会话，同款先例），不做 Key scope；openapi.yaml 补 `Ops` tag 的 8 个 path。

**Tech Stack:** Go stdlib (net/http ServeMux)、SQLite store、OpenAPI 3.0.3 yaml。

**Spec:** `docs/superpowers/specs/2026-09-17-ops-api-key-design.md`

## Global Constraints

- 鉴权顺序与 `/api/accounts*` 严格一致：先 `resolveKey`，验不过再走会话；错误契约 401 `invalid_api_key`（openapi.yaml 承诺）。
- 引导态语义不变：api_keys 表为空时 `resolveKey` 返回 `(nil, nil)` 放行。
- 放行清单是白名单——`/api/keys`、`/api/settings`、`/api/accounts*` 之外的写端点一律不得进清单。
- 测试命令：`go test ./internal/api/ -count=1`；提交信息以 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>` 结尾。

---

### Task 1: auth 放行排查端点 + 测试

**Files:**
- Modify: `internal/api/api.go`（`auth()` 约 136-170 行，加 `opsAPI` 分流）
- Modify: `internal/api/apikeys_test.go`（追加测试）

**Interfaces:**
- Consumes: `s.resolveKey(r)`（apikeys.go:76，签名不变）
- Produces: `opsReadOnlyAPI(path string) bool`——Task 3 的 openapi.yaml 与后续维护者依赖此清单函数

- [ ] **Step 1: 写失败测试**

追加到 `internal/api/apikeys_test.go` 末尾：

```go
// TestOpsAPIKeyAuth 钉住排查端点的 Key 放行：有效 Key 直接过（先 Key 后会话），
// 无 Key + 已设密码时 401，清单外端点不得被 Key 放行。
func TestOpsAPIKeyAuth(t *testing.T) {
	srv := &Server{Store: newTestStore(t)}
	if _, err := srv.Store.CreateAPIKey("sk-ops", "排查", nil, 0, 0, 1); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PASSWORD", "pw") // 开登录：逼出「只认会话」的默认分支

	ops := []struct{ method, path string }{
		{"GET", "/api/waf/analyze?log_id=1"},
		{"GET", "/api/waf/baselines?log_id=1"},
		{"GET", "/api/waf-signatures"},
		{"POST", "/api/waf/probe"},
		{"GET", "/api/waf/probe/probe-1"},
		{"DELETE", "/api/waf/probe/probe-1"},
		{"POST", "/api/sql-query"},
		{"GET", "/api/request-logs"},
		{"GET", "/api/logs"},
		{"GET", "/api/stats"},
	}
	for _, c := range ops {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer sk-ops")
		w := httptest.NewRecorder()
		if !srv.auth(w, req) {
			t.Fatalf("%s %s 带有效 Key 应放行，got %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	// 无 Key：401（已设密码、无会话）。
	req := httptest.NewRequest("POST", "/api/sql-query", nil)
	w := httptest.NewRecorder()
	if srv.auth(w, req) || w.Code != 401 {
		t.Fatalf("无 Key 应 401，got ok=%v code=%d", srv.auth(w, req), w.Code)
	}
	// 清单外端点不得被 Key 放行（敏感管理面）。
	for _, p := range []string{"/api/keys", "/api/settings", "/api/analytics", "/api/proxy-check"} {
		req := httptest.NewRequest("GET", p, nil)
		req.Header.Set("Authorization", "Bearer sk-ops")
		w := httptest.NewRecorder()
		if srv.auth(w, req) {
			t.Fatalf("%s 不得被 Key 放行", p)
		}
	}
	// 引导态（无密码、无 Key）：放行不变。
	boot := &Server{Store: newTestStore(t)}
	if !boot.auth(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/sql-query", nil)) {
		t.Fatal("引导态应开放")
	}
}
```

注意：第二个断言里 `srv.auth` 被调用两次会写两次响应——改成先存结果再判：

```go
	ok := srv.auth(w, req)
	if ok || w.Code != 401 {
		t.Fatalf("无 Key 应 401，got ok=%v code=%d", ok, w.Code)
	}
```

（计划里的代码块以本修正版为准。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/api/ -run TestOpsAPIKeyAuth -count=1`
Expected: FAIL——ops 端点 401（`opsReadOnlyAPI` 未定义会直接编译失败，同样算失败证据）。

- [ ] **Step 3: 最小实现**

`internal/api/api.go` 的 `auth()` 里，把：

```go
		accountsAPI := strings.HasPrefix(r.URL.Path, "/api/accounts")
		if accountsAPI {
			if _, err := s.resolveKey(r); err == nil {
				return true
			}
		}
```

改为：

```go
		accountsAPI := strings.HasPrefix(r.URL.Path, "/api/accounts")
		opsAPI := opsReadOnlyAPI(r.URL.Path)
		if accountsAPI || opsAPI {
			if _, err := s.resolveKey(r); err == nil {
				return true
			}
		}
```

并把下面两处 `accountsAPI` 的用途保持不变（`accountsAPI` 分支的错误体仍是 `invalid_api_key`；`opsAPI` 走同样的 401 错误体——把 `if accountsAPI` 的两处条件改为 `if accountsAPI || opsAPI`，错误消息与 type 沿用 `invalid_api_key`，与 openapi.yaml 契约一致）。

包内新增清单函数（放 `api.go` 的 `auth()` 上方）：

```go
// opsReadOnlyAPI 报告路径是否属于「排查类」端点：只读分析 + SQL 只读查询 + WAF
// 在线探针。任何有效 API Key 可调（与 /api/accounts* 同款先例：先 Key 后会话），
// 供外部排查代理自助取数（见 docs/superpowers/specs/2026-09-17-ops-api-key-design.md）。
// 白名单制：清单外端点（含 /api/keys、/api/settings 等敏感管理面）不得被 Key 放行。
func opsReadOnlyAPI(path string) bool {
	switch path {
	case "/api/waf/analyze", "/api/waf/baselines", "/api/waf-signatures",
		"/api/waf/probe", "/api/sql-query", "/api/request-logs", "/api/logs", "/api/stats":
		return true
	}
	return strings.HasPrefix(path, "/api/waf/probe/") // job_id 子路径（GET 进度 / DELETE 中止）
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go vet ./... && go test ./internal/api/ -count=1`
Expected: 全部 PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/api/api.go internal/api/apikeys_test.go
git commit -m "api: allow API key auth for ops read-only endpoints

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: openapi.yaml 补 Ops 端点

**Files:**
- Modify: `openapi.yaml`（info.description 鉴权说明；tags 加 Ops；paths 追加 8 个 path）

**Interfaces:**
- Consumes: Task 1 的 `opsReadOnlyAPI` 清单（yaml 与之一一对应）
- Produces: 对外契约文档（无代码接口）

- [ ] **Step 1: info 与 tags 更新**

`info.description` 的鉴权段落把「所有 `/api/accounts*` 端点支持两种请求头」改为「所有 `/api/accounts*` 与排查类端点（WAF 检测、SQL 查询、请求日志等，见 Ops tag）支持两种请求头」；`tags:` 列表加：

```yaml
  - name: Ops
    description: 运维排查只读端点 + WAF 在线探针（任何有效 API Key 可调，与 Accounts 同款鉴权）
```

- [ ] **Step 2: 追加 paths（插在 Accounts paths 之后、components 之前）**

```yaml
  /api/waf-signatures:
    get:
      tags: [Ops]
      summary: WAF 签名子串表
      description: 返回 403 取证用的特征子串表副本（全小写、大小写不敏感计数）。
      responses:
        "200":
          description: 签名列表
          content:
            application/json:
              schema:
                type: object
                properties:
                  signatures:
                    type: array
                    items: { type: string }
        "500": { $ref: "#/components/responses/InternalError" }

  /api/waf/baselines:
    get:
      tags: [Ops]
      summary: 指定失败日志的对照候选
      description: 返回与目标 403 行可对照的成功日志候选（自动三级回退 + 手动下拉源）。
      parameters:
        - { name: log_id, in: query, required: true, schema: { type: integer } }
      responses:
        "200":
          description: 对照候选列表
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: array
                    items:
                      type: object
                      properties:
                        id: { type: integer }
                        createdAt: { type: string }
                        requestBytes: { type: integer }
                        accountEmail: { type: string }
        "400": { $ref: "#/components/responses/InvalidRequest" }
        "404": { $ref: "#/components/responses/NotFound" }

  /api/waf/analyze:
    get:
      tags: [Ops]
      summary: 对一条 403 日志做离线分析
      description: 返回签名计数、对照 diff（baseline_id 缺省三级回退自动挑）、当天体积分桶。零出站请求。
      parameters:
        - { name: log_id, in: query, required: true, schema: { type: integer } }
        - { name: baseline_id, in: query, required: false, schema: { type: integer } }
      responses:
        "200":
          description: 分析结果（signatureCounts / baseline / diff / sizeBuckets）
          content:
            application/json:
              schema:
                type: object
                properties:
                  signatureCounts:
                    type: object
                    additionalProperties: { type: integer }
                  baseline:
                    type: object
                    nullable: true
                  diff:
                    type: array
                    items:
                      type: object
                      properties:
                        path: { type: string }
                        kind: { type: string, enum: [added, changed, removed] }
                        baselineLen: { type: integer }
                        targetLen: { type: integer }
                        baselinePreview: { type: string }
                        targetPreview: { type: string }
                  sizeBuckets:
                    type: array
                    items: { type: object }
        "400": { $ref: "#/components/responses/InvalidRequest" }
        "404": { $ref: "#/components/responses/NotFound" }

  /api/waf/probe:
    post:
      tags: [Ops]
      summary: 发起 WAF 在线探针（叶子二分）
      description: |
        对差异叶子逐个/二分向上游发探针请求验证触发行。单飞：已有 job 运行中回 409。
        job 不持久化（重启即丢）。removed 叶子不可探测；叶子全文超上游 query 上限回 400。
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [log_id, baseline_id, paths]
              properties:
                log_id: { type: integer }
                baseline_id: { type: integer }
                paths:
                  type: array
                  items: { type: string }
                  description: 差异叶子路径（waf/analyze 的 diff[].path）
                account_id: { type: integer, description: 缺省取首个活跃账号 }
                model: { type: string, description: 缺省 claude-opus-4-8 }
      responses:
        "200":
          description: job 已发起
          content:
            application/json:
              schema:
                type: object
                properties:
                  job_id: { type: string }
        "400": { $ref: "#/components/responses/InvalidRequest" }
        "404": { $ref: "#/components/responses/NotFound" }
        "409": { $ref: "#/components/responses/InvalidRequest" }

  /api/waf/probe/{job_id}:
    get:
      tags: [Ops]
      summary: 探针 job 进度轮询
      parameters:
        - { name: job_id, in: path, required: true, schema: { type: string } }
      responses:
        "200":
          description: job 快照（status/phase/variants/hits/conclusions/summary）
          content:
            application/json:
              schema: { type: object }
        "404": { $ref: "#/components/responses/NotFound" }
    delete:
      tags: [Ops]
      summary: 中止探针 job
      parameters:
        - { name: job_id, in: path, required: true, schema: { type: string } }
      responses:
        "200":
          description: 已中止
          content:
            application/json:
              schema: { type: object, properties: { success: { type: boolean } } }
        "404": { $ref: "#/components/responses/NotFound" }

  /api/sql-query:
    post:
      tags: [Ops]
      summary: SQLite 只读查询
      description: 仅接受 SELECT/WITH/EXPLAIN；写操作与 PRAGMA 一律拒绝。最多 200 行，单元格超长截断。
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [sql]
              properties:
                sql: { type: string }
      responses:
        "200":
          description: 查询结果
          content:
            application/json:
              schema:
                type: object
                properties:
                  columns: { type: array, items: { type: string } }
                  rows:
                    type: array
                    items: { type: array }
                  count: { type: integer }
                  truncated: { type: boolean }
                  elapsedMs: { type: integer }
        "400": { $ref: "#/components/responses/InvalidRequest" }

  /api/request-logs:
    get:
      tags: [Ops]
      summary: 请求日志（按指纹会话分组分页）
      parameters:
        - { name: page, in: query, schema: { type: integer, default: 1 } }
        - { name: pageSize, in: query, schema: { type: integer, default: 20, maximum: 100 } }
      responses:
        "200":
          description: 分组日志
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { type: array, items: { type: object } }
                  total: { type: integer }
                  page: { type: integer }
                  pageSize: { type: integer }

  /api/logs:
    get:
      tags: [Ops]
      summary: 最近调用日志
      responses:
        "200":
          description: 日志列表
          content:
            application/json:
              schema:
                type: object
                properties:
                  data: { type: array, items: { type: object } }

  /api/stats:
    get:
      tags: [Ops]
      summary: 网关统计
      responses:
        "200":
          description: 统计对象
          content:
            application/json:
              schema: { type: object }
```

- [ ] **Step 3: 校验 yaml 合法**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('openapi.yaml')); print('ok')"`
Expected: `ok`（若环境无 pyyaml 则目测缩进 + `git diff` 复核）。

- [ ] **Step 4: 提交**

```bash
git add openapi.yaml
git commit -m "docs: document ops endpoints in openapi.yaml

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 端到端冒烟（本机起服务，Key 实调）

**Files:** 无新文件（验证性任务）

**Interfaces:**
- Consumes: Task 1/2 全部产出
- Produces: 验证记录（无需提交物）

- [ ] **Step 1: 起服务 + 建号**

Run: `GATEWAY_PORT=1931 go run . &`（后台），等 `/health` 200。

- [ ] **Step 2: 实调验证**

```bash
# 引导态直接建一把 Key 需走面板；简化：引导态直接调排查端点（应 200）
curl -s -X POST localhost:1931/api/sql-query -d '{"sql":"SELECT 1"}' | head -c 200
curl -s "localhost:1931/api/waf-signatures" | head -c 200
curl -s "localhost:1931/api/waf/analyze?log_id=1" | head -c 200
# 清单外必须 401（无 Key、引导态开放时跳过此条）
curl -s "localhost:1931/api/keys" -o /dev/null -w '%{http_code}\n'
```

Expected: sql-query 返回 `{"columns":...,"rows":[["1"]]...}`；waf-signatures 返回签名数组；analyze 对不存在 id 返回 404（合法）；/api/keys 引导态 200（开放引导态不变，此项只确认服务活着）。

带 Key 路径已被 Task 1 的 `TestOpsAPIKeyAuth` 覆盖，冒烟只验服务装配无遗漏。

- [ ] **Step 3: 关服务**

`kill %1`。

---

## Self-Review 记录

- Spec 覆盖：auth 分流（Task 1）、yaml（Task 2）、测试五口径（Task 1 Step 1 含引导态/401/清单外）、白名单负例（/api/keys 等）——全覆盖。
- 占位符：无 TBD/TODO；所有代码块完整。
- 类型一致：`opsReadOnlyAPI(path string) bool` 在 Task 1 定义、Task 2 yaml 与其清单一一对应。

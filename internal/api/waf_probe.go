// waf_probe.go —— 面板「WAF 检测」的在线探针二分：把 repro403 实验的七轮手工二分
// 固化为后台 job。叶子轮逐个验证差异叶子，命中的叶子按行二分收敛到触发行。
// 铁律：显式人工发起（页面确认弹窗）、同一时刻仅一个 job、不持久化（重启即丢）、
// 不走 router（不占重试预算、不触发账号冷却、不写 request_logs）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// probePad 用良性散文把 s 填充到约 targetBytes 字节，保证各变体长度可比（方法论同
// repro403 实验的 padTo：唯一变量是内容形状，不是长度）。填充不含标记特征。
// 不做截断：探针内容必须完整，截断会切掉待验证特征。
func probePad(s string, targetBytes int) string {
	const filler = " The quick brown fox jumps over the lazy dog while nearby a calm river flows past green fields."
	var b strings.Builder
	b.WriteString(s)
	for b.Len() < targetBytes {
		// 简报原版整块追加 filler 会越过 targetBytes（filler 95 字节不整除差值），
		// 末块只补足差值以保证等长可比（与测试「精确 pad 到 200」一致）。
		if need := targetBytes - b.Len(); len(filler) > need {
			b.WriteString(filler[:need])
			break
		}
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

// isRunning 统一锁口径：status 属于 j.mu，读它必须先取 j.mu（abort handler 与
// 单飞检查共用，避免跨锁读的数据竞争）。
func (j *probeJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == "running"
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
		// 上游对 input.query 有 MaxUpstreamQueryRunes 硬校验：超长叶子会让所有变体
		// 变 other_error → 不命中 → 「全部叶子放行」的误导性假阴性，必须在发起前拒绝。
		// 前缀按 probePrefix 一个典型 nonce 的长度估算，保守留 100 字符余量。
		const probePrefixRunes = 100
		if utf8.RuneCountInString(v)+probePrefixRunes > provider.MaxUpstreamQueryRunes {
			jsonError(w, 400, fmt.Sprintf("叶子 %s 全文加前缀超出上游 %d 字符 query 上限，无法逐字探测", p, provider.MaxUpstreamQueryRunes), "invalid_request_error")
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
	if s.probe.current != nil && s.probe.current.isRunning() {
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
	if j.isRunning() {
		j.cancel()
	}
	// 已结束的 job 是 no-op：按实际状态返回，不谎报 aborted。
	j.mu.Lock()
	st := j.status
	j.mu.Unlock()
	jsonWrite(w, 200, map[string]interface{}{"status": st})
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
		blocked, other := 0, 0
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
				// pass 无需计数：blocked/other 皆零即 pass
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
	// stopIfCancelled 在阶段边界统一处理中止传播。trySend 的 select 在 ctx 已取消时
	// 仍可能随机选中 time.After 分支（两 case 同就绪）而跳过 aborted 置位，
	// 所以不能只靠 trySend：这里兜底设置状态后返回 true，调用方直接 return，
	// 严禁让后续阶段的 finish("done", ...) 覆盖成假结论。
	stopIfCancelled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		j.finish("aborted", "探针已中止")
		return true
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
	// 简报原版为 `if _, hit := trySend(...)`，但 trySend 仅返回 hit 一个值，
	// 编译不过；按原语义改为直接接收返回值。
	if hit := trySend("对照(等长纯文本)", probePad("Please summarize this note in one short sentence. nonce="+nextNonce()+". ", maxLen)); hit {
		j.finish("aborted", "对照变体（等长纯文本）也被 403——当前账号/出口处于风控窗口，探针结果不可信。请稍后重试或换账号。")
		return
	}
	// 中止传播：取消后直接退出，不产出假结论。
	if stopIfCancelled() {
		return
	}

	// 2) 叶子轮：每个勾选叶子单独成变体（基于对照，只注入该叶子内容）。
	j.setPhase("leaf")
	var hitLeaves []probeLeaf
	for _, l := range leaves {
		if stopIfCancelled() {
			return
		}
		// 前缀只取一次：两次 nextNonce() 恒等长，pad 到自身长度是 no-op，
		// 会让叶子变体与对照不等长。统一 pad 到 maxLen（对照组的长度）。
		prefix := probePrefix(nextNonce())
		if trySend("叶子 "+l.path, probePad(prefix+l.value, maxLen)) {
			hitLeaves = append(hitLeaves, l)
			j.mu.Lock()
			j.hits = append(j.hits, l.path)
			j.mu.Unlock()
		}
	}
	if stopIfCancelled() {
		return
	}
	if len(hitLeaves) == 0 {
		j.finish("done", "全部叶子放行——触发可能是组合特征或对照本身已含特征，建议换对照或转手工（repro403 实验）。")
		return
	}

	// 3) 行级二分：对每个命中叶子按行二分，~log2(行数) 轮收敛到触发行。
	j.setPhase("bisect")
	for _, l := range hitLeaves {
		if stopIfCancelled() {
			return
		}
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
		// 中止传播：取消后 bisectDescend 的两次 trySend 都是 ctx.Done 短路，
		// 其 ambiguous 结论不可信，不落盘。
		if stopIfCancelled() {
			return
		}
		j.mu.Lock()
		j.conclusions = append(j.conclusions, probeConclusion{
			Path: l.path, Lines: lines[lo:hi], Ambiguous: amb, Rounds: rounds,
		})
		j.mu.Unlock()
	}
	if stopIfCancelled() {
		return
	}
	j.finish("done", "探针完成：命中 "+fmt.Sprint(len(hitLeaves))+" 个叶子，结论见触发行列表。")
}

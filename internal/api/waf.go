// waf.go —— 面板「WAF 检测」页的端点：签名表暴露（单一事实源出口）、
// 对照候选列表、离线分析（analyze，见 Task 5）。全部只读、零出站请求。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"ps2api/internal/provider"
	"ps2api/internal/store"
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
	// ponytail: 按字节封顶 500（含省略号），rune 累加防止截半个字符
	n, end := 0, 0
	for end < len(r) {
		w := len(string(r[end]))
		if n+w+len("…") > leafPreviewMax {
			break
		}
		n += w
		end++
	}
	return string(r[:end]) + "…"
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

// ── 探针支撑：按路径取叶子全文 ──────────────────────────────

// leafSeg 是 leafDiff 路径的一段：对象键或数组下标。
type leafSeg struct {
	key     string
	index   int
	isIndex bool
}

// parseLeafPath 把 leafDiff 产出的路径（"input.query"、"toolResponses[0].content.message"）
// 解析成段序列，供 leafValue 定位叶子。畸形路径（缺 ]、下标非数字）返回 nil——
// 只截断到已解析前缀会让 "arr[x]" 误命中前缀节点 arr，故显式判畸形。
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
				return nil
			}
			n, err := strconv.Atoi(strings.TrimSpace(p[i+1 : i+j]))
			if err != nil {
				return nil
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
	segs := parseLeafPath(path)
	if segs == nil {
		return "", false
	}
	for _, seg := range segs {
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

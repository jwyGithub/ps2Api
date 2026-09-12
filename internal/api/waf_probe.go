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

// 供 Go 编译器确认 context 会被后续任务使用而保留的占位引用将在 Task 4 删除。
var _ = context.Background

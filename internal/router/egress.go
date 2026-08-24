package router

// nextEgressSeq 计算「当前账号」本次出站应使用的出口序号（喂给 provider 的 EgressAttempt）。
// 它把出口序号与全局 attempt 计数解耦：
//   - 首次尝试：0（用账号粘性出口）。
//   - 同账号连续重试（curAcc==prevAcc）：prevSeq+1，经 (stickyBase+seq)%N 轮换到下一个代理出口 IP。
//   - 跨账号 failover 换号（curAcc!=prevAcc）：归零，让新账号从自身粘性出口重新走代理池——
//     绝不因全局重试数堆高使 seq>=N 而在 selectFor 里回退本机直连（换号多因 403，直连必再被拦）。
func nextEgressSeq(prevSeq int, prevAcc, curAcc int64, firstAttempt bool) int {
	if firstAttempt || curAcc != prevAcc {
		return 0
	}
	return prevSeq + 1
}

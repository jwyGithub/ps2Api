package pool

import (
	"path/filepath"
	"testing"
	"time"

	"ps2api/internal/store"
)

// newTestStore 建一个临时库并写入 3 个 active 账号，返回 store 与 email→id 映射。
func newTestStore(t *testing.T) (*store.Store, map[string]int64) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "pool_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ids := map[string]int64{}
	for _, email := range []string{"a1@test.com", "a2@test.com", "a3@test.com"} {
		acc, err := s.UpsertAccount(email, "", `{"access_token":"tok","user_id":"u","workspace_id":"w"}`, "manual")
		if err != nil {
			t.Fatal(err)
		}
		ids[email] = acc.ID
	}
	return s, ids
}

// 403 换号应优先切到剩余频率额度(RateRemaining)最高的账号；排除首选后应退而求其次选次高者。
func TestNextByRateRemainingPrefersHighestQuota(t *testing.T) {
	s, ids := newTestStore(t)
	// a1=30/30(满)，a2=10/30，a3=25/30
	if err := s.SetRateLimit(ids["a1@test.com"], 30, 30, 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRateLimit(ids["a2@test.com"], 30, 10, 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRateLimit(ids["a3@test.com"], 30, 25, 60, nil); err != nil {
		t.Fatal(err)
	}
	p := New(s)

	// 无排除：应选剩余额度最满的 a1(30/30)。
	acc, err := p.NextByRateRemaining(nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != ids["a1@test.com"] {
		t.Fatalf("expected highest-remaining account a1(%d), got %d", ids["a1@test.com"], acc.ID)
	}
	p.Done(acc.ID)

	// 排除 a1(模拟其被 403 拦截并加入 excluded)：剩余的 a2(10)/a3(25) 中应选 a3。
	acc2, err := p.NextByRateRemaining(map[int64]bool{ids["a1@test.com"]: true})
	if err != nil {
		t.Fatal(err)
	}
	if acc2.ID != ids["a3@test.com"] {
		t.Fatalf("expected next-highest-remaining account a3(%d), got %d", ids["a3@test.com"], acc2.ID)
	}
}

// 各账号 RateLimit 不一致时，选号应按剩余「比例」而非绝对剩余值。
// 构造 a1=20/60(33%)、a2=10/10(100%)、a3=25/50(50%)：绝对剩余最大是 a3(25)，
// 但比例最高是 a2(100%)，应选 a2 以验证主键确实是比例。
func TestNextByRateRemainingUsesRatioNotAbsolute(t *testing.T) {
	s, ids := newTestStore(t)
	if err := s.SetRateLimit(ids["a1@test.com"], 60, 20, 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRateLimit(ids["a2@test.com"], 10, 10, 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRateLimit(ids["a3@test.com"], 50, 25, 60, nil); err != nil {
		t.Fatal(err)
	}
	p := New(s)

	acc, err := p.NextByRateRemaining(nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != ids["a2@test.com"] {
		t.Fatalf("expected highest-ratio account a2(%d), got %d", ids["a2@test.com"], acc.ID)
	}
}

// QuotaModeAbsolute 应按剩余额度绝对值选号（与比例序相悖时体现差异）。
// 同一构造 a1=20/60、a2=10/10、a3=25/50：比例最高是 a2(100%)，但绝对剩余最大是 a3(25)，
// absolute 模式应选 a3；off(RoundRobin) 模式则不看额度、走普通轮询。
func TestNextByQuotaAbsoluteVsRatio(t *testing.T) {
	s, ids := newTestStore(t)
	if err := s.SetRateLimit(ids["a1@test.com"], 60, 20, 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRateLimit(ids["a2@test.com"], 10, 10, 60, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRateLimit(ids["a3@test.com"], 50, 25, 60, nil); err != nil {
		t.Fatal(err)
	}
	p := New(s)

	// absolute：应选剩余绝对值最大的 a3(25)。
	acc, err := p.NextByQuota(nil, QuotaModeAbsolute)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != ids["a3@test.com"] {
		t.Fatalf("absolute mode: expected highest-remaining account a3(%d), got %d", ids["a3@test.com"], acc.ID)
	}
	p.Done(acc.ID)

	// ratio：同数据应选比例最高的 a2(100%)，确认两模式确有区别。
	acc2, err := p.NextByQuota(nil, QuotaModeRatio)
	if err != nil {
		t.Fatal(err)
	}
	if acc2.ID != ids["a2@test.com"] {
		t.Fatalf("ratio mode: expected highest-ratio account a2(%d), got %d", ids["a2@test.com"], acc2.ID)
	}
}

// 用量额度已耗尽（QuotaLimit>0 且 QuotaRemaining<=0）的账号，即便速率窗口最新鲜，
// 也不该在 403 换号里被选中——除非池内已无任何有额度账号可用（最后兜底轮才放行）。
func TestNextSkipsQuotaExhausted(t *testing.T) {
	s, ids := newTestStore(t)
	// 三个号速率都满 30/30；a1 用量额度耗尽（50000/0），a2、a3 仍有额度。
	for _, e := range []string{"a1@test.com", "a2@test.com", "a3@test.com"} {
		if err := s.SetRateLimit(ids[e], 30, 30, 60, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetQuotaSnapshot(ids["a1@test.com"], store.QuotaSnapshot{State: "AVAILABLE", Limit: 50000, Remaining: 0}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuotaSnapshot(ids["a2@test.com"], store.QuotaSnapshot{State: "AVAILABLE", Limit: 50000, Remaining: 40000}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuotaSnapshot(ids["a3@test.com"], store.QuotaSnapshot{State: "AVAILABLE", Limit: 50000, Remaining: 40000}); err != nil {
		t.Fatal(err)
	}
	p := New(s)

	// 403 换号：a1 速率虽满但额度见底，应被跳过，只会选到 a2 或 a3。
	for i := 0; i < 5; i++ {
		acc, err := p.NextByRateRemaining(nil)
		if err != nil {
			t.Fatal(err)
		}
		if acc.ID == ids["a1@test.com"] {
			t.Fatalf("iteration %d: quota-exhausted account a1(%d) should be skipped", i, ids["a1@test.com"])
		}
		p.Done(acc.ID)
	}

	// 普通轮询选号同样应跳过额度耗尽的 a1。
	for i := 0; i < 5; i++ {
		acc, err := p.Next(nil)
		if err != nil {
			t.Fatal(err)
		}
		if acc.ID == ids["a1@test.com"] {
			t.Fatalf("round-robin iteration %d: quota-exhausted account a1(%d) should be skipped", i, ids["a1@test.com"])
		}
		p.Done(acc.ID)
	}
}

// 当池内所有账号额度都耗尽时，最后兜底轮应放行（宁可一试也不硬失败，额度快照可能陈旧）。
func TestNextFallsBackWhenAllQuotaExhausted(t *testing.T) {
	s, ids := newTestStore(t)
	for _, e := range []string{"a1@test.com", "a2@test.com", "a3@test.com"} {
		if err := s.SetRateLimit(ids[e], 30, 30, 60, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.SetQuotaSnapshot(ids[e], store.QuotaSnapshot{State: "AVAILABLE", Limit: 50000, Remaining: 0}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(s)
	acc, err := p.NextByRateRemaining(nil)
	if err != nil {
		t.Fatalf("expected fallback to return an account when all exhausted, got err: %v", err)
	}
	if acc == nil {
		t.Fatal("expected a non-nil account from last-resort fallback")
	}
}

// 普通轮询选号应软避让「最近被占用」(Reserve) 的账号：三个号均空闲(load 0)时，
// 若首选位置的号被预留，应跳到未被预留的号，避免多客户端挤在同一账号上。
func TestNextAvoidsReservedAccountRoundRobin(t *testing.T) {
	s, ids := newTestStore(t)
	p := New(s)

	// 预留 a1（轮询起点 last=-1 → start=0 即 a1）。未预留时 Next 会命中 a1 立即返回；
	// 预留后应避开 a1，落到下一个未被占用的号。
	p.Reserve(ids["a1@test.com"], time.Minute)
	acc, err := p.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID == ids["a1@test.com"] {
		t.Fatalf("reserved account a1(%d) should be avoided when idle unreserved peers exist, got it", ids["a1@test.com"])
	}
}

// 预留是「软」避让：当所有账号都被预留时仍须返回一个账号（不硬失败），保证可用性。
func TestNextReturnsReservedWhenAllReserved(t *testing.T) {
	s, ids := newTestStore(t)
	p := New(s)
	for _, e := range []string{"a1@test.com", "a2@test.com", "a3@test.com"} {
		p.Reserve(ids[e], time.Minute)
	}
	acc, err := p.Next(nil)
	if err != nil {
		t.Fatalf("expected an account even when all reserved, got err: %v", err)
	}
	if acc == nil {
		t.Fatal("expected a non-nil account when all reserved (soft avoidance must not starve)")
	}
}

// Reserve 的时长 d<=0 时不预留（等价于 account_reservation_seconds=0 关闭该机制）：
// 此时 a1 不被避让，轮询起点仍会命中 a1。
func TestReserveZeroDurationIsNoop(t *testing.T) {
	s, ids := newTestStore(t)
	p := New(s)
	p.Reserve(ids["a1@test.com"], 0)
	acc, err := p.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != ids["a1@test.com"] {
		t.Fatalf("d<=0 must not reserve; expected round-robin start a1(%d), got %d", ids["a1@test.com"], acc.ID)
	}
}

// 过期的预留不再避让：预留窗口到期后，账号恢复为可正常命中的空闲号。
func TestReservationExpiresRestoresAvailability(t *testing.T) {
	s, ids := newTestStore(t)
	p := New(s)
	p.Reserve(ids["a1@test.com"], time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	acc, err := p.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != ids["a1@test.com"] {
		t.Fatalf("expired reservation should no longer avoid a1(%d); got %d", ids["a1@test.com"], acc.ID)
	}
}

package pool

import (
	"path/filepath"
	"testing"

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

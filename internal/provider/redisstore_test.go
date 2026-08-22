package provider

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedisStore 用 miniredis 起一个进程内假 Redis，返回接好的 store、mr 句柄与清理函数。
func newTestRedisStore(t *testing.T, ttl time.Duration) (*redisConversationStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("启动 miniredis 失败: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := newRedisConversationStore(rdb, "test", ttl)
	return store, mr
}

func TestRedisConversation_PutGet(t *testing.T) {
	store, _ := newTestRedisStore(t, time.Hour)

	if _, ok := store.GetConversation(1, "fp1"); ok {
		t.Fatal("空库应当 miss")
	}
	store.PutConversation(1, "fp1", "conv-1")
	got, ok := store.GetConversation(1, "fp1")
	if !ok || got != "conv-1" {
		t.Fatalf("期望 conv-1/true，得到 %q/%v", got, ok)
	}
	// 账号隔离：同指纹不同账号互不可见。
	if _, ok := store.GetConversation(2, "fp1"); ok {
		t.Fatal("跨账号不应命中")
	}
}

func TestRedisConversation_OwnerAndToolGroup(t *testing.T) {
	store, _ := newTestRedisStore(t, time.Hour)

	store.PutOwner("fp1", 42)
	id, ok := store.GetOwner("fp1")
	if !ok || id != 42 {
		t.Fatalf("期望 owner 42/true，得到 %d/%v", id, ok)
	}

	store.PutToolGroup(42, "tc-1", "group-1")
	g, ok := store.GetToolGroup(42, "tc-1")
	if !ok || g != "group-1" {
		t.Fatalf("期望 group-1/true，得到 %q/%v", g, ok)
	}
}

func TestRedisConversation_TTL(t *testing.T) {
	store, mr := newTestRedisStore(t, time.Minute)
	store.PutConversation(1, "fp1", "conv-1")

	// miniredis 需手动推进时间来触发过期。
	mr.FastForward(2 * time.Minute)
	if _, ok := store.GetConversation(1, "fp1"); ok {
		t.Fatal("TTL 过期后应当 miss")
	}
}

func TestRedisConversation_TTLZeroNeverExpires(t *testing.T) {
	store, mr := newTestRedisStore(t, 0)
	store.PutConversation(1, "fp1", "conv-1")

	mr.FastForward(1000 * time.Hour)
	if _, ok := store.GetConversation(1, "fp1"); !ok {
		t.Fatal("TTL=0 应当永不过期")
	}
}

// TestRedisConversation_Reset 覆盖 Reset 的核心语义：
// 只清目标账号名下的 conv + owner，其他账号一律不动；
// 且 owner 已被别的账号覆盖过的指纹，不得被误删。
func TestRedisConversation_Reset(t *testing.T) {
	store, _ := newTestRedisStore(t, time.Hour)

	// 账号 1 的会话与归属。
	store.PutConversation(1, "fpA", "conv-1A")
	store.PutConversation(1, "fpB", "conv-1B")
	store.PutOwner("fpA", 1)
	store.PutOwner("fpB", 1)

	// 账号 2 的会话与归属（应完好无损）。
	store.PutConversation(2, "fpC", "conv-2C")
	store.PutOwner("fpC", 2)

	// fpB 的归属随后被账号 2 覆盖：Reset(1) 不得删掉这条 owner（值已非 1）。
	store.PutOwner("fpB", 2)

	store.Reset(1)

	// 账号 1 的会话映射应全部清除。
	if _, ok := store.GetConversation(1, "fpA"); ok {
		t.Error("Reset 后账号 1 的 fpA 会话应被清除")
	}
	if _, ok := store.GetConversation(1, "fpB"); ok {
		t.Error("Reset 后账号 1 的 fpB 会话应被清除")
	}
	// 账号 2 的会话不受影响。
	if v, ok := store.GetConversation(2, "fpC"); !ok || v != "conv-2C" {
		t.Error("Reset 不应影响账号 2 的会话")
	}

	// fpA 归属值==1，应被删除。
	if _, ok := store.GetOwner("fpA"); ok {
		t.Error("Reset 后 fpA 的归属应被清除")
	}
	// fpB 归属已被账号 2 覆盖（值==2），不得误删。
	if id, ok := store.GetOwner("fpB"); !ok || id != 2 {
		t.Errorf("被账号 2 覆盖的 fpB 归属不应被误删，得到 %d/%v", id, ok)
	}
	// fpC 归属属于账号 2，应保留。
	if id, ok := store.GetOwner("fpC"); !ok || id != 2 {
		t.Errorf("Reset 不应影响账号 2 的 fpC 归属，得到 %d/%v", id, ok)
	}
}

func TestNewConversationStore_FallbackWhenNoRedis(t *testing.T) {
	t.Setenv("REDIS_URL", "")
	t.Setenv("REDIS_ADDR", "")
	if _, ok := newConversationStore().(*memoryConversationStore); !ok {
		t.Fatal("未配置 Redis 时应回退内存实现")
	}
}

func TestNewConversationStore_FallbackWhenBadURL(t *testing.T) {
	t.Setenv("REDIS_URL", "not-a-valid-url")
	if _, ok := newConversationStore().(*memoryConversationStore); !ok {
		t.Fatal("Redis 连接串非法时应回退内存实现")
	}
}

func TestNewConversationStore_UsesRedisWhenReachable(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("启动 miniredis 失败: %v", err)
	}
	defer mr.Close()

	t.Setenv("REDIS_URL", "redis://"+mr.Addr())
	store := newConversationStore()
	if _, ok := store.(*redisConversationStore); !ok {
		t.Fatalf("Redis 可达时应使用 Redis 实现，得到 %T", store)
	}
}

func TestRedisConvTTL_Parsing(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultConvTTL},
		{"30m", 30 * time.Minute},
		{"0", 0},
		{"never", 0},
		{"off", 0},
		{"garbage", defaultConvTTL},
	}
	for _, c := range cases {
		t.Setenv("REDIS_CONV_TTL", c.env)
		if got := redisConvTTL(); got != c.want {
			t.Errorf("REDIS_CONV_TTL=%q: 期望 %s，得到 %s", c.env, c.want, got)
		}
	}
}

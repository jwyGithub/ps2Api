package provider

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisConversationStore 是 ConversationStore 的 Redis 实现，用于多实例部署时共享
// 会话态（进程内 memoryConversationStore 无法跨实例，且只增不减）。
//
// 设计约束：
//   - 接口方法不带 context/error（第 3 步刻意保持调用方零改动），因此本实现每次操作
//     自建一个短超时 ctx，任一 Redis 故障都优雅降级——读操作退化为 miss（等价"新会话"，
//     语义安全），写/重置操作仅打 WARN 日志，绝不阻塞请求链路。会话存储本质是复用会话
//     的"最佳努力"优化，丢失时最坏结果是开一个新会话，不影响正确性。
//   - 所有键带可配 TTL（默认 defaultConvTTL），既满足多实例共享，又顺带解决内存实现
//     "只增不减"的无界增长问题——过期会话自动回收。TTL=0 表示永不过期。
//
// 键布局（prefix 默认 "ps2api"，与内存实现一样属实现私有细节，调用方无感知）：
//
//	{prefix}:conv:{accountID}:{fingerprint} -> conversationID
//	{prefix}:owner:{fingerprint}            -> accountID(十进制)   会话归属账号
//	{prefix}:ownset:{accountID}             -> SET{fingerprint}    归属反向索引（供 Reset）
//	{prefix}:tg:{accountID}:{toolCallID}    -> toolCallGroupId
type redisConversationStore struct {
	rdb       *redis.Client
	prefix    string
	ttl       time.Duration
	opTimeout time.Duration
	addr      string // 连接地址，仅用于启动日志展示
	db        int    // Redis 逻辑库号，仅用于启动日志展示
}

// Mode 返回人类可读的存储模式描述，供启动日志体现当前会话存储去向。
func (r *redisConversationStore) Mode() string {
	return "Redis (" + r.addr + " db=" + strconv.Itoa(r.db) + ", prefix=" + r.prefix + ", ttl=" + ttlLabel(r.ttl) + ")"
}

const (
	defaultConvTTL   = 72 * time.Hour        // 会话映射默认存活时长，可用 REDIS_CONV_TTL 覆盖
	defaultOpTimeout = 3 * time.Second        // 单次 Redis 操作超时上限，防止 Redis 卡顿拖垮请求
	scanCount        = 256                     // SCAN 每批游标步长
	defaultKeyPrefix = "ps2api"
)

func newRedisConversationStore(rdb *redis.Client, prefix string, ttl time.Duration) *redisConversationStore {
	if prefix == "" {
		prefix = defaultKeyPrefix
	}
	return &redisConversationStore{rdb: rdb, prefix: prefix, ttl: ttl, opTimeout: defaultOpTimeout}
}

func (r *redisConversationStore) opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), r.opTimeout)
}

func (r *redisConversationStore) convK(accountID int64, fp string) string {
	return r.prefix + ":conv:" + strconv.FormatInt(accountID, 10) + ":" + fp
}

func (r *redisConversationStore) ownerK(fp string) string {
	return r.prefix + ":owner:" + fp
}

func (r *redisConversationStore) ownSetK(accountID int64) string {
	return r.prefix + ":ownset:" + strconv.FormatInt(accountID, 10)
}

func (r *redisConversationStore) toolK(accountID int64, toolCallID string) string {
	return r.prefix + ":tg:" + strconv.FormatInt(accountID, 10) + ":" + toolCallID
}

func (r *redisConversationStore) GetConversation(accountID int64, fingerprint string) (string, bool) {
	ctx, cancel := r.opCtx()
	defer cancel()
	v, err := r.rdb.Get(ctx, r.convK(accountID, fingerprint)).Result()
	if err != nil {
		return "", false // redis.Nil(未命中) 或连接故障：一律退化为"无会话"，安全开新会话
	}
	return v, true
}

func (r *redisConversationStore) PutConversation(accountID int64, fingerprint, conversationID string) {
	ctx, cancel := r.opCtx()
	defer cancel()
	if err := r.rdb.Set(ctx, r.convK(accountID, fingerprint), conversationID, r.ttl).Err(); err != nil {
		log.Printf("WARN: Redis 写入会话映射失败(account=%d): %v", accountID, err)
	}
}

func (r *redisConversationStore) GetOwner(fingerprint string) (int64, bool) {
	ctx, cancel := r.opCtx()
	defer cancel()
	v, err := r.rdb.Get(ctx, r.ownerK(fingerprint)).Result()
	if err != nil {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func (r *redisConversationStore) PutOwner(fingerprint string, accountID int64) {
	ctx, cancel := r.opCtx()
	defer cancel()
	// 同时维护反向索引集合，供 Reset 只删归属本账号的指纹（Redis 无法按值删除）。
	pipe := r.rdb.Pipeline()
	pipe.Set(ctx, r.ownerK(fingerprint), accountID, r.ttl)
	pipe.SAdd(ctx, r.ownSetK(accountID), fingerprint)
	if r.ttl > 0 {
		pipe.Expire(ctx, r.ownSetK(accountID), r.ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("WARN: Redis 写入会话归属失败(account=%d): %v", accountID, err)
	}
}

func (r *redisConversationStore) GetToolGroup(accountID int64, toolCallID string) (string, bool) {
	ctx, cancel := r.opCtx()
	defer cancel()
	v, err := r.rdb.Get(ctx, r.toolK(accountID, toolCallID)).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

func (r *redisConversationStore) PutToolGroup(accountID int64, toolCallID, groupID string) {
	ctx, cancel := r.opCtx()
	defer cancel()
	if err := r.rdb.Set(ctx, r.toolK(accountID, toolCallID), groupID, r.ttl).Err(); err != nil {
		log.Printf("WARN: Redis 写入工具调用组失败(account=%d): %v", accountID, err)
	}
}

// Reset 清空某账号名下的会话映射与归属映射，语义与内存实现逐字节一致
// （只清 conv + owner，不动 toolGroups；orphan 的工具组键靠 TTL 自然过期）。
func (r *redisConversationStore) Reset(accountID int64) {
	ctx, cancel := r.opCtx()
	defer cancel()

	// 1) 会话映射：{prefix}:conv:{account}:* 前缀扫描删除（fingerprint 是 sha256 hex，无冒号，模式安全）。
	if err := r.scanDelete(ctx, r.convK(accountID, "*")); err != nil {
		log.Printf("WARN: Redis 重置会话映射失败(account=%d): %v", accountID, err)
	}

	// 2) 归属映射：借反向索引集合定位候选指纹，逐个校验 owner 当前值仍==本账号再删，
	//    复刻内存实现"仅删值==accountID"的语义，避免误删被其他账号覆盖过的指纹。
	setK := r.ownSetK(accountID)
	fps, err := r.rdb.SMembers(ctx, setK).Result()
	if err != nil {
		log.Printf("WARN: Redis 读取归属索引失败(account=%d): %v", accountID, err)
		return
	}
	want := strconv.FormatInt(accountID, 10)
	del := make([]string, 0, len(fps)+1)
	for _, fp := range fps {
		if v, err := r.rdb.Get(ctx, r.ownerK(fp)).Result(); err == nil && v == want {
			del = append(del, r.ownerK(fp))
		}
	}
	del = append(del, setK) // 反向索引本身一并回收
	if err := r.rdb.Del(ctx, del...).Err(); err != nil {
		log.Printf("WARN: Redis 删除会话归属失败(account=%d): %v", accountID, err)
	}
}

// scanDelete 用 SCAN 游标分批匹配并删除键，避免 KEYS 阻塞 Redis。
func (r *redisConversationStore) scanDelete(ctx context.Context, match string) error {
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, match, scanCount).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := r.rdb.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

// ─── 工厂与配置 ─────────────────────────────────────────────────

// newConversationStore 依配置选择会话存储实现：
//   - 设了 REDIS_URL（或 REDIS_ADDR）且探活成功 -> Redis 实现（多实例共享 + TTL 回收）。
//   - 未配置，或解析/连接失败 -> 回退进程内内存实现，保证网关永不因 Redis 不可用而拒服务。
func newConversationStore() ConversationStore {
	raw := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if raw == "" {
		// 兼容裸 host:port 写法（REDIS_ADDR），补上 scheme 交给 ParseURL。
		if addr := strings.TrimSpace(os.Getenv("REDIS_ADDR")); addr != "" {
			if strings.Contains(addr, "://") {
				raw = addr
			} else {
				raw = "redis://" + addr
			}
		}
	}
	if raw == "" {
		return newMemoryConversationStore()
	}

	opt, err := redis.ParseURL(raw)
	if err != nil {
		log.Printf("WARN: 解析 Redis 连接串失败，回退进程内会话存储: %v", err)
		return newMemoryConversationStore()
	}
	rdb := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("WARN: 连接 Redis 失败，回退进程内会话存储: %v", err)
		_ = rdb.Close()
		return newMemoryConversationStore()
	}
	store := newRedisConversationStore(rdb, redisKeyPrefix(), redisConvTTL())
	store.addr = opt.Addr
	store.db = opt.DB
	return store
}

func redisKeyPrefix() string {
	if v := strings.TrimSpace(os.Getenv("REDIS_KEY_PREFIX")); v != "" {
		return v
	}
	return defaultKeyPrefix
}

// redisConvTTL 读取 REDIS_CONV_TTL（Go duration 串，如 "72h"、"30m"）。
// "0"/"off"/"none"/"never" 表示永不过期；无效值回退默认。
func redisConvTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("REDIS_CONV_TTL"))
	if v == "" {
		return defaultConvTTL
	}
	switch strings.ToLower(v) {
	case "0", "off", "none", "never":
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	log.Printf("WARN: 无效的 REDIS_CONV_TTL=%q，回退默认 %s", v, defaultConvTTL)
	return defaultConvTTL
}

func ttlLabel(ttl time.Duration) string {
	if ttl <= 0 {
		return "never"
	}
	return ttl.String()
}

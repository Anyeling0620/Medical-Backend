package repo

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是公开域读缓存（T4b 排班缓存）的基础设施：
//   - CacheOptions：TTL / 抖动 / Redis 超时，由 config.CacheConfig.Normalize() 转换而来；
//   - cacheRedis：缓存实现真正依赖的 Redis 原语子集，便于用极小测试替身覆盖命中、未命中、
//     版本失效与故障注入（项目现有测试即用「内嵌 redis.UniversalClient 并覆写个别方法」的写法，
//     不引入 miniredis 之类的新依赖）；
//   - jitteredTTL / cacheContext：TTL 抖动与单次 Redis 操作超时。
//
// 与 T4a（目录缓存）的关系：T4a 在并行分支上使用独立的 PUBLIC_CATALOG_CACHE_* 配置与
// 自己的装饰器构造签名，两条分支合并时需要统一 CacheOptions 与配置前缀，
// 避免同一套 Redis 上并存两套语义不同的缓存配置。
//
// 降级方向（最容易照抄错的一点）：缓存是**性能依赖，不是正确性依赖**。
// Redis 出错时读路径必须 fail open 直查数据库，绝不能照抄幂等存储「不可用就返回 503」的做法
// （幂等是正确性依赖，方向相反，见 internal/repo/redis_idempotency.go）。

// defaultCacheRedisTimeout 是 CacheOptions 未指定超时或缺省值非法时的兜底超时，
// 与 config.DefaultCacheRedisTimeout 保持同一口径：慢 Redis 不得把业务请求一起拖慢。
const defaultCacheRedisTimeout = 100 * time.Millisecond

// CacheOptions 是公开域读缓存的运行参数。
type CacheOptions struct {
	// TTL 是缓存基准存活时间。
	TTL time.Duration
	// JitterRatio 是 TTL 抖动比例（0~0.5）：实际 TTL 落在 TTL*(1±JitterRatio) 内。
	JitterRatio float64
	// RedisTimeout 是单次缓存 Redis 操作的超时。
	RedisTimeout time.Duration
}

// cacheRedis 是缓存实现依赖的 Redis 原语子集，只收窄到实际用到的四个命令：
// 实现里因此不可能「顺手」用上 SCAN/KEYS 这类会阻塞 Redis 的高危命令。
type cacheRedis interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	MGet(ctx context.Context, keys ...string) *redis.SliceCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Incr(ctx context.Context, key string) *redis.IntCmd
}

// jitteredTTL 返回带随机抖动的 TTL：base*(1-jitter+rand*2*jitter)。
// 抖动用于避免同一时刻写入的键集体过期造成雪崩；base 非法时返回 0，
// 调用方必须按「不写缓存」处理（Redis 的 0 是永不过期，绝不能用）。
func jitteredTTL(base time.Duration, jitter float64, randFloat func() float64) time.Duration {
	if base <= 0 {
		return 0
	}
	if jitter <= 0 || randFloat == nil {
		return base
	}
	if jitter > 1 {
		jitter = 1
	}
	ttl := time.Duration(float64(base) * (1 - jitter + randFloat()*2*jitter))
	if ttl < time.Millisecond {
		ttl = time.Millisecond
	}
	return ttl
}

// cacheContext 为单次缓存 Redis 操作加超时：慢 Redis 不得把业务请求一起拖慢。
func cacheContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultCacheRedisTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

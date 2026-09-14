package repo

// 本文件把匿名公开域（/api/v1/public/*）的目录读路径（科室 / 子科室 / 医生）包装成
// 「逻辑过期」缓存，作为 port.PublicCatalogRepository 的装饰器叠在 Postgres 实现之上。
//
// 为什么用装饰器而不是改 public_catalog.go：公开域与管理端 catalog 域共用同一个
// PostgresDoctorRepository，而管理端必须始终读到最新数据（基础资料当前没有写接口，
// 但运营会直接改库，后续也可能补管理端维护接口）。把缓存收在包装类型里，bootstrap 只把
// 包装传给 publiccatalog 用例，管理端仍旧拿原始仓储，缓存从结构上就不可能影响管理端。
//
// 缓存语义是「逻辑过期」而不是「物理过期即回源」：
//  1. 物理 TTL 取逻辑 TTL 的 2 倍：逻辑过期后旧值仍在 Redis 里，「先返回旧值、异步重建」
//     才有旧值可返回。若物理过期与逻辑过期同时发生，过期瞬间的每个请求都要在回源上排队，
//     热点 key 依旧会被击穿。
//  2. 命中且未逻辑过期：直接返回缓存值。
//  3. 命中但已逻辑过期：立刻返回旧值，并在后台 goroutine 中回源重建。
//  4. 未命中：同步回源并回填。
//  5. 上面两条回源路径共用 singleflight，同一 key 只回源一次。
//
// 降级方向：读缓存是性能依赖而不是正确性依赖。Redis 读取失败时直接查库（fail open），
// 不能照抄幂等存储那种 fail closed（返回 5xx）的做法，否则缓存反而把可用性拖低。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/port"
)

const (
	// publicCatalogCacheKeyPrefix 与既有 medical:auth:*、medical:idem:* 前缀风格保持一致。
	publicCatalogCacheKeyPrefix = "medical:cache:public:"
	// publicCatalogCacheKeyVersion 是键结构版本号：载荷结构或键维度变更时递增即可整体失效，
	// 不必去 SCAN 删除旧键。
	publicCatalogCacheKeyVersion = "v1"
	// publicCatalogCachePhysicalFactor 是物理 TTL 相对逻辑 TTL 的倍数，取值理由见文件头注释。
	publicCatalogCachePhysicalFactor = 2
	// publicCatalogCacheMissTTL 是「查不到」空值标记的存活时间：只用来挡住短时间内的重复
	// 无效 ID，不能把「此刻不存在」这个结论像正常数据那样固化。
	publicCatalogCacheMissTTL = 60 * time.Second
	// publicCatalogCacheLoadTimeout 是「脱离请求上下文回源」的安全上界，同步合并回源与异步重建
	// 共用。公开域全量也只有几十行、实测一次回源约 24ms，5 秒纯粹是防止回源无界挂住。
	// 注意不要把它当成「延迟预算」去调小：同步路径会被它硬截断，截断后 handler 只能返回 500，
	// 而「没有缓存」的基线是「慢但成功」——收紧前必须先确认它明显大于数据库的 p99.9 尾延迟。
	publicCatalogCacheLoadTimeout = 5 * time.Second
	// publicCatalogCacheStoreFailureBackoff 是回填持续失败后的退避窗口：写路径故障时
	// 旧条目的 expireAt 会永远停在过去，不退避就会每个请求都触发一次重建。
	publicCatalogCacheStoreFailureBackoff = 30 * time.Second
)

// errPublicCatalogCacheMiss 是缓存内部哨兵：命中了「查不到」的空值标记。
// 只有详情类查询会产生这个标记，列表类查询的载荷永远不会为空。
var errPublicCatalogCacheMiss = errors.New("公开目录缓存命中空值标记")

// PublicCatalogCacheOptions 是公开目录缓存的构造参数。
type PublicCatalogCacheOptions struct {
	// Enabled 是总开关：关闭时装饰器完全退化为纯透传。
	Enabled bool
	// TTL 是基础逻辑过期时长（物理 TTL 为其 2 倍）。
	TTL time.Duration
	// JitterRatio 是 TTL 抖动比例，0.2 表示 ±20%。
	JitterRatio float64
}

// CachedPublicCatalogRepository 是 port.PublicCatalogRepository 的逻辑过期缓存装饰器。
// 它只覆盖公开域的 5 个只读方法，管理端目录查询不经过本类型。
type CachedPublicCatalogRepository struct {
	source      port.PublicCatalogRepository
	client      redis.UniversalClient
	enabled     bool
	ttl         time.Duration
	jitterRatio float64
	// group 收敛同一 key 的并发回源：同步未命中与异步重建共用同一组 flight。
	group singleflight.Group
	// now 与 random 可注入，便于测试断言逻辑过期行为与 TTL 抖动区间。
	now    func() time.Time
	random func() float64
	// lastStoreFailure 记录最近一次回填失败的时刻（unix 毫秒，原子读写）。
	// 只读副本、maxmemory、ACL 变更等会让 SET 持续失败，此时必须退避异步重建，
	// 否则每个请求都会额外起一个 goroutine 并多打一次数据库。
	lastStoreFailure atomic.Int64
}

// NewCachedPublicCatalogRepository 构造公开目录缓存装饰器。
// client 为 nil 时装饰器自动退化为纯透传；调用方请勿传入「持有 nil 指针的接口值」。
func NewCachedPublicCatalogRepository(
	source port.PublicCatalogRepository,
	client redis.UniversalClient,
	options PublicCatalogCacheOptions,
) (*CachedPublicCatalogRepository, error) {
	if source == nil {
		return nil, errors.New("公开目录缓存需要底层仓储")
	}
	// 关闭缓存时不校验 TTL：把开关关掉不该因为遗留的 TTL 配置而启动失败。
	if options.Enabled {
		if options.TTL <= 0 {
			return nil, fmt.Errorf("公开目录缓存 TTL 必须为正数：%s", options.TTL)
		}
		if options.JitterRatio < 0 || options.JitterRatio >= 1 {
			return nil, fmt.Errorf("公开目录缓存抖动比例必须落在 [0,1)：%v", options.JitterRatio)
		}
	}
	return &CachedPublicCatalogRepository{
		source:      source,
		client:      client,
		enabled:     options.Enabled,
		ttl:         options.TTL,
		jitterRatio: options.JitterRatio,
		now:         time.Now,
		random:      rand.Float64,
	}, nil
}

// ListPublicDepartments 分页返回公开科室列表（带缓存）。
func (r *CachedPublicCatalogRepository) ListPublicDepartments(ctx context.Context, f catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error) {
	if r == nil || r.source == nil {
		return nil, 0, sql.ErrConnDone
	}
	page, ok := publicCatalogPageNumber(offset, limit)
	if !r.cacheReady() || !ok {
		return r.source.ListPublicDepartments(ctx, f, offset, limit)
	}
	return cachedPublicCatalogPage(r, ctx, publicCatalogDepartmentsCacheKey(f, page, limit),
		func(ctx context.Context) ([]catalog.Department, int64, error) {
			return r.source.ListPublicDepartments(ctx, f, offset, limit)
		})
}

// FindPublicDepartment 返回单个公开科室（带缓存，含「不存在」空值标记）。
func (r *CachedPublicCatalogRepository) FindPublicDepartment(ctx context.Context, id int64) (*catalog.Department, error) {
	if r == nil || r.source == nil {
		return nil, sql.ErrConnDone
	}
	if !r.cacheReady() {
		return r.source.FindPublicDepartment(ctx, id)
	}
	return cachedPublicCatalogDetail(r, ctx, publicCatalogDepartmentCacheKey(id),
		func(ctx context.Context) (*catalog.Department, error) {
			return r.source.FindPublicDepartment(ctx, id)
		})
}

// ListPublicSubdepartments 分页返回某科室下的公开子科室（带缓存）。
// 科室不存在时源仓储返回 sql.ErrNoRows，这里不缓存：把「科室不存在」折叠成空列表会让
// use case 从 404 变成 200，属于悄悄改契约。
func (r *CachedPublicCatalogRepository) ListPublicSubdepartments(ctx context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error) {
	if r == nil || r.source == nil {
		return nil, 0, sql.ErrConnDone
	}
	page, ok := publicCatalogPageNumber(offset, limit)
	if !r.cacheReady() || !ok {
		return r.source.ListPublicSubdepartments(ctx, departmentID, offset, limit)
	}
	return cachedPublicCatalogPage(r, ctx, publicCatalogSubdepartmentsCacheKey(departmentID, page, limit),
		func(ctx context.Context) ([]catalog.Subdepartment, int64, error) {
			return r.source.ListPublicSubdepartments(ctx, departmentID, offset, limit)
		})
}

// ListPublicDoctors 分页返回公开医生列表（带缓存）。
func (r *CachedPublicCatalogRepository) ListPublicDoctors(ctx context.Context, f catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error) {
	if r == nil || r.source == nil {
		return nil, 0, sql.ErrConnDone
	}
	page, ok := publicCatalogPageNumber(offset, limit)
	if !r.cacheReady() || !ok {
		return r.source.ListPublicDoctors(ctx, f, offset, limit)
	}
	return cachedPublicCatalogPage(r, ctx, publicCatalogDoctorsCacheKey(f, page, limit),
		func(ctx context.Context) ([]catalog.PublicDoctor, int64, error) {
			return r.source.ListPublicDoctors(ctx, f, offset, limit)
		})
}

// FindPublicDoctor 返回公开医生详情（带缓存，含「不存在」空值标记）。
func (r *CachedPublicCatalogRepository) FindPublicDoctor(ctx context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	if r == nil || r.source == nil {
		return nil, sql.ErrConnDone
	}
	if !r.cacheReady() {
		return r.source.FindPublicDoctor(ctx, id)
	}
	return cachedPublicCatalogDetail(r, ctx, publicCatalogDoctorCacheKey(id),
		func(ctx context.Context) (*catalog.PublicDoctorDetail, error) {
			return r.source.FindPublicDoctor(ctx, id)
		})
}

// cacheReady 判断本次调用是否真的走缓存：总开关关闭或没注入 Redis 客户端时退化为纯透传。
func (r *CachedPublicCatalogRepository) cacheReady() bool {
	return r.enabled && r.client != nil
}

// publicCatalogCacheEntry 是逻辑过期缓存信封。
// Payload 省略（nil）表示「查不到」的空值标记；除该标记外载荷一律是回源结果的原始 JSON。
type publicCatalogCacheEntry struct {
	// ExpireAt 是逻辑过期时刻（unix 毫秒）。
	ExpireAt int64           `json:"expireAt"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// publicCatalogPagePayload 是列表类查询的缓存载荷。
// 单独包一层 items+total：total 也是回源结果的一部分，只缓存 items 会让响应里的
// total 走另一条路径（甚至被迫再查一次库）。
type publicCatalogPagePayload[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
}

// cachedPublicCatalogPage 是列表类查询的逻辑过期读穿。
func cachedPublicCatalogPage[T any](
	c *CachedPublicCatalogRepository,
	ctx context.Context,
	key string,
	load func(context.Context) ([]T, int64, error),
) ([]T, int64, error) {
	payload, err := publicCatalogLoadThrough(c, ctx, key, func(ctx context.Context) (publicCatalogPagePayload[T], error) {
		items, total, err := load(ctx)
		if err != nil {
			return publicCatalogPagePayload[T]{}, err
		}
		// 回填前就规范成非 nil 空切片，保证「缓存命中」与「直查」两条路径的
		// 载荷形状一致（items:[] 而不是 items:null）。
		return publicCatalogPagePayload[T]{Items: normalizePublicCatalogItems(items), Total: total}, nil
	}, false)
	if err != nil {
		return nil, 0, err
	}
	// 再兜底一次：历史版本可能写入了 items:null 的载荷。
	return normalizePublicCatalogItems(payload.Items), payload.Total, nil
}

// cachedPublicCatalogDetail 是详情类查询的逻辑过期读穿。
// 源仓储以 sql.ErrNoRows 表示「不存在」，这里把它折叠成空值标记（短 TTL）缓存，
// 命中标记时再还原 sql.ErrNoRows，保证调用方看到的语义与直查完全一致。
func cachedPublicCatalogDetail[T any](
	c *CachedPublicCatalogRepository,
	ctx context.Context,
	key string,
	load func(context.Context) (*T, error),
) (*T, error) {
	value, err := publicCatalogLoadThrough(c, ctx, key, func(ctx context.Context) (*T, error) {
		item, err := load(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			// 「查不到」不是故障：编码成空值标记，避免不存在或恶意的 ID 反复打库。
			return nil, nil
		}
		return item, err
	}, true)
	if errors.Is(err, errPublicCatalogCacheMiss) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, sql.ErrNoRows
	}
	return value, nil
}

// publicCatalogLoadThrough 是逻辑过期读穿的核心。
// allowMiss 表示该查询是否允许出现「查不到」空值标记：只有详情类查询会写标记，
// 列表查询遇到空载荷时按脏数据回源覆盖，绝不能把内部哨兵抛给 use case 变成 500。
// 允许空值标记时，命中标记会返回 errPublicCatalogCacheMiss。
func publicCatalogLoadThrough[P any](
	c *CachedPublicCatalogRepository,
	ctx context.Context,
	key string,
	load func(context.Context) (P, error),
	allowMiss bool,
) (P, error) {
	var zero P
	entry, hit, err := c.readEntry(ctx, key)
	if err != nil {
		// Redis 不可用：降级为直查数据库（fail open），绝不让缓存故障变成业务故障。
		// 仍然经过 singleflight 合并并发回源，避免依赖故障期间把数据库一起打崩；
		// 本次不回填（store=false），不向已经出问题的 Redis 追加写请求。
		return publicCatalogLoadCoalesced(c, ctx, key, load, false)
	}
	if hit {
		expired := c.logicallyExpired(entry)
		emptyPayload := isEmptyPublicCatalogPayload(entry.Payload)
		if emptyPayload && allowMiss {
			// 空值标记走同一套逻辑过期语义：过期后照旧返回「查不到」，同时异步回源。
			// 若在这里直接短路，标记就要一直挡到物理 TTL 才消失，负缓存的实际存活时间
			// 会从 60 秒翻倍到 120 秒，「刚新增的科室/医生」在这段时间里始终是 404。
			if expired {
				publicCatalogRebuildAsync(c, ctx, key, load)
			}
			return zero, errPublicCatalogCacheMiss
		}
		if !emptyPayload {
			var payload P
			if unmarshalErr := json.Unmarshal(entry.Payload, &payload); unmarshalErr == nil {
				if expired {
					// 已逻辑过期：先返回旧值，后台异步重建，请求不再等数据库。
					publicCatalogRebuildAsync(c, ctx, key, load)
				}
				return payload, nil
			}
			// 载荷损坏（键结构升级或人工误写）：按未命中处理，回源后覆盖，不把脏数据打成 500。
		}
		// 走到这里有两种脏数据：列表键下出现空值标记、载荷无法解析。两者都按未命中处理。
	}
	return publicCatalogLoadCoalesced(c, ctx, key, load, true)
}

// publicCatalogLoadCoalesced 同步回源：singleflight 让同一 key 的并发请求只打一次数据库，
// 并把结果回填缓存（store=false 用于 Redis 故障降级路径）。
func publicCatalogLoadCoalesced[P any](
	c *CachedPublicCatalogRepository,
	ctx context.Context,
	key string,
	load func(context.Context) (P, error),
	store bool,
) (P, error) {
	var zero P
	value, err, _ := c.group.Do(key, func() (any, error) {
		// 回源脱离发起方的请求上下文：singleflight 会把 leader 的错误广播给所有 follower，
		// 若沿用 leader 的请求 ctx，某一个客户端断开就会让同一 flight 上其它客户端的请求
		// 一起拿到 context.Canceled（handler 侧即 500）。带超时的分离上下文既保住了有界性，
		// 又不会把单个客户端的取消放大成一批失败。
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publicCatalogCacheLoadTimeout)
		defer cancel()
		loaded, loadErr := load(loadCtx)
		if loadErr != nil {
			return nil, loadErr
		}
		if store {
			// 回填失败不改变本次响应：请求已经拿到数据，下一次请求重新回源即可。
			_ = c.store(loadCtx, key, loaded)
		}
		return loaded, nil
	})
	if err != nil {
		return zero, err
	}
	payload, ok := value.(P)
	if !ok {
		return zero, errors.New("公开目录缓存载荷类型不匹配")
	}
	return payload, nil
}

// publicCatalogRebuildAsync 在逻辑过期后异步重建缓存。
// 请求线程立刻拿到旧值，回源发生在脱离请求上下文的 goroutine 里——请求上下文在响应写完后
// 立即取消，拿它去查库会必然失败；singleflight 保证同一 key 的并发重建只回源一次。
func publicCatalogRebuildAsync[P any](
	c *CachedPublicCatalogRepository,
	ctx context.Context,
	key string,
	load func(context.Context) (P, error),
) {
	// 回填持续失败时退避：重建结果同样写不进去，只会白打一次库并多起一个 goroutine。
	if c.storeFailureBackoffActive() {
		return
	}
	go func() {
		// 后台回源里的 panic 会直接终止整个进程：HTTP 层的 gin.Recovery 覆盖不到分离出去的
		// goroutine，所以这里必须自己兜住，只作废这一次重建，不能让缓存给进程新增崩溃面。
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("公开目录缓存异步重建 panic，已忽略：key=%s err=%v", key, recovered)
			}
		}()
		rebuildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publicCatalogCacheLoadTimeout)
		defer cancel()
		// 重建失败不重试：旧值仍在，下一次请求会再次触发重建。
		_, _, _ = c.group.Do(key, func() (any, error) {
			payload, err := load(rebuildCtx)
			if err != nil {
				return nil, err
			}
			_ = c.store(rebuildCtx, key, payload)
			return payload, nil
		})
	}()
}

// readEntry 读取缓存信封。hit=false 表示键不存在；载荷无法解析同样按未命中处理。
// 只有返回 error 才代表 Redis 本身不可用，调用方据此降级直查。
func (c *CachedPublicCatalogRepository) readEntry(ctx context.Context, key string) (*publicCatalogCacheEntry, bool, error) {
	raw, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var entry publicCatalogCacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, false, nil
	}
	return &entry, true, nil
}

// store 回填缓存。
// 载荷为 nil（详情「查不到」）时写空值标记并改用短 TTL：空值只是「此刻不存在」的临时
// 结论，不能像正常数据那样固化半小时。
func (c *CachedPublicCatalogRepository) store(ctx context.Context, key string, payload any) error {
	body, err := encodePublicCatalogPayload(payload)
	if err != nil {
		return err
	}
	logicalTTL := c.nextLogicalTTL()
	if body == nil {
		logicalTTL = publicCatalogCacheMissTTL
	}
	entry := publicCatalogCacheEntry{
		ExpireAt: c.now().Add(logicalTTL).UnixMilli(),
		Payload:  body,
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := c.client.Set(ctx, key, encoded, logicalTTL*publicCatalogCachePhysicalFactor).Err(); err != nil {
		// 记住写失败：写路径持续故障时旧条目的 expireAt 永远停在过去，不退避就会
		// 每个请求都触发一次异步重建（既多打库，又多起 goroutine）。
		// 只在「刚进入退避」的那一次打日志：写路径长时间故障时既要留下信号，
		// 又不能每个请求刷一行日志。成功回填会把标记清零，恢复后再故障会重新告警。
		if c.lastStoreFailure.Swap(c.now().UnixMilli()) == 0 {
			log.Printf("公开目录缓存回填失败，%s 内暂停异步重建：%v", publicCatalogCacheStoreFailureBackoff, err)
		}
		return err
	}
	c.lastStoreFailure.Store(0)
	return nil
}

// storeFailureBackoffActive 判断是否处在「回填失败退避」窗口内。
func (c *CachedPublicCatalogRepository) storeFailureBackoffActive() bool {
	last := c.lastStoreFailure.Load()
	if last == 0 {
		return false
	}
	return c.now().UnixMilli()-last < publicCatalogCacheStoreFailureBackoff.Milliseconds()
}

// isEmptyPublicCatalogPayload 判断载荷是否为「查不到」空值标记。
// 字段省略（nil）与显式 null 两种等价写法都认，避免它们走出两条不同分支。
func isEmptyPublicCatalogPayload(payload json.RawMessage) bool {
	return len(payload) == 0 || string(payload) == "null"
}

// logicallyExpired 判断信封是否已到逻辑过期时刻（到达即视为过期）。
func (c *CachedPublicCatalogRepository) logicallyExpired(entry *publicCatalogCacheEntry) bool {
	return !c.now().Before(time.UnixMilli(entry.ExpireAt))
}

// nextLogicalTTL 在基础 TTL 上叠加 ±jitterRatio 抖动。
// 抖动方向与幅度都由注入的 random 决定，测试可据此断言落点区间。
func (c *CachedPublicCatalogRepository) nextLogicalTTL() time.Duration {
	if c.jitterRatio <= 0 {
		return c.ttl
	}
	// random 返回 [0,1)，线性映射到 [-1,1) 后再乘抖动比例。
	factor := 1 + c.jitterRatio*(2*c.random()-1)
	ttl := time.Duration(float64(c.ttl) * factor)
	if ttl <= 0 {
		return c.ttl
	}
	return ttl
}

// encodePublicCatalogPayload 把载荷编码成 JSON。
// nil 指针（详情查不到）统一编码成「省略 payload」的空值标记，避免出现 payload=null 与
// payload 缺失两种等价写法。
func encodePublicCatalogPayload(payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if string(body) == "null" {
		return nil, nil
	}
	return json.RawMessage(body), nil
}

// normalizePublicCatalogItems 把 nil 切片规范成空切片。
// use case 层会用同样的方式兜底（见 usecase/publiccatalog），缓存命中与直查必须给出相同的
// 形状（items:[] 而不是 items:null），否则「加缓存」就等于悄悄改了响应契约。
func normalizePublicCatalogItems[T any](items []T) []T {
	if items == nil {
		return make([]T, 0)
	}
	return items
}

// publicCatalogPageNumber 把 use case 传入的 offset/limit 还原成页码。
// 公开域 use case 固定以 offset=(page-1)*pageSize 调用（见 publiccatalog.offset）；
// 出现非规范的 offset/limit 组合时返回 false，调用方直接绕过缓存——否则不同的 offset
// 会被折叠到同一个 page 键上，返回上一页的数据。
func publicCatalogPageNumber(offset, limit int) (int, bool) {
	if limit <= 0 || offset < 0 || offset%limit != 0 {
		return 0, false
	}
	return offset/limit + 1, true
}

// publicCatalogDepartmentsCacheKey 科室列表键。
// 设计文档给的形式是 medical:cache:public:departments:v1（不含分页）；这里补上
// p{page}:s{pageSize}，理由同 publicCatalogPageNumber：分页维度少一个就会命中错页。
func publicCatalogDepartmentsCacheKey(f catalog.DepartmentFilter, page, pageSize int) string {
	hash := publicCatalogFilterHash(
		"outpatient="+publicCatalogBoolToken(f.Outpatient),
		"recommended="+publicCatalogBoolToken(f.Recommended),
		"sort="+f.Sort,
		"order="+f.Order,
	)
	return publicCatalogListCacheKey("departments", hash, page, pageSize)
}

// publicCatalogSubdepartmentsCacheKey 子科室列表键：契约里该接口只有分页参数，
// 因此摘要维度只有 departmentId 本身。
func publicCatalogSubdepartmentsCacheKey(departmentID int64, page, pageSize int) string {
	return publicCatalogListCacheKey("subdepts", strconv.FormatInt(departmentID, 10), page, pageSize)
}

// publicCatalogDoctorsCacheKey 公开医生列表键：过滤与排序条件取 sha256 前 16 位。
func publicCatalogDoctorsCacheKey(f catalog.PublicDoctorFilter, page, pageSize int) string {
	hash := publicCatalogFilterHash(
		"deptId="+publicCatalogInt64Token(f.DepartmentID),
		"subId="+publicCatalogInt64Token(f.SubdepartmentID),
		"name="+publicCatalogStringToken(f.Name),
		"sort="+f.Sort,
		"order="+f.Order,
	)
	return publicCatalogListCacheKey("doctors", hash, page, pageSize)
}

// publicCatalogDepartmentCacheKey 科室详情键。
func publicCatalogDepartmentCacheKey(id int64) string {
	return publicCatalogDetailCacheKey("department", id)
}

// publicCatalogDoctorCacheKey 医生详情键。
func publicCatalogDoctorCacheKey(id int64) string {
	return publicCatalogDetailCacheKey("doctor", id)
}

// publicCatalogListCacheKey 拼装列表类键：资源 + 维度摘要 + 分页 + 版本。
func publicCatalogListCacheKey(resource, dimension string, page, pageSize int) string {
	return fmt.Sprintf("%s%s:%s:p%d:s%d:%s",
		publicCatalogCacheKeyPrefix, resource, dimension, page, pageSize, publicCatalogCacheKeyVersion)
}

// publicCatalogDetailCacheKey 拼装详情类键：详情无过滤维度，只按资源 ID 区分。
func publicCatalogDetailCacheKey(resource string, id int64) string {
	return fmt.Sprintf("%s%s:%d:%s", publicCatalogCacheKeyPrefix, resource, id, publicCatalogCacheKeyVersion)
}

// publicCatalogFilterHash 把「排序字段 + 过滤条件」规范化后取 sha256 前 16 位，
// 避免把中文姓名等长值直接写进键名。
// 每个维度都带长度前缀（自定界编码）：即使某一维的值里出现 "|" 或 ":"，
// 也无法与相邻维度重组出同一个串，编码对维度列表是单射的。
func publicCatalogFilterHash(parts ...string) string {
	var builder strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&builder, "%d:%s|", len(part), part)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// 下面三个 token 函数把可选过滤条件编码成「未传」与「传值」彼此可区分的稳定字符串。
// 关键约束：nil（未传该参数）必须与「用户能构造出来的任何取值」编码不同。固定哨兵串做不到
// 这一点——例如用 "-" 表示未传时，?name=- 会得到同一个 token，于是「不传 name」与
// 「name=-」这两种结果集不同的请求落到同一个缓存键上，既会返回错数据，也能被匿名调用者
// 互相投毒。因此统一用 "n" 表示未传，用「类型前缀 + 长度前缀 + 值」表示已传。
const publicCatalogFilterAbsentToken = "n"

func publicCatalogInt64Token(value *int64) string {
	if value == nil {
		return publicCatalogFilterAbsentToken
	}
	return "i" + strconv.FormatInt(*value, 10)
}

func publicCatalogStringToken(value *string) string {
	if value == nil {
		return publicCatalogFilterAbsentToken
	}
	// 长度前缀让编码无歧义：已传值的 token 一律以 "s<长度>:" 开头，不可能等于 "n"。
	return "s" + strconv.Itoa(len(*value)) + ":" + *value
}

func publicCatalogBoolToken(value *bool) string {
	if value == nil {
		return publicCatalogFilterAbsentToken
	}
	return "b" + strconv.FormatBool(*value)
}

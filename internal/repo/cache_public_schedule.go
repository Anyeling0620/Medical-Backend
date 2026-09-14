package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"

	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/port"
)

// 本文件是匿名公开域可挂号时段查询（GET /api/v1/public/schedules）的读缓存装饰器（T4b）。
// 装饰器只包裹 port.PublicScheduleRepository 这一个读方法，PostgresScheduleRepository
// 与 use case 的查询逻辑都不改动，装配改在 internal/bootstrap/app.go。
//
// 缓存语义（与 T4a 的目录缓存刻意不同，不要照搬）：
//   - **不用逻辑过期**：时段带有 remaining（余量），过期后继续返回旧值等于让患者看到
//     「可挂」却在下单时拿到 409，因此这里只用 5~10 秒短 TTL + 抖动，绝不复用逻辑过期；
//   - **缓存空结果**：实测「今天起 7 天」窗口经常是 0 条，缓存空结果能显著减少无效回源；
//     空结果同样只活一个短 TTL，并且写路径会递增版本号，因此新增排班不会被空结果挡住；
//   - **版本号失效**：键里带版本号（见 port.ScheduleCacheVersioner），写操作只需 INCR，
//     旧键自然失效，不做 SCAN 全库；
//   - **fail open**：Redis 的任何错误都降级为直查数据库并跳过回填，绝不返回 5xx。
//
// 键结构：
//
//	值键：medical:cache:public:schedules:{scope}:{fromDate}:{toDate}:o{offset}:l{limit}:v{subVer}.{docVer}.{allVer}
//	版本键：medical:cache:ver:schedules:sub:{id} / medical:cache:ver:schedules:doctor:{id}
//	        medical:cache:ver:schedules:all
//
// scope 由实际过滤条件拼成（sub1-doc2 / sub1 / doc2），保证「只按医生查」的键里也带医生版本号。
const (
	scheduleCacheKeyPrefix        = "medical:cache:public:schedules"
	scheduleCacheVersionKeyPrefix = "medical:cache:ver:schedules"
	// scheduleCacheQueryTimeout 是缓存回源的兜底超时：回源不沿用调用方 ctx（见 ListPublicSchedules）。
	scheduleCacheQueryTimeout = 5 * time.Second
)

// 编译期断言：装饰器满足公开域读接口，缓存同时满足写路径的失效端口。
var (
	_ port.PublicScheduleRepository = (*CachedPublicScheduleRepository)(nil)
	_ port.ScheduleCacheVersioner   = (*ScheduleCache)(nil)
)

// cachedSchedulePage 是缓存值结构：条目 + 总数。
// Items 始终是非 nil 切片（空结果是 []，不是 null），与 use case 层的空切片口径一致。
type cachedSchedulePage struct {
	Items []schedule.PublicSchedule `json:"items"`
	Total int64                     `json:"total"`
}

// CachedPublicScheduleRepository 用 Redis 短 TTL 缓存包装公开时段查询。
type CachedPublicScheduleRepository struct {
	repo  port.PublicScheduleRepository
	cache *ScheduleCache
	// group 做进程内 singleflight：同一 key 并发只回源一次，避免热点键过期瞬间打穿数据库。
	// 多实例部署时该防护只在单进程内有效，短 TTL 本身把多实例的放大倍数限制在秒级。
	group singleflight.Group
}

// NewCachedPublicScheduleRepository 构造装饰器；cache 为 nil（缓存未启用或 Redis 不可用）时
// 装饰器退化为直查，保证调用方无需分支。
func NewCachedPublicScheduleRepository(repository port.PublicScheduleRepository, cache *ScheduleCache) *CachedPublicScheduleRepository {
	return &CachedPublicScheduleRepository{repo: repository, cache: cache}
}

// ListPublicSchedules 先读缓存，未命中再回源并回填；Redis 出错时直查数据库（fail open）。
func (r *CachedPublicScheduleRepository) ListPublicSchedules(ctx context.Context, f schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error) {
	if r == nil || r.repo == nil {
		return nil, 0, errors.New("排班仓库未配置")
	}
	// 两个过滤维度都无效（缺失或非正整数）的查询不会出现在契约里（绑定层与 use case 都会拒绝），
	// 且这种键没有维度版本可言、只能被全局失效兜住：直接直查，把这个误用面变成不可能，
	// 而不是留下一个「只靠全局失效兜底」的缓存键。
	if (f.SubdepartmentID == nil || *f.SubdepartmentID <= 0) && (f.DoctorID == nil || *f.DoctorID <= 0) {
		return r.listDirect(ctx, f, offset, limit)
	}
	// 缓存未启用、未注入 Redis 或版本号读不出来：直查数据库，行为与改动前完全一致。
	if !r.cache.enabled() {
		return r.listDirect(ctx, f, offset, limit)
	}
	key, ok := r.cacheKey(ctx, f, offset, limit)
	if !ok {
		return r.listDirect(ctx, f, offset, limit)
	}
	if page, hit := r.load(ctx, key); hit {
		return page.Items, page.Total, nil
	}
	value, err, _ := r.group.Do(key, func() (any, error) {
		// 回源不沿用调用方 ctx：singleflight 的首个调用方一旦断开，所有等待者会一起失败；
		// 这里剥离取消信号并加超时兜底（与建单补偿同一取舍，见 usecase/registration/service.go）。
		queryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduleCacheQueryTimeout)
		defer cancel()
		items, total, err := r.repo.ListPublicSchedules(queryCtx, f, offset, limit)
		if err != nil {
			// 回源失败不写缓存，交给上层按原有错误语义处理（可能是 5xx，但不缓存脏数据）。
			return nil, err
		}
		if items == nil {
			// 与 use case 层保持同一空切片语义：否则缓存路径输出 items:null、直查路径输出 items:[]，
			// 等于悄悄改了契约形状。
			items = make([]schedule.PublicSchedule, 0)
		}
		page := cachedSchedulePage{Items: items, Total: total}
		r.store(queryCtx, key, page)
		return page, nil
	})
	if err != nil {
		return nil, 0, err
	}
	page, ok := value.(cachedSchedulePage)
	if !ok {
		// 理论上不可达（group 只返回上面构造的类型）；按未命中处理并直查，避免 500。
		log.Printf("提示：公开排班缓存回源返回值类型异常，降级直查数据库 key=%s", key)
		return r.listDirect(ctx, f, offset, limit)
	}
	// singleflight 会把同一份回源结果共享给同 key 的所有等待者：这里浅拷贝一份再返回，
	// 避免将来 usecase 里的一次排序/追加/过滤把并发请求的数据串改（PublicSchedule 内无指针字段，
	// 浅拷贝即可完整隔离）。
	items := make([]schedule.PublicSchedule, len(page.Items))
	copy(items, page.Items)
	return items, page.Total, nil
}

// listDirect 直查数据库并做与缓存路径完全一致的归一化：空结果必须是空切片而不是 nil，
// 否则「缓存路径 items:[]、降级路径 items:null」会悄悄改掉契约形状（fail open 也要等价）。
//
// 出错时有意丢弃仓库可能返回的部分结果（仓储实现会在 rows 迭代中途出错时同时返回 items/total），
// 返回 (nil, 0, err)：调用方只按 err 分支处理，但缓存路径从不返回部分结果，
// 两条路径保持同一形状，也不要把它「修」回原样透传。
func (r *CachedPublicScheduleRepository) listDirect(ctx context.Context, f schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error) {
	items, total, err := r.repo.ListPublicSchedules(ctx, f, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	if items == nil {
		items = make([]schedule.PublicSchedule, 0)
	}
	return items, total, nil
}

// cacheKey 组装缓存键；返回 false 表示版本号读取失败，调用方必须降级直查。
func (r *CachedPublicScheduleRepository) cacheKey(ctx context.Context, f schedule.PublicScheduleFilter, offset, limit int) (string, bool) {
	keys := make([]string, 0, 3)
	scopeParts := make([]string, 0, 2)
	if f.SubdepartmentID != nil && *f.SubdepartmentID > 0 {
		keys = append(keys, r.cache.subVersionKey(*f.SubdepartmentID))
		scopeParts = append(scopeParts, "sub"+strconv.FormatInt(*f.SubdepartmentID, 10))
	}
	if f.DoctorID != nil && *f.DoctorID > 0 {
		keys = append(keys, r.cache.doctorVersionKey(*f.DoctorID))
		scopeParts = append(scopeParts, "doc"+strconv.FormatInt(*f.DoctorID, 10))
	}
	// 全局版本键必须出现在每个键里：「定位不到作用域」的写路径（如删除时段）只能递增它，
	// 用它兜住所有查询，宁可多失效也不漏失效。
	keys = append(keys, r.cache.globalVersionKey())

	versions, err := r.cache.readVersions(ctx, keys)
	if err != nil {
		log.Printf("提示：公开排班缓存版本号读取失败，降级直查数据库: %v", err)
		return "", false
	}
	if len(versions) != len(keys) {
		// MGET 返回值数量与请求数量不一致属于异常响应：降级直查，避免下面按位置取值越界。
		log.Printf("提示：公开排班缓存版本号数量异常，降级直查数据库 want=%d got=%d", len(keys), len(versions))
		return "", false
	}
	var subdepartmentVersion, doctorVersion int64
	pos := 0
	if f.SubdepartmentID != nil && *f.SubdepartmentID > 0 {
		subdepartmentVersion = versions[pos]
		pos++
	}
	if f.DoctorID != nil && *f.DoctorID > 0 {
		doctorVersion = versions[pos]
		pos++
	}
	globalVersion := versions[pos]

	scope := "all"
	if len(scopeParts) > 0 {
		scope = strings.Join(scopeParts, "-")
	}
	// 键里必须同时带 offset 与 limit：契约允许 pageSize 1~100，只带页码会让
	// 「第 1 页 20 条」与「第 1 页 50 条」落到同一个键上互相返回对方的数据；
	// 用 offset 而不是由页码反推，避免上游传非对齐 offset 时两个不同切片共用缓存。
	return fmt.Sprintf("%s:%s:%s:%s:o%d:l%d:v%d.%d.%d",
		scheduleCacheKeyPrefix, scope, f.FromDate, f.ToDate, offset, limit,
		subdepartmentVersion, doctorVersion, globalVersion), true
}

// load 读取缓存；返回 false 表示未命中或 Redis 不可用，两种情况都回源。
func (r *CachedPublicScheduleRepository) load(ctx context.Context, key string) (cachedSchedulePage, bool) {
	opCtx, cancel := cacheContext(ctx, r.cache.opts.RedisTimeout)
	defer cancel()
	payload, err := r.cache.redis.Get(opCtx, key).Bytes()
	if err != nil {
		// redis.Nil 是正常的未命中，不打日志；其它错误说明缓存不可用，只提示并降级。
		if !errors.Is(err, redis.Nil) {
			log.Printf("提示：公开排班缓存读取失败，降级直查数据库 key=%s: %v", key, err)
		}
		return cachedSchedulePage{}, false
	}
	var page cachedSchedulePage
	if err := json.Unmarshal(payload, &page); err != nil {
		// 反序列化失败通常是键结构变更：按未命中处理并直查数据库，不返回 5xx。
		log.Printf("提示：公开排班缓存反序列化失败，降级直查数据库 key=%s: %v", key, err)
		return cachedSchedulePage{}, false
	}
	if page.Items == nil {
		page.Items = make([]schedule.PublicSchedule, 0)
	}
	return page, true
}

// store 回填缓存。失败只记录日志：缓存是性能依赖，回填失败不能让读请求失败（fail open）。
func (r *CachedPublicScheduleRepository) store(ctx context.Context, key string, page cachedSchedulePage) {
	payload, err := json.Marshal(page)
	if err != nil {
		log.Printf("告警：公开排班缓存序列化失败 key=%s: %v", key, err)
		return
	}
	ttl := jitteredTTL(r.cache.opts.TTL, r.cache.opts.JitterRatio, r.cache.randFloat)
	if ttl <= 0 {
		// Redis 的 0 表示「永不过期」，余量缓存绝不允许永不过期：
		// 配置非法时宁可每次回源，也不留下永久脏数据。
		log.Printf("告警：公开排班缓存 TTL 非法（%s），跳过回填 key=%s", r.cache.opts.TTL, key)
		return
	}
	opCtx, cancel := cacheContext(ctx, r.cache.opts.RedisTimeout)
	defer cancel()
	if err := r.cache.redis.Set(opCtx, key, payload, ttl).Err(); err != nil {
		log.Printf("提示：公开排班缓存回填失败 key=%s: %v", key, err)
	}
}

// ScheduleCache 同时承担两个角色：公开时段读缓存的存储，以及写路径的版本号递增器。
// 两者共用同一套键前缀与配置，拆成两个类型只会让装配更绕。
type ScheduleCache struct {
	redis cacheRedis
	opts  CacheOptions
	// randFloat 可注入，便于测试断言抖动落在设定区间内。
	randFloat func() float64
}

// NewScheduleCache 构造排班缓存；Redis 客户端为空时返回 nil，调用方据此关闭缓存。
func NewScheduleCache(client redis.UniversalClient, opts CacheOptions) *ScheduleCache {
	if client == nil {
		return nil
	}
	return &ScheduleCache{redis: client, opts: opts, randFloat: rand.Float64}
}

// enabled 判断缓存是否可用；nil 接收者同样返回 false，调用方无需判空。
func (c *ScheduleCache) enabled() bool {
	return c != nil && c.redis != nil
}

// BumpSchedules 递增版本号，使旧缓存键立即失效（port.ScheduleCacheVersioner 的实现）。
//
// 只有「子科室 + 医生」两个维度都已知时才做细粒度失效；任一维度缺失就退化为全局失效。
// 原因是查询键的组合：只按子科室查的键里没有医生版本，只按医生查的键里没有子科室版本，
// 只递增一个维度必然留下一部分键陈旧到 TTL 到期。正确性优先于命中率：
// 漏失效会让前端显示可挂但下单拿 409，多失效只是多一次回源。
func (c *ScheduleCache) BumpSchedules(ctx context.Context, subdepartmentID, doctorID int64) error {
	if !c.enabled() {
		return nil // 缓存未启用：没有键需要失效，也不算失败。
	}
	var keys []string
	if subdepartmentID > 0 && doctorID > 0 {
		keys = []string{c.subVersionKey(subdepartmentID), c.doctorVersionKey(doctorID)}
	} else {
		keys = []string{c.globalVersionKey()}
	}
	// 写操作已经提交：即使客户端断开（ctx 已取消）也必须完成失效，
	// 否则缓存会一直脏到 TTL 到期；这也是这里剥离取消信号的原因。
	// 每个版本键各用一份超时预算：共用一份预算时，慢 Redis 下首个 INCR 耗尽预算会让
	// 后面的维度没生效，形成「只失效一半」的脏读窗口。
	bumpCtx := context.WithoutCancel(ctx)
	var firstErr error
	for _, key := range keys {
		opCtx, cancel := cacheContext(bumpCtx, c.opts.RedisTimeout)
		if err := c.redis.Incr(opCtx, key).Err(); err != nil && firstErr == nil {
			firstErr = err
		}
		cancel()
	}
	return firstErr
}

// readVersions 用一次 MGET 读取版本号；键不存在（nil）按 0 处理，即「从未失效过」。
func (c *ScheduleCache) readVersions(ctx context.Context, keys []string) ([]int64, error) {
	opCtx, cancel := cacheContext(ctx, c.opts.RedisTimeout)
	defer cancel()
	values, err := c.redis.MGet(opCtx, keys...).Result()
	if err != nil {
		return nil, err
	}
	versions := make([]int64, len(values))
	for i, value := range values {
		versions[i] = parseCacheVersion(value)
	}
	return versions, nil
}

// subVersionKey / doctorVersionKey / globalVersionKey 是三个维度的版本号键。
func (c *ScheduleCache) subVersionKey(subdepartmentID int64) string {
	return fmt.Sprintf("%s:sub:%d", scheduleCacheVersionKeyPrefix, subdepartmentID)
}

func (c *ScheduleCache) doctorVersionKey(doctorID int64) string {
	return fmt.Sprintf("%s:doctor:%d", scheduleCacheVersionKeyPrefix, doctorID)
}

func (c *ScheduleCache) globalVersionKey() string {
	return scheduleCacheVersionKeyPrefix + ":all"
}

// parseCacheVersion 把 MGET 返回的任意值解析成版本号；nil、空串与非法值一律按 0 处理。
func parseCacheVersion(value any) int64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case int64:
		if typed > 0 {
			return typed
		}
		return 0
	case int:
		if typed > 0 {
			return int64(typed)
		}
		return 0
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil || parsed < 0 {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

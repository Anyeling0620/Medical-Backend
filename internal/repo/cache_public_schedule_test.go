package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/domain/schedule"
)

// 本文件是公开排班读缓存装饰器与版本号失效的单元测试（不连 Redis、不连数据库）。
//
// Redis 用「内嵌 redis.UniversalClient 并覆写个别方法」的内存替身（与 redis_token_test.go
// 的 fakeRedisClient 同一取舍）：不引入 miniredis 等新依赖；内嵌接口保证一旦实现越界调用
// 未覆写的命令会直接 panic，便于发现。
//
// 覆盖的验收点：
//   a) 未命中回源并回填、命中不再回源；
//   b) INCR 版本号后旧键立即失效（不依赖 TTL 到期）；
//   d) 空结果缓存不会挡住新增排班；
//   e) Redis 报错 fail open（读路径直查、写路径失效失败只记日志）；
//   f) 同一 key 并发只回源一次（singleflight）；
//   h) 空结果在缓存路径与直查路径都输出 items:[] 而不是 null。

// fakeScheduleCacheRedis 是 cacheRedis 的内存替身：一张 map 同时存放值键与版本键，
// 覆写 Get / MGet / Set / Incr 四个命令，并支持按命令注入错误以覆盖 fail open。
type fakeScheduleCacheRedis struct {
	redis.UniversalClient

	mu     sync.Mutex
	values map[string]string
	// 以下错误字段各自只影响对应命令，便于区分「版本号读不到」「值键读不到」「回填失败」。
	getErr  error
	mgetErr error
	setErr  error
	incrErr error

	getCalls  int
	setCalls  int
	mgetCalls int
	incrCalls int
	setKeys   []string
	setTTLs   []time.Duration
	incrKeys  []string
}

func newFakeScheduleCacheRedis() *fakeScheduleCacheRedis {
	return &fakeScheduleCacheRedis{values: map[string]string{}}
}

func (f *fakeScheduleCacheRedis) Get(_ context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return redis.NewStringResult("", f.getErr)
	}
	value, ok := f.values[key]
	if !ok {
		// 真实 Redis 对不存在的键返回 redis.Nil，装饰器据此判定未命中。
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (f *fakeScheduleCacheRedis) MGet(_ context.Context, keys ...string) *redis.SliceCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mgetCalls++
	if f.mgetErr != nil {
		return redis.NewSliceResult(nil, f.mgetErr)
	}
	// 真实 Redis 的 MGET 对不存在的键返回 nil 元素，这里保持一致。
	values := make([]any, len(keys))
	for i, key := range keys {
		if value, ok := f.values[key]; ok {
			values[i] = value
		}
	}
	return redis.NewSliceResult(values, nil)
}

func (f *fakeScheduleCacheRedis) Set(_ context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return redis.NewStatusResult("", f.setErr)
	}
	var payload string
	switch typed := value.(type) {
	case string:
		payload = typed
	case []byte:
		payload = string(typed)
	default:
		payload = fmt.Sprint(typed)
	}
	f.values[key] = payload
	f.setKeys = append(f.setKeys, key)
	f.setTTLs = append(f.setTTLs, expiration)
	return redis.NewStatusResult("OK", nil)
}

func (f *fakeScheduleCacheRedis) Incr(_ context.Context, key string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incrCalls++
	f.incrKeys = append(f.incrKeys, key)
	if f.incrErr != nil {
		return redis.NewIntResult(0, f.incrErr)
	}
	next := int64(1)
	if current, ok := f.values[key]; ok {
		parsed, err := strconv.ParseInt(current, 10, 64)
		if err != nil {
			return redis.NewIntResult(0, err)
		}
		next = parsed + 1
	}
	f.values[key] = strconv.FormatInt(next, 10)
	return redis.NewIntResult(next, nil)
}

// --- 替身的调用记录读取（并发场景下必须持锁）---

func (f *fakeScheduleCacheRedis) storedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.setKeys...)
}

func (f *fakeScheduleCacheRedis) storedValues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	values := make([]string, 0, len(f.setKeys))
	for _, key := range f.setKeys {
		values = append(values, f.values[key])
	}
	return values
}

func (f *fakeScheduleCacheRedis) recordedTTLs() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.setTTLs...)
}

func (f *fakeScheduleCacheRedis) getCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls
}

func (f *fakeScheduleCacheRedis) mgetCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mgetCalls
}

func (f *fakeScheduleCacheRedis) setCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setCalls
}

func (f *fakeScheduleCacheRedis) incrKeysRecorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.incrKeys...)
}

// value 返回键当前存储的原始值（版本键里是十进制字符串）。
func (f *fakeScheduleCacheRedis) value(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[key]
	return value, ok
}

// seed 预置一个键值，用于构造「命中」「载荷损坏」等场景。
func (f *fakeScheduleCacheRedis) seed(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
}

// newTestScheduleCache 用同包可见字段直接构造 ScheduleCache：缓存实现只依赖 cacheRedis
// 窄接口，测试无需真的构造 redis.UniversalClient；randFloat 注入固定随机源以便断言抖动。
func newTestScheduleCache(fake *fakeScheduleCacheRedis) *ScheduleCache {
	return &ScheduleCache{
		redis:     fake,
		opts:      CacheOptions{TTL: 8 * time.Second, JitterRatio: 0.2, RedisTimeout: 50 * time.Millisecond},
		randFloat: func() float64 { return 0.5 },
	}
}

// fakePublicScheduleRepo 是 port.PublicScheduleRepository 的内存桩：
// 记录回源次数与入参，并支持注入结果、错误与「回源前钩子」（并发用例的闸门）。
type fakePublicScheduleRepo struct {
	mu     sync.Mutex
	items  []schedule.PublicSchedule
	total  int64
	err    error
	before func()

	calls      int
	lastFilter schedule.PublicScheduleFilter
	lastOffset int
	lastLimit  int
}

func (f *fakePublicScheduleRepo) ListPublicSchedules(_ context.Context, filter schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error) {
	if f.before != nil {
		f.before()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastOffset = offset
	f.lastLimit = limit
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.items, f.total, nil
}

func (f *fakePublicScheduleRepo) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePublicScheduleRepo) setResult(items []schedule.PublicSchedule, total int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = items
	f.total = total
}

// cacheTestFilter 构造公开时段过滤条件；维度传 0 表示不过滤该维度
// （契约 §8.1 允许只给 subdepartmentId 或只给 doctorId）。
func cacheTestFilter(subdepartmentID, doctorID int64, from, to string) schedule.PublicScheduleFilter {
	filter := schedule.PublicScheduleFilter{FromDate: from, ToDate: to}
	if subdepartmentID > 0 {
		filter.SubdepartmentID = &subdepartmentID
	}
	if doctorID > 0 {
		filter.DoctorID = &doctorID
	}
	return filter
}

// wantScheduleCacheKey 按设计文档写死的键格式拼接期望键：
// medical:cache:public:schedules:{scope}:{fromDate}:{toDate}:o{offset}:l{limit}:v{subVer}.{docVer}.{allVer}
// 刻意不复用生产代码，避免「实现怎么写、测试就怎么算」的自我一致。
func wantScheduleCacheKey(scope, from, to string, offset, limit int, subVersion, doctorVersion, allVersion int64) string {
	return fmt.Sprintf("medical:cache:public:schedules:%s:%s:%s:o%d:l%d:v%d.%d.%d",
		scope, from, to, offset, limit, subVersion, doctorVersion, allVersion)
}

const (
	cacheTestFromDate = "2026-09-20"
	cacheTestToDate   = "2026-09-26"
)

// assertEmptyNonNil 断言空结果查询满足契约形状：err 为空、items 非 nil（序列化为 []）、total=0。
func assertEmptyNonNil(t *testing.T, items []schedule.PublicSchedule, total int64, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("空结果查询不应报错: %v", err)
	}
	if items == nil {
		t.Fatalf("空结果必须是 items:[]，实际为 nil（会被序列化成 null）")
	}
	if len(items) != 0 {
		t.Errorf("空结果条目数 = %d, want 0", len(items))
	}
	if total != 0 {
		t.Errorf("空结果 total = %d, want 0", total)
	}
}

// TestCachedPublicScheduleMissThenHit 覆盖验收点 a：
// 未命中时回源数据库并回填缓存；命中时直接返回缓存、不再回源也不再回填。
func TestCachedPublicScheduleMissThenHit(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	cache := newTestScheduleCache(fake)
	repo := &fakePublicScheduleRepo{
		items: []schedule.PublicSchedule{{ScheduleID: 12, Date: cacheTestFromDate, Slot: 1, Maximum: 3, Remaining: 2}},
		total: 1,
	}
	decorator := NewCachedPublicScheduleRepository(repo, cache)
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
	if err != nil {
		t.Fatalf("首次查询（未命中）不应报错: %v", err)
	}
	if repo.callCount() != 1 {
		t.Fatalf("未命中必须回源一次，实际 %d 次", repo.callCount())
	}
	if total != 1 || len(items) != 1 || items[0].ScheduleID != 12 {
		t.Fatalf("回源结果 = %d 条/total=%d, want 1 条/total=1", len(items), total)
	}
	if fake.setCallCount() != 1 {
		t.Errorf("未命中后必须回填一次，实际 %d 次", fake.setCallCount())
	}

	wantKey := wantScheduleCacheKey("sub2", cacheTestFromDate, cacheTestToDate, 0, 20, 0, 0, 0)
	keys := fake.storedKeys()
	if len(keys) != 1 {
		t.Fatalf("回填键数量 = %d, want 1", len(keys))
	}
	if keys[0] != wantKey {
		t.Errorf("缓存键 = %q, want %q", keys[0], wantKey)
	}
	// 抖动 TTL 必须落在 [base*(1-jitter), base*(1+jitter)] = [6.4s, 9.6s]；
	// 这里注入的随机源固定为 0.5，因此期望正好等于基准 8s。
	if ttl := fake.recordedTTLs()[0]; ttl != 8*time.Second {
		t.Errorf("回填 TTL = %v, want 8s（抖动比例 0.2、随机源 0.5 时应等于基准）", ttl)
	}

	items, total, err = decorator.ListPublicSchedules(ctx, filter, 0, 20)
	if err != nil {
		t.Fatalf("第二次查询（命中）不应报错: %v", err)
	}
	if repo.callCount() != 1 {
		t.Errorf("命中后不得再回源，实际回源 %d 次", repo.callCount())
	}
	if fake.setCallCount() != 1 {
		t.Errorf("命中后不得再次回填，实际回填 %d 次", fake.setCallCount())
	}
	if total != 1 || len(items) != 1 || items[0].ScheduleID != 12 {
		t.Errorf("命中结果 = %d 条/total=%d, want 1 条/total=1", len(items), total)
	}
}

// TestCachedPublicScheduleKeySeparatesSlice 覆盖分页切片不得共用缓存：
// 相同 offset 不同 limit（第 1 页 20 条 vs 第 1 页 50 条）以及不同 offset（含非对齐 offset 10）
// 都必须落在不同的键上并各自回源，不能互相返回对方的数据。
// 非对齐 offset 是关键用例：按键码反推页码时 offset=10/limit=20 会算成第 1 页，
// 与 offset=0/limit=20 共用同一个键，直接改成 offset/limit 分片才能避免。
func TestCachedPublicScheduleKeySeparatesSlice(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	repo := &fakePublicScheduleRepo{
		items: []schedule.PublicSchedule{{ScheduleID: 12}},
		total: 1,
	}
	decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	// 第一页 20 条。
	if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
		t.Fatalf("第一次查询不应报错: %v", err)
	}
	// 相同 offset、不同 limit：必须回源，不能读到 20 条的缓存。
	repo.setResult([]schedule.PublicSchedule{{ScheduleID: 12}, {ScheduleID: 13}}, 2)
	if _, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 50); err != nil {
		t.Fatalf("第二次查询不应报错: %v", err)
	} else if total != 2 {
		t.Errorf("相同 offset 不同 limit 不得共用缓存，total = %d, want 2", total)
	}
	// 非对齐 offset：按键码反推页码时它会与 offset=0 撞键。
	repo.setResult([]schedule.PublicSchedule{{ScheduleID: 14}}, 3)
	items, total, err := decorator.ListPublicSchedules(ctx, filter, 10, 20)
	if err != nil {
		t.Fatalf("第三次查询不应报错: %v", err)
	}
	if len(items) != 1 || items[0].ScheduleID != 14 || total != 3 {
		t.Errorf("不同 offset 不得共用缓存，实际 items=%+v total=%d", items, total)
	}
	if repo.callCount() != 3 {
		t.Fatalf("三次不同切片必须各自回源一次，实际回源 %d 次（说明切片共用了同一个键）", repo.callCount())
	}

	keys := fake.storedKeys()
	if len(keys) != 3 {
		t.Fatalf("回填键数量 = %d, want 3", len(keys))
	}
	wantKeys := []string{
		wantScheduleCacheKey("sub2", cacheTestFromDate, cacheTestToDate, 0, 20, 0, 0, 0),
		wantScheduleCacheKey("sub2", cacheTestFromDate, cacheTestToDate, 0, 50, 0, 0, 0),
		wantScheduleCacheKey("sub2", cacheTestFromDate, cacheTestToDate, 10, 20, 0, 0, 0),
	}
	for i, want := range wantKeys {
		if keys[i] != want {
			t.Errorf("第 %d 个缓存键 = %q, want %q", i+1, keys[i], want)
		}
	}
}

// TestCachedPublicScheduleVersionBumpInvalidatesOldKey 覆盖验收点 b：
// 递增版本号后旧键立即失效（下一次查询必须回源），完全不依赖 TTL 到期。
func TestCachedPublicScheduleVersionBumpInvalidatesOldKey(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	cache := newTestScheduleCache(fake)
	repo := &fakePublicScheduleRepo{
		items: []schedule.PublicSchedule{{ScheduleID: 12, Remaining: 2}},
		total: 1,
	}
	decorator := NewCachedPublicScheduleRepository(repo, cache)
	filter := cacheTestFilter(2, 16, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	// 第一次查询：写入 v0.0.0 的键。
	if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
		t.Fatalf("首次查询不应报错: %v", err)
	}
	oldKey := fake.storedKeys()[0]

	// 写路径失效：子科室 2 + 医生 16 两个维度各递增一次（用 INCR 而不是覆盖）。
	if err := cache.BumpSchedules(ctx, 2, 16); err != nil {
		t.Fatalf("BumpSchedules 不应报错: %v", err)
	}
	wantIncr := []string{
		"medical:cache:ver:schedules:sub:2",
		"medical:cache:ver:schedules:doctor:16",
	}
	if got := fake.incrKeysRecorded(); !reflect.DeepEqual(got, wantIncr) {
		t.Errorf("递增的版本键 = %v, want %v", got, wantIncr)
	}
	for _, key := range wantIncr {
		if value, _ := fake.value(key); value != "1" {
			t.Errorf("版本键 %s = %q, want \"1\"", key, value)
		}
	}

	// 不需要等 TTL：数据变化后下一次查询必须回源并拿到新值。
	repo.setResult([]schedule.PublicSchedule{{ScheduleID: 12, Remaining: 1}}, 1)
	items, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
	if err != nil {
		t.Fatalf("版本号递增后的查询不应报错: %v", err)
	}
	if repo.callCount() != 2 {
		t.Fatalf("版本号递增后必须重新回源，实际回源 %d 次（说明旧键仍在生效）", repo.callCount())
	}
	if len(items) != 1 || items[0].Remaining != 1 {
		t.Errorf("必须返回回源后的新值，实际 %+v", items)
	}

	keys := fake.storedKeys()
	if len(keys) != 2 {
		t.Fatalf("回填键数量 = %d, want 2（旧键 + 新键）", len(keys))
	}
	if want := wantScheduleCacheKey("sub2-doc16", cacheTestFromDate, cacheTestToDate, 0, 20, 1, 1, 0); keys[1] != want {
		t.Errorf("新键 = %q, want %q", keys[1], want)
	}
	if keys[1] == oldKey {
		t.Errorf("版本号递增后键必须变化，实际仍是 %q", oldKey)
	}

	// 第三次查询命中新键：连续查询不应继续回源。
	if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
		t.Fatalf("第三次查询不应报错: %v", err)
	}
	if repo.callCount() != 2 {
		t.Errorf("新键应正常命中，实际回源 %d 次", repo.callCount())
	}
}

// TestCachedPublicScheduleVersionScopeIsolation 覆盖版本号的作用域隔离与粗粒度兜底：
//   - 只有「子科室 + 医生」两个维度都已知时才做细粒度失效（两个键各 INCR 一次），
//     它必须同时让「按子科室+医生」与「只按医生」两类查询失效（契约 §8.1 允许只传 doctorId）；
//   - 与本次写操作无关的作用域（其它子科室 + 其它医生）必须继续命中，
//     否则任何写操作都会把整个公开排班缓存打穿；
//   - 任一维度缺失（例如删除时段拿不到作用域）退化为全局失效，兜住所有查询。
func TestCachedPublicScheduleVersionScopeIsolation(t *testing.T) {
	ctx := context.Background()

	t.Run("细粒度失效覆盖子科室与医生两个维度", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		cache := newTestScheduleCache(fake)
		repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
		decorator := NewCachedPublicScheduleRepository(repo, cache)
		planFilter := cacheTestFilter(2, 16, cacheTestFromDate, cacheTestToDate)
		doctorOnly := cacheTestFilter(0, 16, cacheTestFromDate, cacheTestToDate)

		// 先把两类查询都缓存下来（各自未命中回源一次）。
		if _, _, err := decorator.ListPublicSchedules(ctx, planFilter, 0, 20); err != nil {
			t.Fatalf("子科室+医生查询不应报错: %v", err)
		}
		if _, _, err := decorator.ListPublicSchedules(ctx, doctorOnly, 0, 20); err != nil {
			t.Fatalf("只按医生查询不应报错: %v", err)
		}
		if repo.callCount() != 2 {
			t.Fatalf("首次两类查询应各回源一次，实际回源 %d 次", repo.callCount())
		}

		// 细粒度失效：sub + doctor 两个键各 INCR 一次。
		if err := cache.BumpSchedules(ctx, 2, 16); err != nil {
			t.Fatalf("BumpSchedules 不应报错: %v", err)
		}
		wantIncr := []string{"medical:cache:ver:schedules:sub:2", "medical:cache:ver:schedules:doctor:16"}
		if got := fake.incrKeysRecorded(); !reflect.DeepEqual(got, wantIncr) {
			t.Errorf("递增的版本键 = %v, want %v", got, wantIncr)
		}

		// 两类查询的键里分别带医生版本号、子科室版本号，因此都必须重新回源。
		repo.setResult([]schedule.PublicSchedule{{ScheduleID: 12}, {ScheduleID: 13}}, 2)
		if items, _, err := decorator.ListPublicSchedules(ctx, doctorOnly, 0, 20); err != nil {
			t.Fatalf("只按医生查询不应报错: %v", err)
		} else if len(items) != 2 {
			t.Errorf("只按医生的查询必须被医生维度失效，实际返回 %d 条 want 2", len(items))
		}
		if _, _, err := decorator.ListPublicSchedules(ctx, planFilter, 0, 20); err != nil {
			t.Fatalf("子科室+医生查询不应报错: %v", err)
		}
		if repo.callCount() != 4 {
			t.Errorf("两类查询都必须重新回源，实际回源 %d 次（细粒度失效漏掉一个维度会让部分键陈旧到 TTL 到期）", repo.callCount())
		}

		// 新键稳定命中：不重复回源。
		if _, _, err := decorator.ListPublicSchedules(ctx, doctorOnly, 0, 20); err != nil {
			t.Fatalf("只按医生查询不应报错: %v", err)
		}
		if repo.callCount() != 4 {
			t.Errorf("新键应正常命中，实际回源 %d 次", repo.callCount())
		}
	})

	t.Run("无关作用域的失效不影响本查询", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		cache := newTestScheduleCache(fake)
		repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
		decorator := NewCachedPublicScheduleRepository(repo, cache)
		filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)

		if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
			t.Fatalf("首次查询不应报错: %v", err)
		}
		// 另一个子科室 + 另一个医生的细粒度写操作：本查询的键里不含这两个版本号，应继续命中。
		if err := cache.BumpSchedules(ctx, 7, 21); err != nil {
			t.Fatalf("BumpSchedules 不应报错: %v", err)
		}
		if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
			t.Fatalf("第二次查询不应报错: %v", err)
		}
		if repo.callCount() != 1 {
			t.Errorf("无关作用域的失效不应让本查询回源，实际回源 %d 次", repo.callCount())
		}
	})

	t.Run("单维度失效退化为全局失效", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		cache := newTestScheduleCache(fake)
		repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
		decorator := NewCachedPublicScheduleRepository(repo, cache)
		filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)

		if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
			t.Fatalf("首次查询不应报错: %v", err)
		}
		// 缺医生维度：只能全局失效，否则「只按子科室」之外的查询键会陈旧。
		if err := cache.BumpSchedules(ctx, 2, 0); err != nil {
			t.Fatalf("BumpSchedules 不应报错: %v", err)
		}
		if got := fake.incrKeysRecorded(); !reflect.DeepEqual(got, []string{"medical:cache:ver:schedules:all"}) {
			t.Errorf("单维度失效必须只递增全局键，实际 %v", got)
		}
		if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
			t.Fatalf("第二次查询不应报错: %v", err)
		}
		if repo.callCount() != 2 {
			t.Errorf("全局版本号递增后必须重新回源，实际回源 %d 次", repo.callCount())
		}
	})

}

// TestCachedPublicScheduleEmptyResultCached 覆盖验收点 h：
// 空结果会被缓存，且缓存路径与直查路径必须输出同一形状 items:[]（不是 null）。
func TestCachedPublicScheduleEmptyResultCached(t *testing.T) {
	ctx := context.Background()
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)

	t.Run("内嵌仓库返回空非 nil 切片", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		repo := &fakePublicScheduleRepo{items: make([]schedule.PublicSchedule, 0), total: 0}
		decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))

		items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20) // 未命中 → 回源
		assertEmptyNonNil(t, items, total, err)
		items, total, err = decorator.ListPublicSchedules(ctx, filter, 0, 20) // 命中缓存
		assertEmptyNonNil(t, items, total, err)
		if repo.callCount() != 1 {
			t.Errorf("空结果必须被缓存：第二次查询不得回源，实际回源 %d 次", repo.callCount())
		}

		// 缓存载荷本身也必须是 items:[]：反序列化后 Items 非 nil（null 会得到 nil）。
		var payload struct {
			Items []schedule.PublicSchedule `json:"items"`
			Total int64                     `json:"total"`
		}
		stored := fake.storedValues()
		if len(stored) != 1 {
			t.Fatalf("回填载荷数量 = %d, want 1", len(stored))
		}
		if err := json.Unmarshal([]byte(stored[0]), &payload); err != nil {
			t.Fatalf("缓存载荷不是合法 JSON: %v（载荷=%s）", err, stored[0])
		}
		if payload.Items == nil {
			t.Errorf("缓存载荷 items 为 null，契约要求 []：%s", stored[0])
		}
		if strings.Contains(stored[0], `"items":null`) {
			t.Errorf("缓存载荷不得出现 items:null：%s", stored[0])
		}
	})

	t.Run("内嵌仓库返回 nil 切片时缓存路径仍输出 []", func(t *testing.T) {
		// 真实 PostgresScheduleRepository 返回 make([]T, 0)；这里用 nil 覆盖异常实现，
		// 确保装饰器自己的未命中/命中路径都会把空结果归一化为非 nil。
		fake := newFakeScheduleCacheRedis()
		repo := &fakePublicScheduleRepo{items: nil, total: 0}
		decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))

		items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
		assertEmptyNonNil(t, items, total, err)
		items, total, err = decorator.ListPublicSchedules(ctx, filter, 0, 20)
		assertEmptyNonNil(t, items, total, err)
	})
}

// TestCachedPublicScheduleEmptyResultDoesNotHideNewSchedule 覆盖验收点 d：
// 先缓存空结果，再让写路径递增版本号（模拟 CreatePlan），
// 随后查询必须回源看到新增排班，而不是继续返回旧的空结果。
func TestCachedPublicScheduleEmptyResultDoesNotHideNewSchedule(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	cache := newTestScheduleCache(fake)
	repo := &fakePublicScheduleRepo{items: make([]schedule.PublicSchedule, 0), total: 0}
	decorator := NewCachedPublicScheduleRepository(repo, cache)
	filter := cacheTestFilter(2, 16, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	// 第一步：查询并缓存空结果（实测「今天起 7 天」窗口经常是 0 条）。
	items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
	assertEmptyNonNil(t, items, total, err)
	emptyKey := fake.storedKeys()[0]

	// 第二步：CreatePlan 提交后递增「子科室 2 + 医生 16」两个维度的版本号。
	if err := cache.BumpSchedules(ctx, 2, 16); err != nil {
		t.Fatalf("BumpSchedules 不应报错: %v", err)
	}

	// 第三步：新增排班立即可见，不依赖空结果缓存到期。
	repo.setResult([]schedule.PublicSchedule{{ScheduleID: 99, Date: "2026-09-21", Slot: 2, Maximum: 5, Remaining: 5}}, 1)
	items, total, err = decorator.ListPublicSchedules(ctx, filter, 0, 20)
	if err != nil {
		t.Fatalf("版本号失效后的查询不应报错: %v", err)
	}
	if len(items) != 1 || total != 1 {
		t.Fatalf("新增排班必须立即可见，实际 items=%d total=%d（仍返回旧空结果说明空结果缓存未被失效）", len(items), total)
	}
	if repo.callCount() != 2 {
		t.Errorf("版本号失效后必须回源一次，实际回源 %d 次", repo.callCount())
	}
	keys := fake.storedKeys()
	if len(keys) != 2 || keys[1] == emptyKey {
		t.Errorf("新增排班后必须写入变化后的键，旧键=%q 现有键=%v", emptyKey, keys)
	}
}

// TestCachedPublicScheduleFailOpen 覆盖验收点 e：
// 缓存（Redis）故障是性能依赖故障，读路径必须降级直查数据库并返回结果，绝不升级成 5xx；
// 回填失败只记日志；失效失败把错误交回调用方（usecase 只记日志，不让写操作失败）。
func TestCachedPublicScheduleFailOpen(t *testing.T) {
	redisDown := errors.New("redis 连接失败")
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	cases := []struct {
		name         string
		configure    func(*fakeScheduleCacheRedis)
		wantSetCalls int
	}{
		{"版本号读取失败（MGET 报错）", func(f *fakeScheduleCacheRedis) { f.mgetErr = redisDown }, 0},
		{"值键读取失败（GET 报错）", func(f *fakeScheduleCacheRedis) { f.getErr = redisDown }, 1},
		{"回填失败（SET 报错）", func(f *fakeScheduleCacheRedis) { f.setErr = redisDown }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeScheduleCacheRedis()
			tc.configure(fake)
			repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
			decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))

			items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
			if err != nil {
				t.Fatalf("Redis 故障必须 fail open（直查数据库），实际返回错误: %v", err)
			}
			if len(items) != 1 || total != 1 {
				t.Fatalf("必须返回数据库结果，实际 items=%d total=%d", len(items), total)
			}
			if repo.callCount() != 1 {
				t.Errorf("Redis 故障时必须回源一次，实际回源 %d 次", repo.callCount())
			}
			if fake.setCallCount() != tc.wantSetCalls {
				t.Errorf("回填次数 = %d, want %d", fake.setCallCount(), tc.wantSetCalls)
			}
		})
	}

	t.Run("缓存载荷损坏时按未命中处理", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
		decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
		// 预置一个版本号正常但载荷损坏的键（模拟键结构变更后遗留的旧值）。
		fake.seed(wantScheduleCacheKey("sub2", cacheTestFromDate, cacheTestToDate, 0, 20, 0, 0, 0), "{不是合法 JSON")

		items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
		if err != nil {
			t.Fatalf("载荷损坏时不得返回 5xx 语义的错误: %v", err)
		}
		if len(items) != 1 || total != 1 {
			t.Fatalf("必须降级直查数据库，实际 items=%d total=%d", len(items), total)
		}
		if repo.callCount() != 1 {
			t.Errorf("载荷损坏必须回源，实际回源 %d 次", repo.callCount())
		}
	})

	t.Run("写路径失效失败把错误交回调用方", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		fake.incrErr = redisDown
		cache := newTestScheduleCache(fake)
		if err := cache.BumpSchedules(ctx, 2, 16); !errors.Is(err, redisDown) {
			t.Errorf("BumpSchedules err = %v, want %v（调用方据此记录告警日志）", err, redisDown)
		}
	})

	t.Run("回源本身报错时原样向上返回且不回填", func(t *testing.T) {
		// fail open 只针对缓存故障：数据库故障仍按原错误语义上抛（可能是 5xx）。
		databaseDown := errors.New("数据库连接失败")
		fake := newFakeScheduleCacheRedis()
		repo := &fakePublicScheduleRepo{err: databaseDown}
		decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))

		if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); !errors.Is(err, databaseDown) {
			t.Errorf("回源错误必须原样返回，实际 %v", err)
		}
		if fake.setCallCount() != 0 {
			t.Errorf("回源失败不得回填脏数据，实际回填 %d 次", fake.setCallCount())
		}
	})
}

// TestCachedPublicScheduleSingleflight 覆盖验收点 f：同一 key 的并发查询只回源一次。
//
// 用闸门让首个调用方停在回源内部，其余调用方此刻并发进入，从而真实覆盖 singleflight 合并；
// 即使个别调用方在回源结束后才进入，它也会命中刚写入的缓存，因此「只回源一次」是稳定断言。
func TestCachedPublicScheduleSingleflight(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	cache := newTestScheduleCache(fake)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	repo := &fakePublicScheduleRepo{
		items: []schedule.PublicSchedule{{ScheduleID: 12, Maximum: 3, Remaining: 2}},
		total: 1,
		before: func() {
			once.Do(func() { close(started) })
			<-release
		},
	}
	decorator := NewCachedPublicScheduleRepository(repo, cache)
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)

	const concurrency = 8
	var wg sync.WaitGroup
	totals := make([]int64, concurrency)
	errs := make([]error, concurrency)
	itemsByCaller := make([][]schedule.PublicSchedule, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			items, total, err := decorator.ListPublicSchedules(context.Background(), filter, 0, 20)
			itemsByCaller[index], totals[index], errs[index] = items, total, err
		}(i)
	}
	<-started
	// 留出时间让其余调用方进入 singleflight.Do；它们不会因此再触发回源。
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if repo.callCount() != 1 {
		t.Fatalf("并发 %d 次查询只能回源一次，实际回源 %d 次", concurrency, repo.callCount())
	}
	if fake.setCallCount() != 1 {
		t.Errorf("并发回填只应发生一次，实际 %d 次", fake.setCallCount())
	}
	for i := range totals {
		if errs[i] != nil {
			t.Errorf("第 %d 个并发调用返回错误: %v", i, errs[i])
		}
		if totals[i] != 1 {
			t.Errorf("第 %d 个并发调用 total = %d, want 1", i, totals[i])
		}
	}

	// singleflight 会把同一份回源结果共享给所有等待者：装饰器在返回前做了一次浅拷贝，
	// 因此某个调用方原地改写返回值不得影响其它调用方（否则并发请求会互相篡改数据）。
	if len(itemsByCaller[0]) != 1 {
		t.Fatalf("首个调用方应拿到 1 条，实际 %d 条", len(itemsByCaller[0]))
	}
	itemsByCaller[0][0].ScheduleID = 9999
	for i := 1; i < concurrency; i++ {
		if len(itemsByCaller[i]) != 1 {
			t.Fatalf("第 %d 个调用方应拿到 1 条，实际 %d 条", i, len(itemsByCaller[i]))
		}
		if itemsByCaller[i][0].ScheduleID != 12 {
			t.Errorf("第 %d 个调用方看到了被改写的共享值：scheduleId=%d, want 12", i, itemsByCaller[i][0].ScheduleID)
		}
	}
}

// TestCachedPublicScheduleDirectQueryWhenCacheDisabled 覆盖「缓存未启用 / 未注入 Redis」的退化路径：
// 必须与改动前完全一致地直查数据库、不触碰 Redis，并且与缓存路径输出同一形状（验收点 h）。
func TestCachedPublicScheduleDirectQueryWhenCacheDisabled(t *testing.T) {
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	cases := []struct {
		name  string
		build func(repo *fakePublicScheduleRepo) *CachedPublicScheduleRepository
	}{
		{"cache 为 nil（Redis 未连接或总开关关闭）", func(repo *fakePublicScheduleRepo) *CachedPublicScheduleRepository {
			return NewCachedPublicScheduleRepository(repo, nil)
		}},
		{"ScheduleCache 未注入 redis 客户端", func(repo *fakePublicScheduleRepo) *CachedPublicScheduleRepository {
			return &CachedPublicScheduleRepository{repo: repo, cache: &ScheduleCache{}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 仓库返回 nil 切片（真实 PostgresScheduleRepository 返回 make([]T,0)）：
			// 用异常实现逼出 listDirect 的空切片归一化，确保降级路径不会输出 items:null。
			repo := &fakePublicScheduleRepo{items: nil, total: 0}
			decorator := tc.build(repo)

			items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
			assertEmptyNonNil(t, items, total, err)
			if repo.callCount() != 1 {
				t.Errorf("缓存未启用时必须直查数据库一次，实际 %d 次", repo.callCount())
			}

			// 与缓存路径等价性：同一仓库、同一入参在启用缓存时也应得到相同形状与总数。
			fake := newFakeScheduleCacheRedis()
			cachedDecorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
			cachedItems, cachedTotal, err := cachedDecorator.ListPublicSchedules(ctx, filter, 0, 20)
			assertEmptyNonNil(t, cachedItems, cachedTotal, err)
			if cachedTotal != total || len(cachedItems) != len(items) {
				t.Errorf("缓存路径结果 (%d 条/total=%d) 与直查路径 (%d 条/total=%d) 不等价",
					len(cachedItems), cachedTotal, len(items), total)
			}
		})
	}
}

// TestCachedPublicScheduleMissingRepository 内嵌仓库缺失时必须返回错误而不是 panic / 空结果。
func TestCachedPublicScheduleMissingRepository(t *testing.T) {
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	var nilDecorator *CachedPublicScheduleRepository
	if _, _, err := nilDecorator.ListPublicSchedules(ctx, filter, 0, 20); err == nil {
		t.Errorf("nil 接收者必须返回错误")
	}
	if _, _, err := NewCachedPublicScheduleRepository(nil, nil).ListPublicSchedules(ctx, filter, 0, 20); err == nil {
		t.Errorf("内嵌仓库缺失必须返回错误")
	}
}

// TestScheduleCacheBumpSchedulesScopes 覆盖 port.ScheduleCacheVersioner 的作用域约定：
// 只有「子科室 + 医生」两个维度都已知时才做细粒度失效（两个键各 INCR 一次）；
// 任一维度缺失（0 或负数）就只递增全局版本键——只递增单个维度会留下一部分查询键陈旧到 TTL 到期。
func TestScheduleCacheBumpSchedulesScopes(t *testing.T) {
	cases := []struct {
		name            string
		subdepartmentID int64
		doctorID        int64
		wantKeys        []string
	}{
		{"子科室 + 医生都已知（唯一细粒度分支）", 2, 16, []string{"medical:cache:ver:schedules:sub:2", "medical:cache:ver:schedules:doctor:16"}},
		{"只有子科室（缺医生维度 → 全局失效）", 2, 0, []string{"medical:cache:ver:schedules:all"}},
		{"只有医生（缺子科室维度 → 全局失效）", 0, 16, []string{"medical:cache:ver:schedules:all"}},
		{"两个维度都未知（删除时段的兜底）", 0, 0, []string{"medical:cache:ver:schedules:all"}},
		{"负数按未知处理", -1, -2, []string{"medical:cache:ver:schedules:all"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeScheduleCacheRedis()
			cache := newTestScheduleCache(fake)
			ctx := context.Background()

			if err := cache.BumpSchedules(ctx, tc.subdepartmentID, tc.doctorID); err != nil {
				t.Fatalf("BumpSchedules 不应报错: %v", err)
			}
			if got := fake.incrKeysRecorded(); !reflect.DeepEqual(got, tc.wantKeys) {
				t.Errorf("递增的版本键 = %v, want %v", got, tc.wantKeys)
			}
			// 再次失效必须是递增而不是覆盖：否则「第二次写操作」会让键回到 v1，命中第一次的旧值。
			if err := cache.BumpSchedules(ctx, tc.subdepartmentID, tc.doctorID); err != nil {
				t.Fatalf("BumpSchedules 不应报错: %v", err)
			}
			for _, key := range tc.wantKeys {
				if value, _ := fake.value(key); value != "2" {
					t.Errorf("版本键 %s = %q, want \"2\"（必须使用 INCR 而不是 SET）", key, value)
				}
			}
		})
	}

	t.Run("缓存未启用时不报错也不触碰 Redis", func(t *testing.T) {
		ctx := context.Background()
		var nilCache *ScheduleCache
		if err := nilCache.BumpSchedules(ctx, 1, 2); err != nil {
			t.Errorf("nil 缓存接收者必须视为空操作，实际 %v", err)
		}
		empty := &ScheduleCache{}
		if err := empty.BumpSchedules(ctx, 1, 2); err != nil {
			t.Errorf("未注入 redis 客户端时必须视为空操作，实际 %v", err)
		}
	})
}

// TestParseCacheVersion 覆盖版本号解析的健壮性：nil / 空串 / 非法值 / 负数一律按 0
// （即「从未失效过」）处理，保证 Redis 里出现异常值时只会多回源一次，不会 panic。
func TestParseCacheVersion(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int64
	}{
		{"nil（键不存在）", nil, 0},
		{"int64 正值", int64(3), 3},
		{"int64 零值", int64(0), 0},
		{"int64 负值", int64(-5), 0},
		{"int 正值", int(4), 4},
		{"字符串正值", "7", 7},
		{"字符串带空白", "  5  ", 5},
		{"字符串非法", "abc", 0},
		{"字符串负数", "-1", 0},
		{"空字符串", "", 0},
		{"不支持的类型按 0 处理", 3.5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCacheVersion(tc.value); got != tc.want {
				t.Errorf("parseCacheVersion(%#v) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// TestCachedPublicScheduleSkipsStoreWhenTTLNonPositive 覆盖回填的 TTL 兜底：
// Redis 的过期时间 0 表示「永不过期」，余量缓存绝不能写永不过期键；
// 配置非法（TTL <= 0）时必须跳过回填、每次直查数据库，而不是留下永久脏数据。
func TestCachedPublicScheduleSkipsStoreWhenTTLNonPositive(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	cache := &ScheduleCache{
		redis:     fake,
		opts:      CacheOptions{TTL: 0, JitterRatio: 0.2, RedisTimeout: 50 * time.Millisecond},
		randFloat: func() float64 { return 0.5 },
	}
	repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
	decorator := NewCachedPublicScheduleRepository(repo, cache)
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
		if err != nil {
			t.Fatalf("第 %d 次查询不应报错: %v", i, err)
		}
		if len(items) != 1 || total != 1 {
			t.Fatalf("第 %d 次查询必须返回数据库结果，实际 items=%d total=%d", i, len(items), total)
		}
	}
	if fake.setCallCount() != 0 {
		t.Errorf("TTL 非法时不得回填（0 表示永不过期），实际回填 %d 次", fake.setCallCount())
	}
	if keys := fake.storedKeys(); len(keys) != 0 {
		t.Errorf("TTL 非法时不得写入任何键，实际写入 %v", keys)
	}
	if repo.callCount() != 2 {
		t.Errorf("TTL 非法时每次查询都要回源，实际回源 %d 次", repo.callCount())
	}
}

// TestCachedPublicScheduleDirectFallbackNormalizesEmptyItems 覆盖「直查透传」降级分支的空切片归一化：
// 缓存未启用、版本号读取失败（cacheKey 失败）两条分支都必须把内嵌仓库的 nil 切片归一化为
// 空切片，输出 items:[] 而不是 null——fail open 只是不缓存，不能悄悄改掉契约形状。
func TestCachedPublicScheduleDirectFallbackNormalizesEmptyItems(t *testing.T) {
	ctx := context.Background()
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)

	cases := []struct {
		name  string
		build func(repo *fakePublicScheduleRepo) *CachedPublicScheduleRepository
	}{
		{"缓存未启用（cache 为 nil）", func(repo *fakePublicScheduleRepo) *CachedPublicScheduleRepository {
			return NewCachedPublicScheduleRepository(repo, nil)
		}},
		{"版本号读取失败（MGET 报错 → cacheKey 失败）", func(repo *fakePublicScheduleRepo) *CachedPublicScheduleRepository {
			fake := newFakeScheduleCacheRedis()
			fake.mgetErr = errors.New("redis 连接失败")
			return NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 仓库返回 nil 切片（真实实现返回 make([]T, 0)）：逼出 listDirect 的归一化逻辑。
			repo := &fakePublicScheduleRepo{items: nil, total: 0}
			decorator := tc.build(repo)

			items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
			assertEmptyNonNil(t, items, total, err)
			if repo.callCount() != 1 {
				t.Errorf("降级路径必须直查数据库一次，实际 %d 次", repo.callCount())
			}
		})
	}

	// listDirect 是三条直查透传分支（含 singleflight 返回值类型异常）共用的归一化入口，
	// 这里直接对它断言，把第三条分支的归一化行为也钉住。
	t.Run("listDirect 直接调用也归一化空切片", func(t *testing.T) {
		repo := &fakePublicScheduleRepo{items: nil, total: 0}
		decorator := NewCachedPublicScheduleRepository(repo, nil)

		items, total, err := decorator.listDirect(ctx, filter, 0, 20)
		assertEmptyNonNil(t, items, total, err)
	})

	// 直查失败时保持原错误语义：既不吞掉错误，也不返回「半成品」空切片。
	t.Run("直查失败原样返回错误", func(t *testing.T) {
		databaseDown := errors.New("数据库连接失败")
		repo := &fakePublicScheduleRepo{err: databaseDown}
		decorator := NewCachedPublicScheduleRepository(repo, nil)

		items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
		if !errors.Is(err, databaseDown) {
			t.Fatalf("直查失败必须原样返回错误，实际 %v", err)
		}
		if items != nil || total != 0 {
			t.Errorf("直查失败时应返回 (nil, 0)，实际 items=%v total=%d", items, total)
		}
	})
}

// TestCachedPublicScheduleNilFiltersBypassCache 固化「两个过滤维度都为 nil 时短路直查」：
// 子科室与医生都缺失的查询不会出现在契约里（use case 会返回 422），而且它的缓存键只能被
// 全局失效兜住，属于易漏失效的键。装饰器因此必须直接走 listDirect：
// 不读缓存（GET/MGET）、不写缓存（SET）、不做 singleflight。
func TestCachedPublicScheduleNilFiltersBypassCache(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12, Remaining: 2}}, total: 1}
	decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
	// 只给日期窗口，两个过滤维度都不传。
	filter := schedule.PublicScheduleFilter{FromDate: cacheTestFromDate, ToDate: cacheTestToDate}
	ctx := context.Background()

	for i := 1; i <= 2; i++ {
		items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
		if err != nil {
			t.Fatalf("第 %d 次查询不应报错: %v", i, err)
		}
		if len(items) != 1 || total != 1 {
			t.Fatalf("第 %d 次查询必须返回数据库结果，实际 items=%d total=%d", i, len(items), total)
		}
	}
	if repo.callCount() != 2 {
		t.Errorf("无过滤条件时不得缓存，两次查询都要直查，实际回源 %d 次", repo.callCount())
	}
	if fake.getCallCount() != 0 || fake.mgetCallCount() != 0 || fake.setCallCount() != 0 {
		t.Errorf("无过滤条件时不得访问 Redis，实际 GET=%d MGET=%d SET=%d",
			fake.getCallCount(), fake.mgetCallCount(), fake.setCallCount())
	}
	if keys := fake.storedKeys(); len(keys) != 0 {
		t.Errorf("无过滤条件时不得写入任何键，实际 %v", keys)
	}

	// 空结果同样保持 items:[]（短路径走的也是 listDirect 的归一化）。
	repo.setResult(nil, 0)
	items, total, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
	assertEmptyNonNil(t, items, total, err)
}

// TestCachedPublicScheduleReturnedSliceIsolated 固化「调用方原地改写返回值不得污染缓存」：
// 未命中回源拿到切片后原地改写元素，再次查询（缓存命中）必须读到原值。
//
// 注：回源路径在返回前多做了一次浅拷贝，主要目的是隔离 singleflight 同一 key 的并发等待者
// （见 TestCachedPublicScheduleSingleflight）；这里断言的是能稳定观测到的边界隔离，
// 即缓存载荷不因调用方改写而变化。
func TestCachedPublicScheduleReturnedSliceIsolated(t *testing.T) {
	fake := newFakeScheduleCacheRedis()
	repo := &fakePublicScheduleRepo{
		items: []schedule.PublicSchedule{{ScheduleID: 12, Maximum: 3, Remaining: 2}},
		total: 1,
	}
	decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
	filter := cacheTestFilter(2, 0, cacheTestFromDate, cacheTestToDate)
	ctx := context.Background()

	items, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
	if err != nil {
		t.Fatalf("首次查询不应报错: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("首次查询应返回 1 条，实际 %d 条", len(items))
	}
	// 调用方原地改写返回值（模拟 usecase 层的排序/加工，或 handler 层的字段覆盖）。
	items[0].ScheduleID = 9999
	items[0].Remaining = 99

	hitItems, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20)
	if err != nil {
		t.Fatalf("第二次查询不应报错: %v", err)
	}
	if repo.callCount() != 1 {
		t.Fatalf("第二次查询应命中缓存，实际回源 %d 次", repo.callCount())
	}
	if len(hitItems) != 1 {
		t.Fatalf("命中结果应返回 1 条，实际 %d 条", len(hitItems))
	}
	if hitItems[0].ScheduleID != 12 || hitItems[0].Remaining != 2 {
		t.Errorf("缓存值被调用方改写污染：scheduleId=%d remaining=%d, want 12/2",
			hitItems[0].ScheduleID, hitItems[0].Remaining)
	}
}

// TestCachedPublicScheduleNonPositiveFilterBypassCache 覆盖短路条件的另一半输入面：
// 过滤条件是「非 nil 但非正整数」（0 / 负数）时同样没有维度版本可言，必须与 nil 一样直接直查，
// 不得生成 scope=all 的缓存键——那种键只能靠全局失效兜住，是最容易漏失效的一类键。
// 末尾的对照用例确认条件放宽没有过头：医生维度为正整数时仍按正常路径走缓存。
func TestCachedPublicScheduleNonPositiveFilterBypassCache(t *testing.T) {
	ctx := context.Background()
	zero := int64(0)
	negative := int64(-1)
	positive := int64(16)

	cases := []struct {
		name   string
		filter schedule.PublicScheduleFilter
	}{
		{"sub=0 且 doc=nil", schedule.PublicScheduleFilter{SubdepartmentID: &zero, FromDate: cacheTestFromDate, ToDate: cacheTestToDate}},
		{"sub=nil 且 doc=0", schedule.PublicScheduleFilter{DoctorID: &zero, FromDate: cacheTestFromDate, ToDate: cacheTestToDate}},
		{"sub=0 且 doc=0", schedule.PublicScheduleFilter{SubdepartmentID: &zero, DoctorID: &zero, FromDate: cacheTestFromDate, ToDate: cacheTestToDate}},
		{"sub=-1 且 doc=nil", schedule.PublicScheduleFilter{SubdepartmentID: &negative, FromDate: cacheTestFromDate, ToDate: cacheTestToDate}},
		{"sub=nil 且 doc=-1", schedule.PublicScheduleFilter{DoctorID: &negative, FromDate: cacheTestFromDate, ToDate: cacheTestToDate}},
		{"sub=0 且 doc=-1", schedule.PublicScheduleFilter{SubdepartmentID: &zero, DoctorID: &negative, FromDate: cacheTestFromDate, ToDate: cacheTestToDate}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeScheduleCacheRedis()
			repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
			decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))

			for i := 1; i <= 2; i++ {
				items, total, err := decorator.ListPublicSchedules(ctx, tc.filter, 0, 20)
				if err != nil {
					t.Fatalf("第 %d 次查询不应报错: %v", i, err)
				}
				if len(items) != 1 || total != 1 {
					t.Fatalf("第 %d 次查询必须返回数据库结果，实际 items=%d total=%d", i, len(items), total)
				}
			}
			if repo.callCount() != 2 {
				t.Errorf("过滤维度无效时不得缓存，两次查询都要直查，实际回源 %d 次", repo.callCount())
			}
			if fake.getCallCount() != 0 || fake.mgetCallCount() != 0 || fake.setCallCount() != 0 {
				t.Errorf("过滤维度无效时不得访问 Redis，实际 GET=%d MGET=%d SET=%d",
					fake.getCallCount(), fake.mgetCallCount(), fake.setCallCount())
			}
			if keys := fake.storedKeys(); len(keys) != 0 {
				t.Errorf("过滤维度无效时不得写入任何键（尤其不得出现 scope=all），实际 %v", keys)
			}
		})
	}

	// 对照用例：子科室为 0、医生为正整数是契约允许的「只按医生查」，
	// 必须仍然走缓存（否则条件放宽过头会把正常查询也变成每次回源）。
	t.Run("对照：doc 为正整数时仍走缓存", func(t *testing.T) {
		fake := newFakeScheduleCacheRedis()
		repo := &fakePublicScheduleRepo{items: []schedule.PublicSchedule{{ScheduleID: 12}}, total: 1}
		decorator := NewCachedPublicScheduleRepository(repo, newTestScheduleCache(fake))
		filter := schedule.PublicScheduleFilter{
			SubdepartmentID: &zero,
			DoctorID:        &positive,
			FromDate:        cacheTestFromDate,
			ToDate:          cacheTestToDate,
		}

		for i := 1; i <= 2; i++ {
			if _, _, err := decorator.ListPublicSchedules(ctx, filter, 0, 20); err != nil {
				t.Fatalf("第 %d 次查询不应报错: %v", i, err)
			}
		}
		if repo.callCount() != 1 {
			t.Errorf("doc 为正整数时应缓存，第二次查询应命中，实际回源 %d 次", repo.callCount())
		}
		keys := fake.storedKeys()
		want := wantScheduleCacheKey("doc16", cacheTestFromDate, cacheTestToDate, 0, 20, 0, 0, 0)
		if len(keys) != 1 || keys[0] != want {
			t.Errorf("缓存键 = %v, want [%q]（sub=0 不进 scope，doc 维度照常）", keys, want)
		}
	})
}

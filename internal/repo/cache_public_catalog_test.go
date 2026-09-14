package repo

// 本文件是「公开域目录逻辑过期缓存」装饰器（cache_public_catalog.go）的单元测试。
//
// 采用包内测试（package repo）而非 repo_test 外部包：逻辑过期、空值标记、TTL 抖动、
// singleflight 收敛这些行为要通过未导出字段/函数（now、random、ttl、store、readEntry、
// nextLogicalTTL、publicCatalog*CacheKey 等）观察与注入，外部包无法触达。
//
// 测试替身设计：
//   - cacheTestRedisClient：内嵌 redis.UniversalClient，只覆写 Get/Set，既记录调用
//     （键、TTL、值、次数）又可注入故障，避免引入 miniredis 依赖，也不与
//     redis_token_test.go 里的 fakeRedisClient 重名。
//   - stubPublicCatalogRepository：内存假仓储，逐方法记录调用次数并允许注入返回值/错误。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/port"
	publiccatalogservice "Medical-Web-Backend/internal/usecase/publiccatalog"
)

// cacheTestBaseTime 是受控时钟的起点：固定值保证断言 ExpireAt 差值时不依赖真实时间。
var cacheTestBaseTime = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)

// cacheTestClock 是可注入的受控时钟。用原子变量读写，避免并发用例里出现数据竞争。
type cacheTestClock struct {
	nanos atomic.Int64
}

func newCacheTestClock(start time.Time) *cacheTestClock {
	c := &cacheTestClock{}
	c.nanos.Store(start.UnixNano())
	return c
}

func (c *cacheTestClock) now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *cacheTestClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// newCacheTestRepository 按任务约定构造装饰器：Enabled=true、TTL=30m、JitterRatio=0.2，
// 再注入受控时钟，并把 random 固定成 0.5 —— 抖动系数 1+0.2*(2*0.5-1) 恰好等于 1，
// 于是基础 TTL 稳定为 30 分钟，断言 ExpireAt 时不会被抖动干扰。
func newCacheTestRepository(
	t *testing.T,
	source port.PublicCatalogRepository,
	client redis.UniversalClient,
) (*CachedPublicCatalogRepository, *cacheTestClock) {
	t.Helper()
	repo, err := NewCachedPublicCatalogRepository(source, client, PublicCatalogCacheOptions{
		Enabled:     true,
		TTL:         30 * time.Minute,
		JitterRatio: 0.2,
	})
	if err != nil {
		t.Fatalf("构造公开目录缓存装饰器失败：%v", err)
	}
	clock := newCacheTestClock(cacheTestBaseTime)
	repo.now = clock.now
	repo.random = func() float64 { return 0.5 }
	return repo, clock
}

// waitUntil 轮询等待条件成立：用带超时的轮询代替固定长睡眠，既避免拖慢测试，
// 又能在异步重建失败时给出明确超时提示（而不是靠 sleep 后侥幸通过）。
func waitUntil(t *testing.T, timeout time.Duration, description string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, description)
}

// countGoroutinesInSingleflightDo 统计当前栈上仍停留在 x/sync/singleflight.(*Group).Do 的
// goroutine 数：leader 阻塞在回源、follower 阻塞在内部 WaitGroup 上都算一个。
//
// 为什么要靠栈文本来数：singleflight 没有暴露「当前有几个等待者」的接口，而并发回源类断言
// （回源次数必须恰好为 1）只有在「全部调用方都已经进入同一个 flight」之后放行才具备确定性。
// 用固定 time.Sleep 既慢又不能保证，这里改成轮询这个可观测事实。
//
// runtime.Stack 在缓冲不足时会把输出截断（此时返回 n == len(buf)），截断后的栈文本会漏数
// goroutine，让 waitForSingleflightWaiters 假失败，所以这里按需扩容后重试。
func countGoroutinesInSingleflightDo() int {
	const frame = "x/sync/singleflight.(*Group).Do"
	const maxSize = 1 << 26
	for size := 1 << 20; ; size *= 2 {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) || size >= maxSize {
			return strings.Count(string(buf[:n]), frame)
		}
	}
}

// waitForSingleflightWaiters 轮询等待恰好有 want 个 goroutine 进入 singleflight 的 Do。
// 超时即失败，避免把「并发窗口没复现」误判成「实现正确」。
func waitForSingleflightWaiters(t *testing.T, want int, description string) {
	t.Helper()
	waitUntil(t, 3*time.Second, description, func() bool {
		return countGoroutinesInSingleflightDo() >= want
	})
}

// cacheTestSetCall 记录一次 Set 调用的键、物理 TTL 与值。
type cacheTestSetCall struct {
	Key        string
	Value      string
	Expiration time.Duration
	// Failed 表示这次写入被注入的故障打回：值不会落盘，调用方应据此进入退避。
	Failed bool
}

// cacheTestRedisClient 是 redis.UniversalClient 的最小测试替身：
// 只覆写实现真正用到的 Get 与 Set，其余方法沿用内嵌接口（未实现，被调用即 panic，
// 便于发现实现越界访问了别的 Redis 命令）。
type cacheTestRedisClient struct {
	redis.UniversalClient

	mu       sync.Mutex
	values   map[string]string
	getErr   error
	getHook  func(key string) *redis.StringCmd
	setErr   error
	getCalls []string
	setCalls []cacheTestSetCall
}

func newCacheTestRedisClient() *cacheTestRedisClient {
	return &cacheTestRedisClient{values: map[string]string{}}
}

// Get 记录读取的键，并按「钩子 > 注入错误 > 命中 > 键不存在」的顺序返回应答。
func (c *cacheTestRedisClient) Get(_ context.Context, key string) *redis.StringCmd {
	c.mu.Lock()
	c.getCalls = append(c.getCalls, key)
	hook := c.getHook
	getErr := c.getErr
	value, ok := c.values[key]
	c.mu.Unlock()

	if hook != nil {
		return hook(key)
	}
	if getErr != nil {
		return redis.NewStringResult("", getErr)
	}
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

// Set 记录写入的键、物理 TTL 与值，同时写入内存映射，让后续 Get 能命中。
// 注入 setErr 时只记录尝试、不落盘，用于验证「回填持续失败 → 退避」。
func (c *cacheTestRedisClient) Set(_ context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	c.mu.Lock()
	defer c.mu.Unlock()
	encoded := ""
	switch typed := value.(type) {
	case []byte:
		encoded = string(typed)
	case string:
		encoded = typed
	}
	c.setCalls = append(c.setCalls, cacheTestSetCall{
		Key:        key,
		Value:      encoded,
		Expiration: expiration,
		Failed:     c.setErr != nil,
	})
	if c.setErr != nil {
		return redis.NewStatusResult("", c.setErr)
	}
	if c.values == nil {
		c.values = map[string]string{}
	}
	c.values[key] = encoded
	return redis.NewStatusResult("OK", nil)
}

// seed 预置缓存内容，用于构造「命中」「逻辑过期」「空值标记」等初始状态。
func (c *cacheTestRedisClient) seed(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = map[string]string{}
	}
	c.values[key] = value
}

func (c *cacheTestRedisClient) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.getCalls)
}

func (c *cacheTestRedisClient) setCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.setCalls)
}

func (c *cacheTestRedisClient) getCallsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.getCalls...)
}

func (c *cacheTestRedisClient) setCallsFor(key string) []cacheTestSetCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var matched []cacheTestSetCall
	for _, call := range c.setCalls {
		if call.Key == key {
			matched = append(matched, call)
		}
	}
	return matched
}

// stubPublicCatalogRepository 是 port.PublicCatalogRepository 的内存假实现。
// 每个方法都记录调用次数，并允许用例注入返回值与错误。
type stubPublicCatalogRepository struct {
	mu sync.Mutex

	departmentsCalls    int
	findDepartmentCalls int
	subdepartmentsCalls int
	doctorsCalls        int
	findDoctorCalls     int

	listDepartments    func(ctx context.Context, f catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error)
	findDepartment     func(ctx context.Context, id int64) (*catalog.Department, error)
	listSubdepartments func(ctx context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error)
	listDoctors        func(ctx context.Context, f catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error)
	findDoctor         func(ctx context.Context, id int64) (*catalog.PublicDoctorDetail, error)
}

var _ port.PublicCatalogRepository = (*stubPublicCatalogRepository)(nil)

func (s *stubPublicCatalogRepository) ListPublicDepartments(ctx context.Context, f catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error) {
	s.mu.Lock()
	s.departmentsCalls++
	load := s.listDepartments
	s.mu.Unlock()
	if load == nil {
		return nil, 0, nil
	}
	return load(ctx, f, offset, limit)
}

func (s *stubPublicCatalogRepository) FindPublicDepartment(ctx context.Context, id int64) (*catalog.Department, error) {
	s.mu.Lock()
	s.findDepartmentCalls++
	load := s.findDepartment
	s.mu.Unlock()
	if load == nil {
		return nil, nil
	}
	return load(ctx, id)
}

func (s *stubPublicCatalogRepository) ListPublicSubdepartments(ctx context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error) {
	s.mu.Lock()
	s.subdepartmentsCalls++
	load := s.listSubdepartments
	s.mu.Unlock()
	if load == nil {
		return nil, 0, nil
	}
	return load(ctx, departmentID, offset, limit)
}

func (s *stubPublicCatalogRepository) ListPublicDoctors(ctx context.Context, f catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error) {
	s.mu.Lock()
	s.doctorsCalls++
	load := s.listDoctors
	s.mu.Unlock()
	if load == nil {
		return nil, 0, nil
	}
	return load(ctx, f, offset, limit)
}

func (s *stubPublicCatalogRepository) FindPublicDoctor(ctx context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	s.mu.Lock()
	s.findDoctorCalls++
	load := s.findDoctor
	s.mu.Unlock()
	if load == nil {
		return nil, nil
	}
	return load(ctx, id)
}

func (s *stubPublicCatalogRepository) departmentsCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.departmentsCalls
}

func (s *stubPublicCatalogRepository) findDepartmentCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findDepartmentCalls
}

func (s *stubPublicCatalogRepository) subdepartmentsCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subdepartmentsCalls
}

func (s *stubPublicCatalogRepository) doctorsCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doctorsCalls
}

func (s *stubPublicCatalogRepository) findDoctorCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findDoctorCalls
}

// cacheTestEntryJSON 拼出与实现一致的信封 JSON；payload 为 nil 时省略 payload 字段，
// 即「查不到」空值标记。
func cacheTestEntryJSON(t *testing.T, expireAt time.Time, payload any) string {
	t.Helper()
	envelope := map[string]any{"expireAt": expireAt.UnixMilli()}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化缓存载荷失败：%v", err)
		}
		envelope["payload"] = json.RawMessage(raw)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("序列化缓存信封失败：%v", err)
	}
	return string(encoded)
}

// cacheTestDecodePage 从记录的 Set 值里解出列表类载荷，便于断言回填内容。
func cacheTestDecodePage[T any](t *testing.T, raw string) publicCatalogPagePayload[T] {
	t.Helper()
	var envelope publicCatalogCacheEntry
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("解析缓存信封失败：%v（raw=%s）", err, raw)
	}
	var page publicCatalogPagePayload[T]
	if err := json.Unmarshal(envelope.Payload, &page); err != nil {
		t.Fatalf("解析缓存载荷失败：%v（payload=%s）", err, envelope.Payload)
	}
	return page
}

// TestCachedPublicCatalogMissLoadsThenServesFromCache 未命中路径：第一次必须回源并回填，
// 第二次必须由缓存承接而不再打库。这是装饰器最基本的前提，若回填失败则缓存形同虚设。
func TestCachedPublicCatalogMissLoadsThenServesFromCache(t *testing.T) {
	want := []catalog.Department{{ID: 1, Name: "内科"}}
	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			return want, 1, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)
	ctx := context.Background()

	first, firstTotal, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("首次调用不应报错：%v", err)
	}
	if firstTotal != 1 {
		t.Fatalf("首次调用 total = %d, want 1", firstTotal)
	}
	if len(first) != 1 || first[0].Name != "内科" {
		t.Fatalf("首次调用数据 = %+v, want %+v", first, want)
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("未命中应回源 1 次，got %d", got)
	}
	if got := client.setCount(); got != 1 {
		t.Fatalf("未命中应回填 1 次，got %d", got)
	}

	second, secondTotal, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("第二次调用不应报错：%v", err)
	}
	if len(second) != 1 || second[0].Name != "内科" || secondTotal != 1 {
		t.Fatalf("第二次调用应由缓存承接，got items=%+v total=%d", second, secondTotal)
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("命中后不应再回源，got %d", got)
	}
}

// TestCachedPublicCatalogHitWithinLogicalTTL 命中且未逻辑过期：直接返回缓存值、不回源。
// 若这里回源，缓存就退化成了「每次都查库」，失去意义。
func TestCachedPublicCatalogHitWithinLogicalTTL(t *testing.T) {
	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			t.Fatal("命中未过期的缓存时不应回源")
			return nil, 0, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, clock := newCacheTestRepository(t, source, client)

	key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
	client.seed(key, cacheTestEntryJSON(t, clock.now().Add(10*time.Minute), publicCatalogPagePayload[catalog.Department]{
		Items: []catalog.Department{{ID: 9, Name: "缓存科室"}},
		Total: 3,
	}))

	items, total, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("命中缓存不应报错：%v", err)
	}
	if len(items) != 1 || items[0].ID != 9 || items[0].Name != "缓存科室" || total != 3 {
		t.Fatalf("应返回缓存值，got items=%+v total=%d", items, total)
	}
	if got := source.departmentsCallCount(); got != 0 {
		t.Fatalf("命中未过期缓存不应回源，got %d", got)
	}
}

// TestCachedPublicCatalogReturnsStaleThenRebuildsAsync 逻辑过期：必须「先返回旧值、
// 再异步重建」。这是本装饰器的核心语义——若同步等待回源则请求延迟被数据库拖住，
// 若返回新值则说明根本没有走缓存。异步重建用轮询+超时断言，避免固定长睡眠掩盖失败。
func TestCachedPublicCatalogReturnsStaleThenRebuildsAsync(t *testing.T) {
	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			return []catalog.Department{{ID: 2, Name: "新科室"}}, 5, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, clock := newCacheTestRepository(t, source, client)

	key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
	client.seed(key, cacheTestEntryJSON(t, clock.now().Add(-time.Second), publicCatalogPagePayload[catalog.Department]{
		Items: []catalog.Department{{ID: 1, Name: "旧科室"}},
		Total: 1,
	}))

	items, total, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("逻辑过期时不应报错：%v", err)
	}
	if len(items) != 1 || items[0].Name != "旧科室" || total != 1 {
		t.Fatalf("逻辑过期时应先返回旧值，got items=%+v total=%d", items, total)
	}

	waitUntil(t, 3*time.Second, "异步重建回填新值", func() bool { return client.setCount() >= 1 })

	calls := client.setCallsFor(key)
	if len(calls) != 1 {
		t.Fatalf("异步重建应回填 1 次，got %d", len(calls))
	}
	rebuilt := cacheTestDecodePage[catalog.Department](t, calls[0].Value)
	if len(rebuilt.Items) != 1 || rebuilt.Items[0].Name != "新科室" || rebuilt.Total != 5 {
		t.Fatalf("异步重建后的缓存 = %+v, want 新科室/5", rebuilt)
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("异步重建应只回源 1 次，got %d", got)
	}
}

// TestCachedPublicCatalogConcurrentMissLoadsOnce 并发未命中：N 个 goroutine 同时打到
// 同一个 key，singleflight 必须把回源收敛成 1 次，否则缓存击穿会让数据库瞬间承压。
// 用 channel 阻塞回源函数，保证所有调用方在回源完成前都进入 flight，稳定复现并发窗口。
func TestCachedPublicCatalogConcurrentMissLoadsOnce(t *testing.T) {
	const goroutines = 16

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once

	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return []catalog.Department{{ID: 1, Name: "内科"}}, 1, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)

	errs := make([]error, goroutines)
	totals := make([]int64, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, totals[idx], errs[idx] = repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
		}(i)
	}

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("等待首次回源开始超时")
	}
	// 回源仍被 release 阻塞：等全部调用方确实进入同一个 flight 再放行，替代固定睡眠。
	waitForSingleflightWaiters(t, goroutines, "全部调用方进入同一个 flight")
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个 goroutine 报错：%v", i, err)
		}
		if totals[i] != 1 {
			t.Fatalf("第 %d 个 goroutine total = %d, want 1", i, totals[i])
		}
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("并发未命中应只回源 1 次，got %d", got)
	}
	if got := client.setCount(); got != 1 {
		t.Fatalf("并发未命中应只回填 1 次，got %d", got)
	}
}

// TestCachedPublicCatalogExpiredConcurrentRebuildsOnce 逻辑过期后的并发请求：每个请求
// 都会触发异步重建，但 singleflight 必须把它们收敛成 1 次回源。否则缓存一过期就会出现
// 「旧值 + 重建风暴」同时发生，热点 key 反而更容易打垮数据库。
func TestCachedPublicCatalogExpiredConcurrentRebuildsOnce(t *testing.T) {
	const goroutines = 16

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once

	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return []catalog.Department{{ID: 2, Name: "新科室"}}, 5, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, clock := newCacheTestRepository(t, source, client)

	key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
	client.seed(key, cacheTestEntryJSON(t, clock.now().Add(-time.Minute), publicCatalogPagePayload[catalog.Department]{
		Items: []catalog.Department{{ID: 1, Name: "旧科室"}},
		Total: 1,
	}))

	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _, errs[idx] = repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
		}(i)
	}
	// 请求线程只负责派生异步重建，等它们全部返回即可确定重建 goroutine 都已创建。
	wg.Wait()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("等待异步重建开始超时")
	}
	// 重建仍被 release 阻塞：等全部异步重建进入同一个 flight 再放行，替代固定睡眠。
	waitForSingleflightWaiters(t, goroutines, "全部异步重建进入同一个 flight")
	close(release)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个 goroutine 报错：%v", i, err)
		}
	}
	waitUntil(t, 3*time.Second, "异步重建回填新值", func() bool { return client.setCount() >= 1 })
	// 放行前已确认全部重建都进入同一个 flight，因此只可能有一次回源与一次回填。
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("并发异步重建应只回源 1 次，got %d", got)
	}
	if got := client.setCount(); got != 1 {
		t.Fatalf("并发异步重建应只回填 1 次，got %d", got)
	}
}

// TestCachedPublicCatalogEmptySliceShapeParity 空集合的形状一致性：底层返回 nil 切片时，
// 「未命中回源」与「命中缓存」两条路径都必须给出非 nil 的空切片，且缓存载荷里的 items
// 必须是 [] 而不是 null。否则加缓存就等于悄悄改了响应契约（items:null vs items:[]）。
func TestCachedPublicCatalogEmptySliceShapeParity(t *testing.T) {
	source := &stubPublicCatalogRepository{
		// 明确返回 nil 切片，模拟「查得到但没有数据」。
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			return nil, 0, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)
	ctx := context.Background()

	// 路径一：未命中回源。
	missItems, missTotal, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("未命中路径不应报错：%v", err)
	}
	if missItems == nil {
		t.Fatalf("未命中路径必须返回非 nil 空切片，got nil")
	}
	if len(missItems) != 0 || missTotal != 0 {
		t.Fatalf("未命中路径应返回 len=0/total=0，got len=%d total=%d", len(missItems), missTotal)
	}

	// 路径二：命中缓存。
	hitItems, hitTotal, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("命中路径不应报错：%v", err)
	}
	if hitItems == nil {
		t.Fatalf("命中路径必须返回非 nil 空切片，got nil")
	}
	if len(hitItems) != 0 || hitTotal != 0 {
		t.Fatalf("命中路径应返回 len=0/total=0，got len=%d total=%d", len(hitItems), hitTotal)
	}

	// 两条路径的响应必须完全等价。
	if len(hitItems) != len(missItems) {
		t.Fatalf("两条路径长度不一致：hit=%d miss=%d", len(hitItems), len(missItems))
	}
	for i := range missItems {
		if hitItems[i] != missItems[i] {
			t.Fatalf("两条路径第 %d 项不一致：hit=%+v miss=%+v", i, hitItems[i], missItems[i])
		}
	}
	// 底层仓储确实只被打了 1 次（第二次走了缓存）。
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("应在首次回源后走缓存，回源次数 = %d", got)
	}

	key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
	calls := client.setCallsFor(key)
	if len(calls) != 1 {
		t.Fatalf("应回填 1 次，got %d", len(calls))
	}
	var envelope publicCatalogCacheEntry
	if err := json.Unmarshal([]byte(calls[0].Value), &envelope); err != nil {
		t.Fatalf("解析缓存信封失败：%v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Payload, &fields); err != nil {
		t.Fatalf("解析缓存载荷失败：%v", err)
	}
	if got := string(fields["items"]); got != "[]" {
		t.Errorf("缓存载荷 items = %s, want []（不得为 null）", got)
	}
}

// TestCachedPublicCatalogTTLJitterBounds TTL 抖动：random 注入 0、0.5、接近 1 时，
// 逻辑 TTL 必须落在 [30m*0.8, 30m*1.2] 且随 random 单调递增。抖动是为了打散同一时刻
// 批量过期造成的缓存雪崩，区间错了就失去意义（负值还会被静默回退成基础 TTL）。
func TestCachedPublicCatalogTTLJitterBounds(t *testing.T) {
	source := &stubPublicCatalogRepository{}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)

	lower, upper := 24*time.Minute, 36*time.Minute
	cases := []struct {
		name   string
		random float64
	}{
		{"下界", 0},
		{"中点", 0.5},
		{"上界附近", 0.999999},
	}
	var previous time.Duration
	for i, tc := range cases {
		repo.random = func() float64 { return tc.random }
		got := repo.nextLogicalTTL()
		if got < lower || got > upper {
			t.Errorf("%s：nextLogicalTTL() = %s，超出 [%s, %s]", tc.name, got, lower, upper)
		}
		if i == 1 && got != 30*time.Minute {
			t.Errorf("random=0.5 应恰好等于基础 TTL 30m，got %s", got)
		}
		if i > 0 && got <= previous {
			t.Errorf("TTL 应随 random 单调递增：random=%v 得到 %s，前一档为 %s", tc.random, got, previous)
		}
		previous = got
	}
	// 关闭抖动时直接返回基础 TTL。
	repo.random = func() float64 { return 0 }
	repo.jitterRatio = 0
	if got := repo.nextLogicalTTL(); got != repo.ttl {
		t.Errorf("抖动比例为 0 时应返回基础 TTL %s，got %s", repo.ttl, got)
	}
}

// TestCachedPublicCatalogRedisReadErrorFallsBack Redis 读取故障必须 fail open：
// 请求仍然成功返回底层数据、error 为 nil。若这里返回错误，缓存故障就会被放大成 5xx，
// 缓存这个纯性能依赖反而拖低了整体可用性。
func TestCachedPublicCatalogRedisReadErrorFallsBack(t *testing.T) {
	want := []catalog.Department{{ID: 1, Name: "内科"}}
	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			return want, 1, nil
		},
	}
	client := newCacheTestRedisClient()
	client.getErr = errors.New("redis 连接中断")
	repo, _ := newCacheTestRepository(t, source, client)

	items, total, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("Redis 故障时应降级直查并返回 nil 错误，got %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Name != "内科" {
		t.Fatalf("降级直查应返回底层数据，got items=%+v total=%d", items, total)
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("降级直查应回源 1 次，got %d", got)
	}
	if got := client.setCount(); got != 0 {
		t.Fatalf("读取故障的降级路径不应回填，got %d", got)
	}
}

// TestCachedPublicCatalogDepartmentNotFoundNegativeCache 穿透防护（科室详情）：
// 底层返回 sql.ErrNoRows 时要写「空值标记」短 TTL 缓存，挡住反复查不存在的 ID；
// 再次调用必须命中标记、不再打库，且仍然还原成 sql.ErrNoRows（语义与直查一致）。
func TestCachedPublicCatalogDepartmentNotFoundNegativeCache(t *testing.T) {
	source := &stubPublicCatalogRepository{
		findDepartment: func(context.Context, int64) (*catalog.Department, error) {
			return nil, sql.ErrNoRows
		},
	}
	client := newCacheTestRedisClient()
	repo, clock := newCacheTestRepository(t, source, client)
	ctx := context.Background()

	if _, err := repo.FindPublicDepartment(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("首次查询应返回 sql.ErrNoRows，got %v", err)
	}
	if got := source.findDepartmentCallCount(); got != 1 {
		t.Fatalf("首次查询应回源 1 次，got %d", got)
	}

	key := publicCatalogDepartmentCacheKey(7)
	calls := client.setCallsFor(key)
	if len(calls) != 1 {
		t.Fatalf("查不到时应写入 1 次空值标记，got %d", len(calls))
	}
	var envelope publicCatalogCacheEntry
	if err := json.Unmarshal([]byte(calls[0].Value), &envelope); err != nil {
		t.Fatalf("解析空值标记失败：%v", err)
	}
	if envelope.Payload != nil {
		t.Errorf("空值标记不应带载荷，got %s", envelope.Payload)
	}
	// 逻辑 TTL 必须是 60 秒（物理 TTL 为其 2 倍），不能像正常数据那样固化半小时。
	if delta := time.UnixMilli(envelope.ExpireAt).Sub(clock.now()); delta != publicCatalogCacheMissTTL {
		t.Errorf("空值标记逻辑 TTL = %s，want %s", delta, publicCatalogCacheMissTTL)
	}
	if want := publicCatalogCacheMissTTL * publicCatalogCachePhysicalFactor; calls[0].Expiration != want {
		t.Errorf("空值标记物理 TTL = %s，want %s", calls[0].Expiration, want)
	}

	if _, err := repo.FindPublicDepartment(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("命中空值标记应返回 sql.ErrNoRows，got %v", err)
	}
	if got := source.findDepartmentCallCount(); got != 1 {
		t.Fatalf("命中空值标记不应再打库，回源次数 = %d", got)
	}
}

// TestCachedPublicCatalogDoctorNotFoundNegativeCache 穿透防护（医生详情）：
// 与科室详情同理，但载荷类型不同（PublicDoctorDetail），必须同样支持空值标记，
// 否则不存在的医生 ID 可以被反复用来打库。
func TestCachedPublicCatalogDoctorNotFoundNegativeCache(t *testing.T) {
	source := &stubPublicCatalogRepository{
		findDoctor: func(context.Context, int64) (*catalog.PublicDoctorDetail, error) {
			return nil, sql.ErrNoRows
		},
	}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)
	ctx := context.Background()

	if _, err := repo.FindPublicDoctor(ctx, 11); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("首次查询应返回 sql.ErrNoRows，got %v", err)
	}
	if got := source.findDoctorCallCount(); got != 1 {
		t.Fatalf("首次查询应回源 1 次，got %d", got)
	}
	key := publicCatalogDoctorCacheKey(11)
	calls := client.setCallsFor(key)
	if len(calls) != 1 {
		t.Fatalf("查不到时应写入 1 次空值标记，got %d", len(calls))
	}
	var envelope publicCatalogCacheEntry
	if err := json.Unmarshal([]byte(calls[0].Value), &envelope); err != nil {
		t.Fatalf("解析空值标记失败：%v", err)
	}
	if envelope.Payload != nil {
		t.Errorf("空值标记不应带载荷，got %s", envelope.Payload)
	}

	if _, err := repo.FindPublicDoctor(ctx, 11); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("命中空值标记应返回 sql.ErrNoRows，got %v", err)
	}
	if got := source.findDoctorCallCount(); got != 1 {
		t.Fatalf("命中空值标记不应再打库，回源次数 = %d", got)
	}
}

// TestCachedPublicCatalogCorruptPayloadTreatedAsMiss 载荷损坏必须按未命中处理：
// 键结构升级或人工误写会留下解析不了的脏数据，此时应回源覆盖而不是把 500 抛给用户。
// 覆盖两种损坏形态：整个值不是 JSON，以及信封合法但 payload 与目标类型不匹配。
func TestCachedPublicCatalogCorruptPayloadTreatedAsMiss(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"整体非法 JSON", "这不是合法的 JSON"},
		{"信封合法但载荷类型不匹配", `{"expireAt":1,"payload":"不是对象"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &stubPublicCatalogRepository{
				listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
					return []catalog.Department{{ID: 1, Name: "内科"}}, 1, nil
				},
			}
			client := newCacheTestRedisClient()
			key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
			client.seed(key, tc.raw)
			repo, _ := newCacheTestRepository(t, source, client)

			items, _, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
			if err != nil {
				t.Fatalf("脏载荷应按未命中处理而不是报错，got %v", err)
			}
			if len(items) != 1 || items[0].Name != "内科" {
				t.Fatalf("脏载荷应回源返回正确数据，got %+v", items)
			}
			if got := source.departmentsCallCount(); got != 1 {
				t.Fatalf("脏载荷应回源 1 次，got %d", got)
			}
			if got := client.setCount(); got != 1 {
				t.Fatalf("脏载荷应被回源结果覆盖，回填次数 = %d", got)
			}

			// 覆盖之后第二次请求应走缓存。
			if _, _, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20); err != nil {
				t.Fatalf("覆盖后第二次请求不应报错：%v", err)
			}
			if got := source.departmentsCallCount(); got != 1 {
				t.Fatalf("覆盖后应走缓存，回源次数 = %d", got)
			}
		})
	}
}

// TestCachedPublicCatalogDisabledOrNilClientPassthrough 总开关关闭或 Redis 客户端为 nil
// 时必须完全退化为纯透传：不读也不写缓存。否则「关掉缓存」这个运维开关会变成半失效状态，
// 或者因为 nil 客户端 panic。
func TestCachedPublicCatalogDisabledOrNilClientPassthrough(t *testing.T) {
	newSource := func() *stubPublicCatalogRepository {
		return &stubPublicCatalogRepository{
			listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
				return []catalog.Department{{ID: 1, Name: "内科"}}, 1, nil
			},
		}
	}

	t.Run("总开关关闭", func(t *testing.T) {
		source := newSource()
		client := newCacheTestRedisClient()
		repo, err := NewCachedPublicCatalogRepository(source, client, PublicCatalogCacheOptions{Enabled: false})
		if err != nil {
			t.Fatalf("关闭缓存时构造不应报错：%v", err)
		}
		for i := 0; i < 2; i++ {
			if _, _, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20); err != nil {
				t.Fatalf("透传调用不应报错：%v", err)
			}
		}
		if got := source.departmentsCallCount(); got != 2 {
			t.Fatalf("关闭缓存时每次都应回源，回源次数 = %d", got)
		}
		if got := client.getCount(); got != 0 {
			t.Errorf("关闭缓存时不应读缓存，Get 次数 = %d", got)
		}
		if got := client.setCount(); got != 0 {
			t.Errorf("关闭缓存时不应写缓存，Set 次数 = %d", got)
		}
	})

	t.Run("Redis 客户端为 nil", func(t *testing.T) {
		source := newSource()
		repo, err := NewCachedPublicCatalogRepository(source, nil, PublicCatalogCacheOptions{
			Enabled:     true,
			TTL:         30 * time.Minute,
			JitterRatio: 0.2,
		})
		if err != nil {
			t.Fatalf("未注入 Redis 客户端时构造不应报错：%v", err)
		}
		for i := 0; i < 2; i++ {
			items, _, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
			if err != nil {
				t.Fatalf("透传调用不应报错：%v", err)
			}
			if len(items) != 1 {
				t.Fatalf("透传应返回底层数据，got %+v", items)
			}
		}
		if got := source.departmentsCallCount(); got != 2 {
			t.Fatalf("未注入客户端时每次都应回源，回源次数 = %d", got)
		}
	})
}

// TestCachedPublicCatalogNonCanonicalPaginationBypassesCache 非规范分页组合必须绕过缓存：
// 键里只带 page/pageSize 两个分页维度，offset%limit != 0 或 limit<=0 时无法安全还原成页码，
// 强行折叠会让「第 2 页」和「第 5 条起」命中同一个键，返回错页数据。
func TestCachedPublicCatalogNonCanonicalPaginationBypassesCache(t *testing.T) {
	cases := []struct {
		name   string
		offset int
		limit  int
	}{
		{"offset 不是 limit 的整数倍", 5, 20},
		{"limit 为 0", 0, 0},
		{"limit 为负数", 0, -1},
		{"offset 为负数", -20, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &stubPublicCatalogRepository{
				listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
					return []catalog.Department{{ID: 1, Name: "内科"}}, 1, nil
				},
			}
			client := newCacheTestRedisClient()
			repo, _ := newCacheTestRepository(t, source, client)

			for i := 0; i < 2; i++ {
				if _, _, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, tc.offset, tc.limit); err != nil {
					t.Fatalf("绕缓存调用不应报错：%v", err)
				}
			}
			if got := source.departmentsCallCount(); got != 2 {
				t.Fatalf("非规范分页应每次回源，回源次数 = %d", got)
			}
			if got := client.getCount(); got != 0 {
				t.Errorf("非规范分页不应读缓存，Get 次数 = %d", got)
			}
			if got := client.setCount(); got != 0 {
				t.Errorf("非规范分页不应写缓存，Set 次数 = %d", got)
			}
		})
	}
}

// TestCachedPublicCatalogSubdepartmentsMissingDepartmentNotCached 科室不存在时，
// ListPublicSubdepartments 的 sql.ErrNoRows 不得被折叠成「空列表 + 200」并且缓存起来：
// 那会把 404 悄悄改成 200，属于改契约；两次调用都必须继续回源并返回 sql.ErrNoRows。
func TestCachedPublicCatalogSubdepartmentsMissingDepartmentNotCached(t *testing.T) {
	source := &stubPublicCatalogRepository{
		listSubdepartments: func(context.Context, int64, int, int) ([]catalog.Subdepartment, int64, error) {
			return nil, 0, sql.ErrNoRows
		},
	}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		items, total, err := repo.ListPublicSubdepartments(ctx, 3, 0, 20)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("第 %d 次调用应返回 sql.ErrNoRows，got %v", i+1, err)
		}
		if items != nil || total != 0 {
			t.Fatalf("第 %d 次调用不应返回空列表，got items=%v total=%d", i+1, items, total)
		}
	}
	if got := source.subdepartmentsCallCount(); got != 2 {
		t.Fatalf("科室不存在时两次都应回源，回源次数 = %d", got)
	}
	if got := client.setCount(); got != 0 {
		t.Fatalf("科室不存在时不得缓存空列表，Set 次数 = %d", got)
	}
}

// TestCachedPublicCatalogCacheKeyFormat 键格式契约：列表键必须带资源名、过滤摘要（16 位
// 哈希）、分页与版本；详情键形如 medical:cache:public:department:7:v1；不同过滤条件与
// 不同页必须落到不同键——少一个维度就会命中错页或错过滤条件的数据。
func TestCachedPublicCatalogCacheKeyFormat(t *testing.T) {
	departmentListPattern := regexp.MustCompile(`^medical:cache:public:departments:[0-9a-f]{16}:p1:s20:v1$`)
	doctorListPattern := regexp.MustCompile(`^medical:cache:public:doctors:[0-9a-f]{16}:p2:s20:v1$`)

	departmentsKey := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
	if !departmentListPattern.MatchString(departmentsKey) {
		t.Errorf("科室列表键 = %q，不符合 %q", departmentsKey, departmentListPattern)
	}
	doctorsKey := publicCatalogDoctorsCacheKey(catalog.PublicDoctorFilter{}, 2, 20)
	if !doctorListPattern.MatchString(doctorsKey) {
		t.Errorf("医生列表键 = %q，不符合 %q", doctorsKey, doctorListPattern)
	}

	if got := publicCatalogDepartmentCacheKey(7); got != "medical:cache:public:department:7:v1" {
		t.Errorf("科室详情键 = %q, want %q", got, "medical:cache:public:department:7:v1")
	}
	if got := publicCatalogDoctorCacheKey(11); got != "medical:cache:public:doctor:11:v1" {
		t.Errorf("医生详情键 = %q, want %q", got, "medical:cache:public:doctor:11:v1")
	}
	if got := publicCatalogSubdepartmentsCacheKey(3, 1, 20); got != "medical:cache:public:subdepts:3:p1:s20:v1" {
		t.Errorf("子科室列表键 = %q, want %q", got, "medical:cache:public:subdepts:3:p1:s20:v1")
	}

	// 不同过滤条件 / 不同分页必须落在不同键上。
	outpatient := false
	pageTwo := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 2, 20)
	pageSizeTen := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 10)
	filtered := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{Outpatient: &outpatient}, 1, 20)
	for name, key := range map[string]string{
		"不同页":    pageTwo,
		"不同页长":   pageSizeTen,
		"不同过滤条件": filtered,
	} {
		if key == departmentsKey {
			t.Errorf("%s 应产生不同键，却都是 %q", name, key)
		}
	}

	departmentID := int64(3)
	filteredDoctors := publicCatalogDoctorsCacheKey(catalog.PublicDoctorFilter{DepartmentID: &departmentID}, 2, 20)
	if filteredDoctors == doctorsKey {
		t.Errorf("不同过滤条件的医生键应不同，却都是 %q", filteredDoctors)
	}
	if got := publicCatalogDoctorsCacheKey(catalog.PublicDoctorFilter{}, 3, 20); got == doctorsKey {
		t.Errorf("不同页的医生键应不同，却都是 %q", got)
	}

	// 实际调用必须使用上面这套键，避免键构造函数与调用点脱节。
	source := &stubPublicCatalogRepository{}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)
	if _, _, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20); err != nil {
		t.Fatalf("调用不应报错：%v", err)
	}
	gets := client.getCallsSnapshot()
	if len(gets) != 1 || !departmentListPattern.MatchString(gets[0]) {
		t.Fatalf("实际读取的键 = %v，want 匹配 %q", gets, departmentListPattern)
	}
}

// cacheTestDecodeEntry 解析替身记录的 Set 值（缓存信封），用于判断载荷是空值标记还是真实数据。
func cacheTestDecodeEntry(t *testing.T, raw string) publicCatalogCacheEntry {
	t.Helper()
	var entry publicCatalogCacheEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatalf("解析缓存信封失败：%v（raw=%s）", err, raw)
	}
	return entry
}

// TestCachedPublicCatalogNegativeMarkerExpiresLogically 空值标记必须和正常数据一样遵守逻辑过期。
// 修复前命中空值标记会直接短路返回，负缓存一直挡到物理 TTL（120 秒）才消失，「刚新增的
// 科室/医生」在这段时间里始终 404，且 60 秒逻辑过期点不会做任何刷新。本用例断言：逻辑过期后
// 仍然先返回旧的「查不到」结论，但同时触发异步重建，把标记/真实数据重新写回。
func TestCachedPublicCatalogNegativeMarkerExpiresLogically(t *testing.T) {
	t.Run("重建仍是查不到时刷新空值标记", func(t *testing.T) {
		source := &stubPublicCatalogRepository{
			findDepartment: func(context.Context, int64) (*catalog.Department, error) {
				return nil, sql.ErrNoRows
			},
		}
		client := newCacheTestRedisClient()
		repo, clock := newCacheTestRepository(t, source, client)
		ctx := context.Background()
		key := publicCatalogDepartmentCacheKey(7)

		if _, err := repo.FindPublicDepartment(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("首次查询应返回 sql.ErrNoRows，got %v", err)
		}
		if got := source.findDepartmentCallCount(); got != 1 {
			t.Fatalf("首次查询应回源 1 次，got %d", got)
		}

		// 前进到逻辑 TTL（60s）之后、物理 TTL（120s）之前。
		clock.advance(61 * time.Second)

		// (a) 先返回旧的「查不到」结论，请求不等待重建。
		if _, err := repo.FindPublicDepartment(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("空值标记逻辑过期后仍应先返回 sql.ErrNoRows，got %v", err)
		}
		// (b) 同时触发异步重建：回源与回填都应再发生一次。
		waitUntil(t, 3*time.Second, "空值标记逻辑过期后触发异步重建并重新回填", func() bool {
			return source.findDepartmentCallCount() >= 2 && client.setCount() >= 2
		})
		calls := client.setCallsFor(key)
		if len(calls) != 2 {
			t.Fatalf("空值标记应被重新回填，Set 次数 = %d", len(calls))
		}
		last := cacheTestDecodeEntry(t, calls[len(calls)-1].Value)
		if last.Payload != nil {
			t.Errorf("重建结果仍是查不到，应继续写空值标记，got payload=%s", last.Payload)
		}
	})

	t.Run("重建时实体已存在则恢复为真实数据", func(t *testing.T) {
		var calls atomic.Int32
		want := &catalog.Department{ID: 7, Name: "恢复的科室"}
		source := &stubPublicCatalogRepository{
			findDepartment: func(context.Context, int64) (*catalog.Department, error) {
				if calls.Add(1) == 1 {
					return nil, sql.ErrNoRows
				}
				return want, nil
			},
		}
		client := newCacheTestRedisClient()
		repo, clock := newCacheTestRepository(t, source, client)
		ctx := context.Background()
		key := publicCatalogDepartmentCacheKey(7)

		if _, err := repo.FindPublicDepartment(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("首次查询应返回 sql.ErrNoRows，got %v", err)
		}
		clock.advance(61 * time.Second)

		// 第二次调用仍返回旧结论（404），但后台重建这次能查到真实科室。
		if _, err := repo.FindPublicDepartment(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("逻辑过期当下仍应先返回 sql.ErrNoRows，got %v", err)
		}
		waitUntil(t, 3*time.Second, "异步重建把空值标记替换为真实数据", func() bool {
			calls := client.setCallsFor(key)
			if len(calls) < 2 {
				return false
			}
			return cacheTestDecodeEntry(t, calls[len(calls)-1].Value).Payload != nil
		})

		// 第三次调用应命中重建后的真实数据，不再是 404。
		got, err := repo.FindPublicDepartment(ctx, 7)
		if err != nil {
			t.Fatalf("重建完成后不应再返回错误：%v", err)
		}
		if got == nil || got.ID != 7 || got.Name != "恢复的科室" {
			t.Fatalf("应返回重建后的真实科室，got %+v", got)
		}
		if n := source.findDepartmentCallCount(); n != 2 {
			t.Fatalf("应回源 2 次（1 次未命中 + 1 次重建），got %d", n)
		}
	})
}

// TestCachedPublicCatalogRedisErrorCoalescesLoads Redis 读取故障的降级路径同样必须合并并发回源。
// 修复前降级分支直接调用 load(ctx)，Redis 一挂，同一热点 key 的并发请求会各自打库，把缓存
// 故障放大成数据库故障；同时降级路径应保持 store=false，不向已经出问题的 Redis 追加写请求。
func TestCachedPublicCatalogRedisErrorCoalescesLoads(t *testing.T) {
	const goroutines = 16

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once

	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return []catalog.Department{{ID: 1, Name: "内科"}}, 1, nil
		},
	}
	client := newCacheTestRedisClient()
	client.getErr = errors.New("redis 连接中断")
	repo, _ := newCacheTestRepository(t, source, client)

	errs := make([]error, goroutines)
	totals := make([]int64, goroutines)
	names := make([]string, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			items, total, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
			errs[idx] = err
			totals[idx] = total
			if len(items) == 1 {
				names[idx] = items[0].Name
			}
		}(i)
	}

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("等待首次降级回源开始超时")
	}
	// 回源仍被 release 阻塞：等全部调用方确实进入同一个 flight 再放行，替代固定睡眠。
	waitForSingleflightWaiters(t, goroutines, "全部调用方进入降级回源的同一个 flight")
	close(release)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("第 %d 个 goroutine 应 fail open（err=nil），got %v", i, errs[i])
		}
		if totals[i] != 1 || names[i] != "内科" {
			t.Fatalf("第 %d 个 goroutine 数据异常：total=%d name=%q", i, totals[i], names[i])
		}
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("Redis 故障降级也应合并并发回源，回源次数 = %d", got)
	}
	if got := client.setCount(); got != 0 {
		t.Fatalf("Redis 故障降级不应回填，Set 次数 = %d", got)
	}
}

// TestCachedPublicCatalogAsyncRebuildPanicIsContained 异步重建跑在脱离请求的 goroutine 里，
// HTTP 层的 gin.Recovery 覆盖不到：回源 panic 若不自己 recover，会直接终止整个进程（缓存给
// 进程新增崩溃面）。本用例让第一次重建 panic，断言进程存活、singleflight 的 flight 被清理，
// 且后续调用仍能正常重建并返回真实数据。
func TestCachedPublicCatalogAsyncRebuildPanicIsContained(t *testing.T) {
	var calls atomic.Int32
	panicked := make(chan struct{})
	var panicOnce sync.Once

	source := &stubPublicCatalogRepository{
		findDepartment: func(context.Context, int64) (*catalog.Department, error) {
			if calls.Add(1) == 1 {
				panicOnce.Do(func() { close(panicked) })
				panic("模拟异步重建回源崩溃")
			}
			return &catalog.Department{ID: 7, Name: "恢复的科室"}, nil
		},
	}
	client := newCacheTestRedisClient()
	repo, clock := newCacheTestRepository(t, source, client)
	ctx := context.Background()
	key := publicCatalogDepartmentCacheKey(7)

	// 预置一条已逻辑过期的正常值：请求会先返回旧值，再异步重建（本次回源将 panic）。
	client.seed(key, cacheTestEntryJSON(t, clock.now().Add(-time.Minute), &catalog.Department{ID: 7, Name: "旧科室"}))

	stale, err := repo.FindPublicDepartment(ctx, 7)
	if err != nil {
		t.Fatalf("逻辑过期时不应报错：%v", err)
	}
	if stale == nil || stale.Name != "旧科室" {
		t.Fatalf("应先返回旧值，got %+v", stale)
	}

	select {
	case <-panicked:
	case <-time.After(3 * time.Second):
		t.Fatal("等待异步重建进入 panic 分支超时")
	}
	// 等这个 panic 的 flight 被彻底清理（doCall 先删除映射项、再把 panic 抛给调用方）：
	// 不等待就发起下一次调用，新调用可能加入这个即将 panic 的 flight 而被一起炸掉。
	waitUntil(t, 3*time.Second, "panic 的 flight 清理完成", func() bool {
		return countGoroutinesInSingleflightDo() == 0
	})

	// 后续调用仍能正常工作：这次重建会成功并写回真实数据。
	if _, err := repo.FindPublicDepartment(ctx, 7); err != nil {
		t.Fatalf("panic 之后的调用不应报错：%v", err)
	}
	waitUntil(t, 3*time.Second, "panic 之后的重建写回真实数据", func() bool {
		calls := client.setCallsFor(key)
		if len(calls) == 0 {
			return false
		}
		return cacheTestDecodeEntry(t, calls[len(calls)-1].Value).Payload != nil
	})

	got, err := repo.FindPublicDepartment(ctx, 7)
	if err != nil {
		t.Fatalf("最终查询不应报错：%v", err)
	}
	if got == nil || got.ID != 7 || got.Name != "恢复的科室" {
		t.Fatalf("最终应返回重建后的真实数据，got %+v", got)
	}
	if n := source.findDepartmentCallCount(); n != 2 {
		t.Fatalf("应回源 2 次（1 次 panic + 1 次成功），got %d", n)
	}
}

// TestCachedPublicCatalogFilterTokenNoCollision 可空过滤条件的编码必须让「未传」与「传值」
// 彼此不可混淆。修复前 publicCatalogStringToken(nil) 与 publicCatalogStringToken(&"-") 都得到
// "-"，于是 /public/doctors 的「不传 name」与「name=-」落到同一个缓存键：两者结果集不同，
// 既会互相返回错数据，匿名调用者还能用 ?name=- 往热门键里投毒。本用例把 token 编码格式锁死，
// 任何回归都会在这里立刻暴露，而不是等到线上表现为串数据。
func TestCachedPublicCatalogFilterTokenNoCollision(t *testing.T) {
	empty := ""
	dash := "-"
	zero := int64(0)
	no := false

	// token 级：未传一律是 "n"；已传必须带类型/长度前缀，永远不可能等于 "n"。
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"string 未传", publicCatalogStringToken(nil), publicCatalogFilterAbsentToken},
		{"string 空串已传", publicCatalogStringToken(&empty), "s0:"},
		{"string 横线已传", publicCatalogStringToken(&dash), "s1:-"},
		{"int64 未传", publicCatalogInt64Token(nil), publicCatalogFilterAbsentToken},
		{"int64 零已传", publicCatalogInt64Token(&zero), "i0"},
		{"bool 未传", publicCatalogBoolToken(nil), publicCatalogFilterAbsentToken},
		{"bool false 已传", publicCatalogBoolToken(&no), "bfalse"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s：token = %q，want %q", tc.name, tc.got, tc.want)
		}
	}
	if publicCatalogFilterAbsentToken != "n" {
		t.Fatalf("未传哨兵应为 \"n\"，got %q", publicCatalogFilterAbsentToken)
	}
	// 任何用户能构造出来的取值都不能编码成未传哨兵。
	for _, v := range []string{"n", "-", "", " ", "null", "0"} {
		got := publicCatalogStringToken(&v)
		if got == publicCatalogFilterAbsentToken {
			t.Fatalf("已传 name=%q 的 token 不应等于未传哨兵", v)
		}
		if want := fmt.Sprintf("s%d:%s", len(v), v); got != want {
			t.Fatalf("已传 name=%q 的 token = %q，want %q", v, got, want)
		}
	}

	// 键级：name 未传、空串、"-" 三种形态必须落在三个不同键上，否则要么串数据，
	// 要么「name=」（空串同样表示不做名字过滤）被错当成另一个查询。
	seen := map[string]string{}
	for _, tc := range []struct {
		name string
		f    catalog.PublicDoctorFilter
	}{
		{"未传", catalog.PublicDoctorFilter{}},
		{"空串", catalog.PublicDoctorFilter{Name: &empty}},
		{"横线", catalog.PublicDoctorFilter{Name: &dash}},
	} {
		key := publicCatalogDoctorsCacheKey(tc.f, 1, 20)
		if prev, ok := seen[key]; ok {
			t.Fatalf("name=%s 与 name=%s 撞了同一个缓存键：%s", tc.name, prev, key)
		}
		seen[key] = tc.name
	}
	// 排序维度同理：未传（空串）与 "-" 也必须分属不同键。
	if publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20) ==
		publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{Sort: "-"}, 1, 20) {
		t.Fatal("sort 未传与 sort=- 不应落在同一个缓存键上")
	}
}

// TestCachedPublicCatalogNameFilterCacheIsolation 行为级验证：name 未传与 name=- 不会互相串数据。
// token 单测只能证明键不同，这里进一步证明「不同键 → 各自回源 → 各自拿到正确集合」，
// 即匿名调用者无法用一个查询的缓存污染另一个查询的响应。
func TestCachedPublicCatalogNameFilterCacheIsolation(t *testing.T) {
	dash := "-"
	unfiltered := []catalog.PublicDoctor{{ID: 1, Name: "内科医生"}}
	dashed := []catalog.PublicDoctor{{ID: 2, Name: "横线医生"}}
	source := &stubPublicCatalogRepository{
		listDoctors: func(_ context.Context, f catalog.PublicDoctorFilter, _, _ int) ([]catalog.PublicDoctor, int64, error) {
			switch {
			case f.Name == nil:
				return unfiltered, 1, nil
			case *f.Name == dash:
				return dashed, 1, nil
			default:
				return nil, 0, nil
			}
		},
	}
	client := newCacheTestRedisClient()
	repo, _ := newCacheTestRepository(t, source, client)
	ctx := context.Background()

	// 先请求「不传 name」，把结果写进它自己的键。
	first, _, err := repo.ListPublicDoctors(ctx, catalog.PublicDoctorFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("不传 name 查询失败：%v", err)
	}
	if len(first) != 1 || first[0].ID != 1 {
		t.Fatalf("不传 name 应返回 ID=1 的集合，got %+v", first)
	}

	// 再请求 name=-：必须回源（另一个键），且返回 - 对应的集合。
	second, _, err := repo.ListPublicDoctors(ctx, catalog.PublicDoctorFilter{Name: &dash}, 0, 20)
	if err != nil {
		t.Fatalf("name=- 查询失败：%v", err)
	}
	if len(second) != 1 || second[0].ID != 2 {
		t.Fatalf("name=- 应返回 ID=2 的集合（不得复用不传 name 的缓存），got %+v", second)
	}

	if got := source.doctorsCallCount(); got != 2 {
		t.Fatalf("两个不同过滤条件应各自回源一次，got %d", got)
	}
	if got := client.setCount(); got != 2 {
		t.Fatalf("两个不同过滤条件应各自回填一次，got %d", got)
	}

	// 反向再查一次：两个键都已各自缓存，不能再回源，也不能串回对方的集合。
	again, _, err := repo.ListPublicDoctors(ctx, catalog.PublicDoctorFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("重复查询失败：%v", err)
	}
	if len(again) != 1 || again[0].ID != 1 {
		t.Fatalf("重复查询不传 name 仍应返回 ID=1 的集合，got %+v", again)
	}
	if got := source.doctorsCallCount(); got != 2 {
		t.Fatalf("重复查询应命中各自缓存，回源次数不该增加，got %d", got)
	}
}

// TestCachedPublicCatalogListEmptyPayloadTreatedAsDirty 列表键下的空载荷必须按脏数据回源覆盖，
// 不能把内部「查不到」哨兵抛成 500。列表查询永远不会产生空值标记（allowMiss=false），
// 因此 {"expireAt":...} 与 {"expireAt":...,"payload":null} 只可能来自键结构升级或人工误写；
// 若照抄详情路径的空值分支，这两条脏数据会让整个列表接口返回 500。
func TestCachedPublicCatalogListEmptyPayloadTreatedAsDirty(t *testing.T) {
	want := []catalog.Department{{ID: 1, Name: "内科"}}
	cases := []struct {
		name  string
		entry func(expireAt time.Time) string
	}{
		{"payload 字段省略", func(expireAt time.Time) string {
			return fmt.Sprintf(`{"expireAt":%d}`, expireAt.UnixMilli())
		}},
		{"payload 显式为 null", func(expireAt time.Time) string {
			return fmt.Sprintf(`{"expireAt":%d,"payload":null}`, expireAt.UnixMilli())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &stubPublicCatalogRepository{
				listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
					return want, 1, nil
				},
			}
			client := newCacheTestRedisClient()
			repo, clock := newCacheTestRepository(t, source, client)
			ctx := context.Background()
			key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)
			// 预置一条物理仍在（expireAt 在未来）但载荷为空的脏条目。
			client.seed(key, tc.entry(clock.now().Add(time.Hour)))

			items, total, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
			if err != nil {
				t.Fatalf("列表键空载荷应按未命中回源，不得报错（尤其不得抛内部哨兵）：%v", err)
			}
			if len(items) != 1 || items[0].Name != "内科" || total != 1 {
				t.Fatalf("应返回回源得到的真实数据，got items=%+v total=%d", items, total)
			}
			if got := source.departmentsCallCount(); got != 1 {
				t.Fatalf("脏数据应按未命中回源一次，got %d", got)
			}
			calls := client.setCallsFor(key)
			if len(calls) != 1 {
				t.Fatalf("脏数据应被真实载荷覆盖，Set 次数 = %d", len(calls))
			}
			page := cacheTestDecodePage[catalog.Department](t, calls[0].Value)
			if len(page.Items) != 1 || page.Items[0].Name != "内科" {
				t.Fatalf("覆盖后的载荷应是真实数据，got %+v", page.Items)
			}

			// 覆盖之后必须恢复正常缓存语义。
			if _, _, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20); err != nil {
				t.Fatalf("覆盖后查询失败：%v", err)
			}
			if got := source.departmentsCallCount(); got != 1 {
				t.Fatalf("覆盖后应命中缓存，回源次数不该增加，got %d", got)
			}
		})
	}
}

// TestCachedPublicCatalogStoreFailureBackoff Redis 写路径持续失败时必须退避异步重建。
// 预置条目逻辑过期后，每个请求都会走「返回旧值 + 异步重建」；若 Set 一直失败，
// 旧条目的 expireAt 永远停在过去，不退避就会每个请求都额外起一个 goroutine 并多打一次库，
// 把一次 Redis 写故障放大成 goroutine 风暴 + 数据库压力。
func TestCachedPublicCatalogStoreFailureBackoff(t *testing.T) {
	source := &stubPublicCatalogRepository{
		listDepartments: func(context.Context, catalog.DepartmentFilter, int, int) ([]catalog.Department, int64, error) {
			return []catalog.Department{{ID: 1, Name: "新数据"}}, 1, nil
		},
	}
	client := newCacheTestRedisClient()
	client.setErr = errors.New("redis 只读副本，SET 被拒")
	repo, clock := newCacheTestRepository(t, source, client)
	ctx := context.Background()
	key := publicCatalogDepartmentsCacheKey(catalog.DepartmentFilter{}, 1, 20)

	// 预置一条已逻辑过期（物理仍在）的旧值，让每个请求都进入「返回旧值 + 重建」分支。
	client.seed(key, cacheTestEntryJSON(t, clock.now().Add(-time.Minute),
		publicCatalogPagePayload[catalog.Department]{Items: []catalog.Department{{ID: 1, Name: "旧数据"}}, Total: 1}))

	first, _, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
	if err != nil {
		t.Fatalf("逻辑过期时应先返回旧值而不是报错：%v", err)
	}
	if len(first) != 1 || first[0].Name != "旧数据" {
		t.Fatalf("应先返回旧值，got %+v", first)
	}

	// 等异步重建真正发生、并因 Set 失败进入退避窗口（确定性同步，不用固定睡眠）。
	waitUntil(t, 3*time.Second, "回填失败后进入退避窗口", repo.storeFailureBackoffActive)
	// 再等这条 flight 彻底排空。退避标记是在 store() 里设置的，它后面还跟着一条「首次失败」
	// 告警日志；首次格式化本地时间会触发 time.initLocal（Windows 上是注册表 syscall，进程内
	// 只发生一次），本机复现时该 goroutine 正停在这条 syscall 上，此时 singleflight 的映射项
	// 仍然存在。若不等它结束就推进时钟发起新重建，新重建会合法地并入这条旧 flight、于是不再
	// 调用 load，断言源调用次数的用例就会稳定假失败（复审 agent 在本机复现过：单跑必挂、合跑才过）。
	waitUntil(t, 5*time.Second, "上一条重建 flight 排空", func() bool {
		return countGoroutinesInSingleflightDo() == 0
	})
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("首次重建应回源一次，got %d", got)
	}
	if got := client.setCount(); got != 1 {
		t.Fatalf("首次重建应尝试回填一次（失败），got %d", got)
	}

	// 退避窗口内连续请求：都只读旧值，不再触发重建。
	for i := 0; i < 20; i++ {
		items, _, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20)
		if err != nil {
			t.Fatalf("退避窗口内第 %d 次请求不应报错：%v", i, err)
		}
		if len(items) != 1 || items[0].Name != "旧数据" {
			t.Fatalf("退避窗口内第 %d 次请求应继续返回旧值，got %+v", i, items)
		}
	}
	if got := source.departmentsCallCount(); got != 1 {
		t.Fatalf("退避窗口内回源次数不应随请求数增长，got %d", got)
	}
	if got := client.setCount(); got != 1 {
		t.Fatalf("退避窗口内不应再尝试回填，got %d", got)
	}

	// 越过退避窗口后应恢复异步重建。
	clock.advance(publicCatalogCacheStoreFailureBackoff + time.Second)
	// 轮询体里持续发请求，而不是「只发一次再干等计数」：异步重建跑在后台 goroutine 里，
	// 任何一次请求触发的重建都可能与前一条 flight 合并而「合法地不调用 load」，
	// 只轮询计数就会漏掉这种合并、干等到超时。这里以缓存侧事实（该键出现第 2 次 Set 尝试）
	// 与源侧计数共同判定，缓存侧事实保证重建确实跑到了回填这一步。
	waitUntil(t, 5*time.Second, "退避窗口结束后恢复异步重建", func() bool {
		if _, _, err := repo.ListPublicDepartments(ctx, catalog.DepartmentFilter{}, 0, 20); err != nil {
			t.Fatalf("退避结束后请求不应报错：%v", err)
		}
		return source.departmentsCallCount() >= 2 && len(client.setCallsFor(key)) >= 2
	})
}

// TestCachedPublicCatalogLeaderCancelDoesNotAffectFollowers 单个客户端断开不得把同一 flight 上
// 其它客户端一起打成 500。singleflight 会把 leader 的错误广播给所有 follower，若回源沿用
// leader 的请求上下文，leader 客户端一取消，follower 会一起拿到 context.Canceled（handler 即 500）。
// 实现改用 context.WithoutCancel + 超时，本用例把这个不变量锁死。
func TestCachedPublicCatalogLeaderCancelDoesNotAffectFollowers(t *testing.T) {
	run := func(t *testing.T, respectContext bool) {
		const followers = 8

		entered := make(chan struct{})
		release := make(chan struct{})
		var enteredOnce sync.Once

		source := &stubPublicCatalogRepository{
			listDepartments: func(ctx context.Context, _ catalog.DepartmentFilter, _, _ int) ([]catalog.Department, int64, error) {
				enteredOnce.Do(func() { close(entered) })
				if respectContext {
					select {
					case <-ctx.Done():
						return nil, 0, ctx.Err()
					case <-release:
					}
				} else {
					<-release
				}
				return []catalog.Department{{ID: 1, Name: "内科"}}, 1, nil
			},
		}
		client := newCacheTestRedisClient()
		repo, _ := newCacheTestRepository(t, source, client)

		type outcome struct {
			items []catalog.Department
			total int64
			err   error
		}
		results := make([]outcome, followers+1)
		var wg sync.WaitGroup

		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		defer cancelLeader()

		wg.Add(1)
		go func() {
			defer wg.Done()
			items, total, err := repo.ListPublicDepartments(leaderCtx, catalog.DepartmentFilter{}, 0, 20)
			results[0] = outcome{items, total, err}
		}()

		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("等待 leader 进入回源超时")
		}

		for i := 1; i <= followers; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				items, total, err := repo.ListPublicDepartments(context.Background(), catalog.DepartmentFilter{}, 0, 20)
				results[idx] = outcome{items, total, err}
			}(i)
		}

		// 等 leader + N 个 follower 都确实进入同一个 flight 再取消 leader：
		// 用可观测事实替代固定睡眠，避免并发窗口没复现导致的假通过。
		waitForSingleflightWaiters(t, followers+1, "leader 与全部 follower 进入同一个 flight")
		cancelLeader()
		close(release)
		wg.Wait()

		if results[0].err != nil {
			t.Fatalf("leader 自身也不应因客户端断开而失败：%v", results[0].err)
		}
		for i := 1; i <= followers; i++ {
			if results[i].err != nil {
				t.Fatalf("第 %d 个 follower 不应受 leader 取消影响，got err=%v", i, results[i].err)
			}
			if len(results[i].items) != 1 || results[i].items[0].Name != "内科" || results[i].total != 1 {
				t.Fatalf("第 %d 个 follower 数据异常：%+v", i, results[i])
			}
		}
		if got := source.departmentsCallCount(); got != 1 {
			t.Fatalf("同一 flight 只应回源一次，got %d", got)
		}
	}

	t.Run("源仓储忽略上下文", func(t *testing.T) { run(t, false) })
	t.Run("源仓储响应上下文取消", func(t *testing.T) { run(t, true) })
}

// TestNewCachedPublicCatalogRepositoryValidation 构造参数校验：错误配置必须在启动时暴露，
// 而不是运行期退化成「TTL=0 每次调用都算逻辑过期」或「抖动比例非法导致 TTL 为负/翻倍」。
func TestNewCachedPublicCatalogRepositoryValidation(t *testing.T) {
	source := &stubPublicCatalogRepository{}
	cases := []struct {
		name    string
		source  port.PublicCatalogRepository
		options PublicCatalogCacheOptions
		wantErr bool
	}{
		{"底层仓储为 nil", nil, PublicCatalogCacheOptions{Enabled: true, TTL: 30 * time.Minute}, true},
		{"开启缓存但 TTL 为 0", source, PublicCatalogCacheOptions{Enabled: true, TTL: 0}, true},
		{"开启缓存但 TTL 为负", source, PublicCatalogCacheOptions{Enabled: true, TTL: -time.Minute}, true},
		{"抖动比例为负", source, PublicCatalogCacheOptions{Enabled: true, TTL: 30 * time.Minute, JitterRatio: -0.1}, true},
		{"抖动比例等于 1", source, PublicCatalogCacheOptions{Enabled: true, TTL: 30 * time.Minute, JitterRatio: 1}, true},
		{"抖动比例大于 1", source, PublicCatalogCacheOptions{Enabled: true, TTL: 30 * time.Minute, JitterRatio: 1.5}, true},
		{"关闭缓存时 TTL 为 0 不应报错", source, PublicCatalogCacheOptions{Enabled: false, TTL: 0}, false},
		{"抖动比例上界内合法", source, PublicCatalogCacheOptions{Enabled: true, TTL: 30 * time.Minute, JitterRatio: 0.999}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewCachedPublicCatalogRepository(tc.source, newCacheTestRedisClient(), tc.options)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望构造失败，但成功了：%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望构造成功，但失败：%v", err)
			}
			if got == nil {
				t.Fatal("构造成功但返回 nil")
			}
		})
	}
}

// TestCachedPublicCatalogNestedSlicesShapeParity 空形状等价：加缓存不得改变响应形状。
//   - 子科室列表由装饰器 normalizePublicCatalogItems 规范成非 nil 空切片，未命中与命中必须一致；
//   - 医生详情的内嵌数组（subdepartments/prices）在仓储层不做规范化，未命中与命中必须完全等价
//     （当前两条路径都是 nil），对外再由 use case 兜底成非 nil 空切片，因此本用例同时断言
//     「直查」与「缓存」两种装配经 use case 之后都是非 nil 空切片且彼此等价。
func TestCachedPublicCatalogNestedSlicesShapeParity(t *testing.T) {
	t.Run("子科室空列表未命中与命中形状一致", func(t *testing.T) {
		source := &stubPublicCatalogRepository{
			listSubdepartments: func(context.Context, int64, int, int) ([]catalog.Subdepartment, int64, error) {
				return nil, 0, nil
			},
		}
		client := newCacheTestRedisClient()
		repo, _ := newCacheTestRepository(t, source, client)
		ctx := context.Background()

		miss, _, err := repo.ListPublicSubdepartments(ctx, 7, 0, 20)
		if err != nil {
			t.Fatalf("未命中查询失败：%v", err)
		}
		hit, _, err := repo.ListPublicSubdepartments(ctx, 7, 0, 20)
		if err != nil {
			t.Fatalf("命中查询失败：%v", err)
		}
		for name, items := range map[string][]catalog.Subdepartment{"未命中": miss, "命中": hit} {
			if items == nil {
				t.Fatalf("%s 路径应返回非 nil 空切片（items:[] 而不是 null）", name)
			}
			if len(items) != 0 {
				t.Fatalf("%s 路径应为空列表，got %+v", name, items)
			}
		}
		if !reflect.DeepEqual(miss, hit) {
			t.Fatalf("未命中与命中形状不一致：%+v vs %+v", miss, hit)
		}
		if got := source.subdepartmentsCallCount(); got != 1 {
			t.Fatalf("第二次应命中缓存，回源次数 = %d", got)
		}
	})

	t.Run("医生详情内嵌 nil 数组两条路径等价", func(t *testing.T) {
		source := &stubPublicCatalogRepository{
			findDoctor: func(context.Context, int64) (*catalog.PublicDoctorDetail, error) {
				return &catalog.PublicDoctorDetail{
					PublicDoctor:   catalog.PublicDoctor{ID: 16, Name: "熊佳钰"},
					Subdepartments: nil,
					Prices:         nil,
				}, nil
			},
		}
		client := newCacheTestRedisClient()
		repo, _ := newCacheTestRepository(t, source, client)
		ctx := context.Background()

		miss, err := repo.FindPublicDoctor(ctx, 16)
		if err != nil {
			t.Fatalf("未命中查询失败：%v", err)
		}
		hit, err := repo.FindPublicDoctor(ctx, 16)
		if err != nil {
			t.Fatalf("命中查询失败：%v", err)
		}
		if !reflect.DeepEqual(miss, hit) {
			t.Fatalf("未命中与命中结果不一致：%+v vs %+v", miss, hit)
		}
		if (miss.Subdepartments == nil) != (hit.Subdepartments == nil) ||
			(miss.Prices == nil) != (hit.Prices == nil) {
			t.Fatalf("内嵌数组形状不一致：miss=%v/%v hit=%v/%v",
				miss.Subdepartments, miss.Prices, hit.Subdepartments, hit.Prices)
		}
		if got := source.findDoctorCallCount(); got != 1 {
			t.Fatalf("第二次应命中缓存，回源次数 = %d", got)
		}

		// 对外契约：直查装配与缓存装配经 use case 后都必须是非 nil 空切片且彼此等价。
		direct := publiccatalogservice.NewService(source, nil, nil)
		cached := publiccatalogservice.NewService(repo, nil, nil)
		directDetail, err := direct.Doctor(ctx, 16)
		if err != nil {
			t.Fatalf("直查经 use case 失败：%v", err)
		}
		cachedDetail, err := cached.Doctor(ctx, 16)
		if err != nil {
			t.Fatalf("缓存经 use case 失败：%v", err)
		}
		for name, detail := range map[string]*catalog.PublicDoctorDetail{"直查": directDetail, "缓存": cachedDetail} {
			if detail.Subdepartments == nil || detail.Prices == nil {
				t.Fatalf("%s 装配经 use case 后应是非 nil 空切片，got sub=%v prices=%v",
					name, detail.Subdepartments, detail.Prices)
			}
			if len(detail.Subdepartments) != 0 || len(detail.Prices) != 0 {
				t.Fatalf("%s 装配应为空数组，got sub=%v prices=%v",
					name, detail.Subdepartments, detail.Prices)
			}
		}
		if !reflect.DeepEqual(directDetail, cachedDetail) {
			t.Fatalf("直查与缓存经 use case 后不等价：%+v vs %+v", directDetail, cachedDetail)
		}
	})
}

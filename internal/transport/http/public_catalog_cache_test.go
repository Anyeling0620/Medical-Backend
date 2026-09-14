package http

// 本文件覆盖「公开域目录逻辑过期缓存」在 HTTP 层的两件事：
//
//  1. 装配接线：/api/v1/public/* 读的必须是 NewRouter 注入的 publicCatalogRepository，
//     而不是管理端的 doctorRepository（用探针记录调用来证明）；
//  2. 端到端等价：同一个请求在「未命中（冷）」「命中（热）」两种缓存状态、
//     以及「装装饰器」「直接用原始桩」两种装配下，响应体 JSON 必须逐字节一致；
//     空结果时 items 必须是 [] 而不是 null（否则「加缓存」就等于改了响应契约）。
//
// 装饰器需要 Redis，本文件自带一个只覆写 Get/Set 的最小替身，不引入 miniredis 依赖。

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/repo"
)

// publicCacheProbeRepo 是装配探针：实现 port.PublicCatalogRepository 并记录每条方法的调用次数。
// 只要探针被调用，就说明公开域读的是 NewRouter 注入的仓储，而不是别的数据源。
type publicCacheProbeRepo struct {
	mu sync.Mutex

	departmentsCalls    int
	subdepartmentsCalls int
	doctorsCalls        int
	findDepartmentCalls int
	findDoctorCalls     int

	departments []catalog.Department
	doctors     []catalog.PublicDoctor
	department  *catalog.Department
	detail      *catalog.PublicDoctorDetail
}

func (p *publicCacheProbeRepo) ListPublicDepartments(_ context.Context, _ catalog.DepartmentFilter, _, _ int) ([]catalog.Department, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.departmentsCalls++
	return p.departments, int64(len(p.departments)), nil
}

func (p *publicCacheProbeRepo) FindPublicDepartment(_ context.Context, id int64) (*catalog.Department, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.findDepartmentCalls++
	if p.department != nil {
		return p.department, nil
	}
	return &catalog.Department{ID: id, Name: "口腔科"}, nil
}

func (p *publicCacheProbeRepo) ListPublicSubdepartments(_ context.Context, departmentID int64, _, _ int) ([]catalog.Subdepartment, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subdepartmentsCalls++
	return []catalog.Subdepartment{{ID: 2, Name: "口腔颌面外科", DepartmentID: departmentID}}, 1, nil
}

func (p *publicCacheProbeRepo) ListPublicDoctors(_ context.Context, _ catalog.PublicDoctorFilter, _, _ int) ([]catalog.PublicDoctor, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.doctorsCalls++
	return p.doctors, int64(len(p.doctors)), nil
}

func (p *publicCacheProbeRepo) FindPublicDoctor(_ context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.findDoctorCalls++
	if p.detail != nil {
		return p.detail, nil
	}
	return &catalog.PublicDoctorDetail{PublicDoctor: catalog.PublicDoctor{ID: id, Name: "熊佳钰"}}, nil
}

var _ port.PublicCatalogRepository = (*publicCacheProbeRepo)(nil)

func (p *publicCacheProbeRepo) departmentsCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.departmentsCalls
}

func (p *publicCacheProbeRepo) doctorsCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.doctorsCalls
}

// publicCacheRedisClient 是 HTTP 端到端用例专用的最小 Redis 替身：只覆写装饰器用到的 Get/Set。
// 请求在本用例里是串行发出的，因此普通 map 足够，无需加锁。
type publicCacheRedisClient struct {
	redis.UniversalClient
	values map[string]string
}

func newPublicCacheRedisClient() *publicCacheRedisClient {
	return &publicCacheRedisClient{values: map[string]string{}}
}

func (c *publicCacheRedisClient) Get(_ context.Context, key string) *redis.StringCmd {
	value, ok := c.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}

func (c *publicCacheRedisClient) Set(_ context.Context, key string, value any, _ time.Duration) *redis.StatusCmd {
	encoded := ""
	switch typed := value.(type) {
	case []byte:
		encoded = string(typed)
	case string:
		encoded = typed
	}
	c.values[key] = encoded
	return redis.NewStatusResult("OK", nil)
}

// newPublicCacheRouter 用生产路由树装配公开域仓储：只有 publicCatalogRepository 被替换，
// 其余仓库保持 nil（本用例只访问 /public/departments 与 /public/doctors）。
func newPublicCacheRouter(t *testing.T, catalogRepository port.PublicCatalogRepository) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{},
		nil, // userRepository
		nil, // tokenRepository
		nil, // doctorRepository
		catalogRepository,
		nil, // scheduleRepository
		nil, // publicScheduleRepository
		nil, // scheduleCacheVersioner
		nil, // idempotencyStore
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		nil, // paymentRepository
		nil, // alipayGateway
		nil, // medicalRecordRepository
		nil, // doctorPatientRepository
	)
}

// newPublicCacheDecoratedRepo 按生产参数装配装饰器（Enabled=true、TTL=30m、抖动 ±20%）。
func newPublicCacheDecoratedRepo(t *testing.T, source port.PublicCatalogRepository) port.PublicCatalogRepository {
	t.Helper()
	decorated, err := repo.NewCachedPublicCatalogRepository(source, newPublicCacheRedisClient(),
		repo.PublicCatalogCacheOptions{Enabled: true, TTL: 30 * time.Minute, JitterRatio: 0.2})
	if err != nil {
		t.Fatalf("构造公开域缓存装饰器失败：%v", err)
	}
	return decorated
}

// publicCacheItemsField 取出响应体里的 items 原始 JSON，用于断言它是 [] 而不是 null。
func publicCacheItemsField(t *testing.T, body []byte) string {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("解析响应体失败：%v（body=%s）", err, body)
	}
	raw, ok := envelope["items"]
	if !ok {
		t.Fatalf("响应缺少 items 字段：%s", body)
	}
	return string(raw)
}

// TestPublicCatalogRouterUsesInjectedPublicCatalogRepository 装配接线：公开域的科室/医生列表
// 必须读 NewRouter 注入的 publicCatalogRepository。探针被调用即证明公开域没有走管理端 doctorRepository。
func TestPublicCatalogRouterUsesInjectedPublicCatalogRepository(t *testing.T) {
	probe := &publicCacheProbeRepo{}
	engine := newPublicCacheRouter(t, probe)

	for _, target := range []string{"/api/v1/public/departments", "/api/v1/public/doctors"} {
		w := publicAnonymousRequest(engine, target, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%s", target, w.Code, w.Body.String())
		}
	}
	if got := probe.departmentsCallCount(); got != 1 {
		t.Fatalf("科室列表应调用注入的公开域仓储，调用次数 = %d", got)
	}
	if got := probe.doctorsCallCount(); got != 1 {
		t.Fatalf("医生列表应调用注入的公开域仓储，调用次数 = %d", got)
	}
}

// TestPublicCatalogHTTPResponsesIdenticalAcrossCacheStates 端到端等价：同一请求在
// 「直查」「缓存冷启动（未命中）」「缓存命中」三种姿态下响应体 JSON 必须逐字节一致。
func TestPublicCatalogHTTPResponsesIdenticalAcrossCacheStates(t *testing.T) {
	departments := []catalog.Department{{ID: 1, Name: "口腔科", Outpatient: true, Description: "口腔疾病诊疗", Recommended: true}}
	doctors := []catalog.PublicDoctor{{
		ID: 16, Name: "熊佳钰", Sex: "女", PhotoURL: "doctor/16.jpg",
		Degree: "博士", Job: "主任医师", Description: "口腔颌面外科", Recommended: true,
	}}

	rawProbe := &publicCacheProbeRepo{departments: departments, doctors: doctors}
	rawEngine := newPublicCacheRouter(t, rawProbe)

	cachedSource := &publicCacheProbeRepo{departments: departments, doctors: doctors}
	cachedEngine := newPublicCacheRouter(t, newPublicCacheDecoratedRepo(t, cachedSource))

	for _, target := range []string{"/api/v1/public/departments", "/api/v1/public/doctors"} {
		raw := publicAnonymousRequest(rawEngine, target, "")
		cold := publicAnonymousRequest(cachedEngine, target, "")
		hot := publicAnonymousRequest(cachedEngine, target, "")

		if raw.Code != http.StatusOK || cold.Code != http.StatusOK || hot.Code != http.StatusOK {
			t.Fatalf("%s 状态码异常：raw=%d cold=%d hot=%d；body=%s",
				target, raw.Code, cold.Code, hot.Code, cold.Body.String())
		}
		if cold.Body.String() != raw.Body.String() {
			t.Errorf("%s 冷启动响应与直查不一致：%s vs %s", target, cold.Body.String(), raw.Body.String())
		}
		if hot.Body.String() != raw.Body.String() {
			t.Errorf("%s 命中缓存响应与直查不一致：%s vs %s", target, hot.Body.String(), raw.Body.String())
		}
	}

	// 冷/热必须真的走了不同路径，否则「相等」不说明问题。
	if got := rawProbe.departmentsCallCount(); got != 1 {
		t.Fatalf("直查装配科室列表应回源一次，got %d", got)
	}
	if got := rawProbe.doctorsCallCount(); got != 1 {
		t.Fatalf("直查装配医生列表应回源一次，got %d", got)
	}
	if got := cachedSource.departmentsCallCount(); got != 1 {
		t.Fatalf("缓存装配：冷启动回源一次、命中不再回源，期望 1 次，got %d", got)
	}
	if got := cachedSource.doctorsCallCount(); got != 1 {
		t.Fatalf("缓存装配：冷启动回源一次、命中不再回源，期望 1 次，got %d", got)
	}
}

// TestPublicCatalogHTTPEmptyItemsSerializedAsArray 空结果形状：直查、冷启动、命中三条路径
// 的 items 都必须是 []（而不是 null），并且三条路径的响应体完全一致。
func TestPublicCatalogHTTPEmptyItemsSerializedAsArray(t *testing.T) {
	rawEngine := newPublicCacheRouter(t, &publicCacheProbeRepo{})
	cachedEngine := newPublicCacheRouter(t, newPublicCacheDecoratedRepo(t, &publicCacheProbeRepo{}))

	for _, target := range []string{"/api/v1/public/departments", "/api/v1/public/doctors"} {
		raw := publicAnonymousRequest(rawEngine, target, "")
		cold := publicAnonymousRequest(cachedEngine, target, "")
		hot := publicAnonymousRequest(cachedEngine, target, "")

		responses := map[string][]byte{
			"直查":   raw.Body.Bytes(),
			"冷启动":  cold.Body.Bytes(),
			"命中缓存": hot.Body.Bytes(),
		}
		for name, body := range responses {
			if got := publicCacheItemsField(t, body); got != "[]" {
				t.Fatalf("%s %s 的 items 应为 []，got %s", name, target, got)
			}
		}
		if cold.Body.String() != raw.Body.String() || hot.Body.String() != raw.Body.String() {
			t.Fatalf("%s 空结果响应不一致：raw=%s cold=%s hot=%s",
				target, raw.Body.String(), cold.Body.String(), hot.Body.String())
		}
	}
}

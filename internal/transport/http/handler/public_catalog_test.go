package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/transport/http/response"
	publiccatalogservice "Medical-Web-Backend/internal/usecase/publiccatalog"
)

// 本文件覆盖匿名公开查询域 HTTP 处理器（internal/transport/http/handler/public_catalog.go）：
// 200 分页 envelope、404/422/500 映射、公开字段裁剪（不泄漏管理端字段）、照片地址补全、
// 满号时段可见与匿名性（携带垃圾/跨域令牌不改变响应）。
//
// 说明：本域路由不挂认证中间件，因此这里直接用 gin.New() 挂载 handler，
// 再用带 Authorization 头的请求验证 handler 自身不读取令牌、响应体逐字节一致。

// publicCatalogTestNow 固定业务时钟：UTC 2026-09-09 00:00 == 上海 2026-09-09 08:00，业务日为 2026-09-09。
var publicCatalogTestNow = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)

// publicCatalogMinioURL 是照片补全用的 MinIO 公开前缀（带尾斜杠，验证会被规范化）。
const publicCatalogMinioURL = "http://minio.example/"

// publicCatalogRepoStub 是 port.PublicCatalogRepository 的内存 stub：
// 返回预置数据并记录最近一次调用参数，便于断言过滤与分页透传。
type publicCatalogRepoStub struct {
	departments         []catalog.Department
	departmentsTotal    int64
	departmentsErr      error
	departmentDetail    *catalog.Department
	findDepartmentErr   error
	subdepartments      []catalog.Subdepartment
	subdepartmentsTotal int64
	subdepartmentsErr   error
	doctors             []catalog.PublicDoctor
	doctorsTotal        int64
	doctorsErr          error
	doctorDetail        *catalog.PublicDoctorDetail
	findDoctorErr       error

	lastSubdepartmentDepartmentID int64
	lastOffset                    int
	lastLimit                     int
}

// ListPublicDepartments 返回预置科室列表并记录分页参数。
func (s *publicCatalogRepoStub) ListPublicDepartments(_ context.Context, _ catalog.DepartmentFilter, offset, limit int) ([]catalog.Department, int64, error) {
	s.lastOffset, s.lastLimit = offset, limit
	return s.departments, s.departmentsTotal, s.departmentsErr
}

// FindPublicDepartment 返回预置科室详情。
func (s *publicCatalogRepoStub) FindPublicDepartment(_ context.Context, _ int64) (*catalog.Department, error) {
	return s.departmentDetail, s.findDepartmentErr
}

// ListPublicSubdepartments 返回预置子科室列表并记录科室编号与分页参数。
func (s *publicCatalogRepoStub) ListPublicSubdepartments(_ context.Context, departmentID int64, offset, limit int) ([]catalog.Subdepartment, int64, error) {
	s.lastSubdepartmentDepartmentID, s.lastOffset, s.lastLimit = departmentID, offset, limit
	return s.subdepartments, s.subdepartmentsTotal, s.subdepartmentsErr
}

// ListPublicDoctors 返回预置公开医生列表并记录分页参数。
func (s *publicCatalogRepoStub) ListPublicDoctors(_ context.Context, _ catalog.PublicDoctorFilter, offset, limit int) ([]catalog.PublicDoctor, int64, error) {
	s.lastOffset, s.lastLimit = offset, limit
	return s.doctors, s.doctorsTotal, s.doctorsErr
}

// FindPublicDoctor 返回预置医生详情。
func (s *publicCatalogRepoStub) FindPublicDoctor(_ context.Context, _ int64) (*catalog.PublicDoctorDetail, error) {
	return s.doctorDetail, s.findDoctorErr
}

// publicScheduleRepoStub 是 port.PublicScheduleRepository 的内存 stub。
type publicScheduleRepoStub struct {
	items      []schedule.PublicSchedule
	total      int64
	err        error
	lastFilter schedule.PublicScheduleFilter
	lastOffset int
	lastLimit  int
}

// ListPublicSchedules 返回预置时段列表并记录过滤与分页参数。
func (s *publicScheduleRepoStub) ListPublicSchedules(_ context.Context, filter schedule.PublicScheduleFilter, offset, limit int) ([]schedule.PublicSchedule, int64, error) {
	s.lastFilter, s.lastOffset, s.lastLimit = filter, offset, limit
	return s.items, s.total, s.err
}

// newPublicCatalogEngine 装配公开域六个路由（字段裁剪与匿名性断言都在 handler 层完成）。
func newPublicCatalogEngine(t *testing.T, catalogRepo *publicCatalogRepoStub, scheduleRepo *publicScheduleRepoStub, minioURL string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := publiccatalogservice.NewService(catalogRepo, scheduleRepo, func() time.Time { return publicCatalogTestNow })
	publicHandler := NewPublicCatalogHandler(service, minioURL)

	engine := gin.New()
	engine.GET("/api/v1/public/departments", publicHandler.ListDepartments)
	engine.GET("/api/v1/public/departments/:departmentId", publicHandler.DepartmentDetail)
	engine.GET("/api/v1/public/departments/:departmentId/subdepartments", publicHandler.Subdepartments)
	engine.GET("/api/v1/public/doctors", publicHandler.Doctors)
	engine.GET("/api/v1/public/doctors/:doctorId", publicHandler.DoctorDetail)
	engine.GET("/api/v1/public/schedules", publicHandler.Schedules)
	return engine
}

// publicPageBody 断言 200 分页 envelope 的结构（items/page/pageSize/total），并返回 items 与 body。
// items 必须是 JSON 数组：空结果时是 []，不能是 null。
func publicPageBody(t *testing.T, w *httptest.ResponseRecorder) ([]map[string]any, map[string]any) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	assertPublicKeys(t, "分页 envelope", body, "items", "page", "pageSize", "total")

	raw, exists := body["items"]
	if !exists {
		t.Fatalf("响应缺少 items 字段：%s", w.Body.String())
	}
	rawItems, ok := raw.([]any)
	if !ok {
		t.Fatalf("items 不是 JSON 数组（可能是 null）：%s", w.Body.String())
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("items 元素不是 JSON 对象：%s", w.Body.String())
		}
		items = append(items, item)
	}
	return items, body
}

// assertPublicKeys 断言 JSON 对象的键集合恰好等于 want（多一个键就失败，用于字段裁剪回归）。
func assertPublicKeys(t *testing.T, label string, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if strings.Join(got, ",") != strings.Join(sortedWant, ",") {
		t.Errorf("%s 键集合 = [%s], want [%s]", label, strings.Join(got, ","), strings.Join(sortedWant, ","))
	}
}

// assertNoForbiddenKeys 断言原始响应体中不存在被裁剪字段的键。
func assertNoForbiddenKeys(t *testing.T, rawBody string, forbidden ...string) {
	t.Helper()
	for _, key := range forbidden {
		if strings.Contains(rawBody, `"`+key+`"`) {
			t.Errorf("公开响应不应出现字段 %q：%s", key, rawBody)
		}
	}
}

// publicSampleDoctor 是公开医生列表/详情共用的样例数据（照片存对象名，验证补全）。
func publicSampleDoctor() catalog.PublicDoctor {
	return catalog.PublicDoctor{
		ID:          16,
		Name:        "熊佳钰",
		Sex:         "女",
		PhotoURL:    "doctor/16.jpg",
		Degree:      "博士",
		Job:         "主任医师",
		Description: "口腔颌面外科",
		Recommended: true,
	}
}

// publicSampleSchedule 是公开时段样例：full 为 true 时表示 remaining=0 的满号时段。
func publicSampleSchedule(scheduleID int64, slot int16, full bool) schedule.PublicSchedule {
	remaining := int16(2)
	if full {
		remaining = 0
	}
	return schedule.PublicSchedule{
		ScheduleID: scheduleID,
		Date:       "2026-09-20",
		Slot:       slot,
		Maximum:    3,
		Remaining:  remaining,
		Amount:     "80.00",
		Doctor: schedule.PublicScheduleDoctor{
			ID:       16,
			Name:     "熊佳钰",
			Job:      "主任医师",
			Degree:   "博士",
			PhotoURL: "doctor/16.jpg",
		},
		Subdepartment: schedule.PublicScheduleSubdepartment{ID: 2, Name: "口腔颌面外科"},
	}
}

// TestPublicDepartmentsListEnvelope 科室列表 200 envelope：空结果是 items:[]，有数据时字段集合固定。
func TestPublicDepartmentsListEnvelope(t *testing.T) {
	t.Run("空结果输出 items:[]", func(t *testing.T) {
		engine := newPublicCatalogEngine(t, &publicCatalogRepoStub{departments: nil}, &publicScheduleRepoStub{}, publicCatalogMinioURL)
		w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/departments", "", nil)

		items, body := publicPageBody(t, w)
		if len(items) != 0 {
			t.Errorf("items = %v, want 空数组", items)
		}
		if !strings.Contains(w.Body.String(), `"items":[]`) {
			t.Errorf("空结果必须输出 items:[]（不是 null）：%s", w.Body.String())
		}
		if body["page"] != float64(1) || body["pageSize"] != float64(20) || body["total"] != float64(0) {
			t.Errorf("分页字段 = %v/%v/%v, want 1/20/0", body["page"], body["pageSize"], body["total"])
		}
	})

	t.Run("字段集合与管理端字段裁剪", func(t *testing.T) {
		repo := &publicCatalogRepoStub{
			departments:      []catalog.Department{{ID: 1, Name: "口腔科", Outpatient: true, Description: "口腔疾病诊疗", Recommended: true}},
			departmentsTotal: 1,
		}
		engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, publicCatalogMinioURL)
		w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/departments?page=3&pageSize=15", "", nil)

		items, body := publicPageBody(t, w)
		if len(items) != 1 {
			t.Fatalf("items = %v, want 1 条", items)
		}
		assertPublicKeys(t, "科室项", items[0], "id", "name", "outpatient", "description", "recommended")
		if items[0]["name"] != "口腔科" || items[0]["outpatient"] != true || items[0]["recommended"] != true {
			t.Errorf("科室项内容错误：%v", items[0])
		}
		if body["page"] != float64(3) || body["pageSize"] != float64(15) || body["total"] != float64(1) {
			t.Errorf("分页字段 = %v/%v/%v, want 3/15/1", body["page"], body["pageSize"], body["total"])
		}
		// 分页换算：page=3、pageSize=15 → offset=30、limit=15。
		if repo.lastOffset != 30 || repo.lastLimit != 15 {
			t.Errorf("repository 收到 offset=%d limit=%d, want 30/15", repo.lastOffset, repo.lastLimit)
		}
	})
}

// TestPublicSubdepartmentsListEnvelope 子科室列表字段集合与科室编号透传。
func TestPublicSubdepartmentsListEnvelope(t *testing.T) {
	repo := &publicCatalogRepoStub{
		subdepartments:      []catalog.Subdepartment{{ID: 2, Name: "口腔颌面外科", DepartmentID: 1, Location: "1号楼2层A区"}},
		subdepartmentsTotal: 1,
	}
	engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, publicCatalogMinioURL)
	w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/departments/1/subdepartments", "", nil)

	items, _ := publicPageBody(t, w)
	if len(items) != 1 {
		t.Fatalf("items = %v, want 1 条", items)
	}
	assertPublicKeys(t, "子科室项", items[0], "id", "name", "departmentId", "location")
	if repo.lastSubdepartmentDepartmentID != 1 {
		t.Errorf("科室编号透传 = %d, want 1", repo.lastSubdepartmentDepartmentID)
	}

	emptyEngine := newPublicCatalogEngine(t, &publicCatalogRepoStub{subdepartments: nil}, &publicScheduleRepoStub{}, publicCatalogMinioURL)
	emptyWriter := scheduleRequest(emptyEngine, http.MethodGet, "/api/v1/public/departments/1/subdepartments", "", nil)
	if !strings.Contains(emptyWriter.Body.String(), `"items":[]`) {
		t.Errorf("空结果必须输出 items:[]（不是 null）：%s", emptyWriter.Body.String())
	}
}

// TestPublicDoctorsListTrimsFields 医生列表只暴露契约 §2.3 的公开字段，并补全照片地址。
func TestPublicDoctorsListTrimsFields(t *testing.T) {
	repo := &publicCatalogRepoStub{doctors: []catalog.PublicDoctor{publicSampleDoctor()}, doctorsTotal: 1}
	engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, publicCatalogMinioURL)
	w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/doctors", "", nil)

	items, _ := publicPageBody(t, w)
	if len(items) != 1 {
		t.Fatalf("items = %v, want 1 条", items)
	}
	assertPublicKeys(t, "医生项", items[0],
		"id", "name", "sex", "photoUrl", "degree", "job", "description", "recommended")
	assertNoForbiddenKeys(t, w.Body.String(),
		"pid", "tel", "address", "email", "status", "birthday", "school", "remark", "hireDate", "tags", "createDate", "uuid")
	if items[0]["photoUrl"] != "http://minio.example/doctor/16.jpg" {
		t.Errorf("photoUrl = %v, want 补全后的地址", items[0]["photoUrl"])
	}
	if items[0]["recommended"] != true || items[0]["sex"] != "女" {
		t.Errorf("医生项内容错误：%v", items[0])
	}
}

// TestPublicDoctorDetailTrimsFields 医生详情字段集合：内嵌子科室与价目均不含管理端字段（价目无 doctorId）。
func TestPublicDoctorDetailTrimsFields(t *testing.T) {
	detail := &catalog.PublicDoctorDetail{
		PublicDoctor: publicSampleDoctor(),
		Subdepartments: []catalog.PublicSubdepartmentRef{
			{ID: 2, Name: "口腔颌面外科"},
		},
		Prices: []catalog.PublicDoctorPrice{
			{ID: 1, Level: "主任医师", Price1: "80.00", Price2: "200.00"},
		},
	}
	repo := &publicCatalogRepoStub{doctorDetail: detail}
	engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, publicCatalogMinioURL)
	w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/doctors/16", "", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	assertPublicKeys(t, "医生详情", body,
		"id", "name", "sex", "photoUrl", "degree", "job", "description", "recommended", "subdepartments", "prices")
	assertNoForbiddenKeys(t, w.Body.String(),
		"pid", "tel", "address", "email", "status", "birthday", "school", "remark", "hireDate", "tags", "createDate", "uuid", "doctorId")

	subdepartments, ok := body["subdepartments"].([]any)
	if !ok || len(subdepartments) != 1 {
		t.Fatalf("subdepartments = %v, want 长度为 1 的数组", body["subdepartments"])
	}
	assertPublicKeys(t, "详情子科室项", subdepartments[0].(map[string]any), "id", "name")

	prices, ok := body["prices"].([]any)
	if !ok || len(prices) != 1 {
		t.Fatalf("prices = %v, want 长度为 1 的数组", body["prices"])
	}
	assertPublicKeys(t, "详情价目项", prices[0].(map[string]any), "id", "level", "price1", "price2")
}

// TestPublicDoctorDetailEmptyCollections 详情无子科室/价目时输出空数组而不是 null。
func TestPublicDoctorDetailEmptyCollections(t *testing.T) {
	detail := &catalog.PublicDoctorDetail{PublicDoctor: publicSampleDoctor()}
	repo := &publicCatalogRepoStub{doctorDetail: detail}
	engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, publicCatalogMinioURL)
	w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/doctors/16", "", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	if !strings.Contains(raw, `"subdepartments":[]`) || !strings.Contains(raw, `"prices":[]`) {
		t.Errorf("空集合必须输出 []：%s", raw)
	}
}

// TestPublicSchedulesFieldsAndFullSlot 时段列表：字段集合固定、满号时段仍在 items、
// 默认日期窗口（业务当天起 7 天）与分页参数透传、医生照片补全。
func TestPublicSchedulesFieldsAndFullSlot(t *testing.T) {
	repo := &publicScheduleRepoStub{
		items: []schedule.PublicSchedule{
			publicSampleSchedule(12, 1, false),
			publicSampleSchedule(13, 2, true),
		},
		total: 2,
	}
	engine := newPublicCatalogEngine(t, &publicCatalogRepoStub{}, repo, publicCatalogMinioURL)
	w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/schedules?subdepartmentId=2&page=2&pageSize=5", "", nil)

	items, body := publicPageBody(t, w)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2 条（满号时段不能被过滤）", items)
	}
	assertPublicKeys(t, "时段项", items[0],
		"scheduleId", "date", "slot", "maximum", "remaining", "amount", "doctor", "subdepartment")
	assertNoForbiddenKeys(t, w.Body.String(), "planId", "workPlanId", "used", "num")

	if items[1]["remaining"] != float64(0) {
		t.Errorf("满号时段 remaining = %v, want 0 且仍在 items 中", items[1]["remaining"])
	}
	doctor, ok := items[0]["doctor"].(map[string]any)
	if !ok {
		t.Fatalf("doctor = %v, want 对象", items[0]["doctor"])
	}
	assertPublicKeys(t, "时段医生项", doctor, "id", "name", "job", "degree", "photoUrl")
	if doctor["photoUrl"] != "http://minio.example/doctor/16.jpg" {
		t.Errorf("时段医生 photoUrl = %v, want 补全后的地址", doctor["photoUrl"])
	}
	subdepartment, ok := items[0]["subdepartment"].(map[string]any)
	if !ok {
		t.Fatalf("subdepartment = %v, want 对象", items[0]["subdepartment"])
	}
	assertPublicKeys(t, "时段子科室项", subdepartment, "id", "name")

	if body["page"] != float64(2) || body["pageSize"] != float64(5) || body["total"] != float64(2) {
		t.Errorf("分页字段 = %v/%v/%v, want 2/5/2", body["page"], body["pageSize"], body["total"])
	}
	if repo.lastOffset != 5 || repo.lastLimit != 5 {
		t.Errorf("repository 收到 offset=%d limit=%d, want 5/5", repo.lastOffset, repo.lastLimit)
	}
	// 缺省日期窗口：业务当天 2026-09-09 起 7 天（含当天）。
	if repo.lastFilter.FromDate != "2026-09-09" || repo.lastFilter.ToDate != "2026-09-15" {
		t.Errorf("缺省窗口 = %s..%s, want 2026-09-09..2026-09-15", repo.lastFilter.FromDate, repo.lastFilter.ToDate)
	}
	if repo.lastFilter.SubdepartmentID == nil || *repo.lastFilter.SubdepartmentID != 2 {
		t.Errorf("subdepartmentId 透传错误：%v", repo.lastFilter.SubdepartmentID)
	}
}

// TestPublicPhotoURLSemantics 照片地址补全：对象名补 base 前缀，已是 http(s) 或为空时原样返回。
func TestPublicPhotoURLSemantics(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		object string
		want   string
	}{
		{"补全对象名", "http://minio.example", "doctor/16.jpg", "http://minio.example/doctor/16.jpg"},
		{"base 尾斜杠被规范化", "http://minio.example/", "doctor/16.jpg", "http://minio.example/doctor/16.jpg"},
		{"对象名带前导斜杠", "http://minio.example", "/doctor/16.jpg", "http://minio.example/doctor/16.jpg"},
		{"已是 http 地址", "http://minio.example", "http://cdn.example/16.jpg", "http://cdn.example/16.jpg"},
		{"已是 https 地址", "http://minio.example", "https://cdn.example/16.jpg", "https://cdn.example/16.jpg"},
		{"空对象名原样返回", "http://minio.example", "", ""},
		{"空 base 原样返回", "", "doctor/16.jpg", "doctor/16.jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := response.PublicPhotoURL(tc.base, tc.object); got != tc.want {
				t.Errorf("PublicPhotoURL(%q,%q) = %q, want %q", tc.base, tc.object, got, tc.want)
			}
		})
	}

	// handler 侧：无 base（未配置 MinIO）时对象名原样返回。
	repo := &publicCatalogRepoStub{doctors: []catalog.PublicDoctor{publicSampleDoctor()}, doctorsTotal: 1}
	engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, "")
	w := scheduleRequest(engine, http.MethodGet, "/api/v1/public/doctors", "", nil)
	items, _ := publicPageBody(t, w)
	if len(items) != 1 || items[0]["photoUrl"] != "doctor/16.jpg" {
		t.Errorf("无 base 时 photoUrl 应原样返回：%v", items)
	}
}

// TestPublicNotFoundResponses 不存在语义：科室/医生统一 404，子科室接口的科室不存在同样 404。
func TestPublicNotFoundResponses(t *testing.T) {
	cases := []struct {
		name   string
		stub   *publicCatalogRepoStub
		method string
		path   string
		code   string
		msg    string
	}{
		{"科室不存在（sql.ErrNoRows）", &publicCatalogRepoStub{findDepartmentErr: sql.ErrNoRows},
			http.MethodGet, "/api/v1/public/departments/99999", publiccatalogservice.CodeDepartmentNotFound, "科室不存在"},
		{"科室不存在（nil,nil）", &publicCatalogRepoStub{departmentDetail: nil},
			http.MethodGet, "/api/v1/public/departments/99999", publiccatalogservice.CodeDepartmentNotFound, "科室不存在"},
		{"医生不存在（sql.ErrNoRows）", &publicCatalogRepoStub{findDoctorErr: sql.ErrNoRows},
			http.MethodGet, "/api/v1/public/doctors/99999", publiccatalogservice.CodeDoctorNotFound, "医生不存在"},
		{"医生不存在（nil,nil）", &publicCatalogRepoStub{doctorDetail: nil},
			http.MethodGet, "/api/v1/public/doctors/99999", publiccatalogservice.CodeDoctorNotFound, "医生不存在"},
		{"子科室接口科室不存在", &publicCatalogRepoStub{subdepartmentsErr: sql.ErrNoRows},
			http.MethodGet, "/api/v1/public/departments/99999/subdepartments", publiccatalogservice.CodeDepartmentNotFound, "科室不存在"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newPublicCatalogEngine(t, tc.stub, &publicScheduleRepoStub{}, publicCatalogMinioURL)
			w := scheduleRequest(engine, tc.method, tc.path, "", nil)
			body := assertErrorStatus(t, w, http.StatusNotFound, tc.code)
			if body["message"] != tc.msg {
				t.Errorf("message = %v, want %s", body["message"], tc.msg)
			}
		})
	}
}

// TestPublicValidationResponses 参数非法统一 422 REQUEST_VALIDATION_FAILED。
func TestPublicValidationResponses(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		message string
	}{
		{"page 为 0", "/api/v1/public/departments?page=0", "page必须为正整数"},
		{"pageSize 超上限", "/api/v1/public/doctors?pageSize=101", "pageSize必须在1到100之间"},
		{"sort 不在白名单", "/api/v1/public/doctors?sort=password", "排序字段或排序方向不支持"},
		{"order 非法", "/api/v1/public/departments?order=drop", "排序字段或排序方向不支持"},
		{"科室编号非正整数", "/api/v1/public/departments/abc", "科室编号必须为正整数"},
		{"医生编号为 0", "/api/v1/public/doctors/0", "医生编号必须为正整数"},
		{"日期格式非法", "/api/v1/public/schedules?subdepartmentId=2&fromDate=2026-9-1", "fromDate格式必须为YYYY-MM-DD"},
		{"必填过滤缺失", "/api/v1/public/schedules?fromDate=2026-09-20", "必须提供 subdepartmentId 或 doctorId"},
		{"日期跨度超 31 天", "/api/v1/public/schedules?subdepartmentId=2&fromDate=2026-09-01&toDate=2026-10-02", "日期范围无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newPublicCatalogEngine(t, &publicCatalogRepoStub{}, &publicScheduleRepoStub{}, publicCatalogMinioURL)
			w := scheduleRequest(engine, http.MethodGet, tc.path, "", nil)
			body := assertErrorStatus(t, w, http.StatusUnprocessableEntity, publiccatalogservice.CodeValidationFailed)
			if body["message"] != tc.message {
				t.Errorf("message = %v, want %s", body["message"], tc.message)
			}
		})
	}
}

// TestPublicAnonymousRequestsIgnoreTokens 携带垃圾令牌或跨域（管理端）令牌访问 /public/* 时，
// 响应状态与响应体必须与不带令牌完全一致（契约 §1.2、测试策略「匿名公开域」）。
func TestPublicAnonymousRequestsIgnoreTokens(t *testing.T) {
	repo := &publicCatalogRepoStub{doctors: []catalog.PublicDoctor{publicSampleDoctor()}, doctorsTotal: 1}
	engine := newPublicCatalogEngine(t, repo, &publicScheduleRepoStub{}, publicCatalogMinioURL)
	const path = "/api/v1/public/doctors?page=1&pageSize=20"

	baseline := scheduleRequest(engine, http.MethodGet, path, "", nil)
	if baseline.Code != http.StatusOK {
		t.Fatalf("匿名请求 status = %d, want 200; body=%s", baseline.Code, baseline.Body.String())
	}

	headers := []map[string]string{
		{"Authorization": "Bearer garbage-not-a-jwt"},
		{"Authorization": "Bearer eyJhbGciOiJIUzI1NiJ9.mis-realm-token.invalid"},
		{"Authorization": ""},
	}
	for _, header := range headers {
		w := scheduleRequest(engine, http.MethodGet, path, "", header)
		if w.Code != baseline.Code {
			t.Errorf("headers=%v status = %d, want %d", header, w.Code, baseline.Code)
		}
		if w.Body.String() != baseline.Body.String() {
			t.Errorf("headers=%v 响应体与匿名请求不一致：\n带令牌: %s\n不带令牌: %s", header, w.Body.String(), baseline.Body.String())
		}
	}
}

// TestPublicRepositoryErrorReturns500 仓储普通错误统一 500 INTERNAL_SERVER_ERROR，message 固定为「查询失败」。
func TestPublicRepositoryErrorReturns500(t *testing.T) {
	boom := errors.New("db down")
	cases := []struct {
		name         string
		catalogStub  *publicCatalogRepoStub
		scheduleStub *publicScheduleRepoStub
		path         string
	}{
		{"科室列表", &publicCatalogRepoStub{departmentsErr: boom}, &publicScheduleRepoStub{}, "/api/v1/public/departments"},
		{"子科室列表", &publicCatalogRepoStub{subdepartmentsErr: boom}, &publicScheduleRepoStub{}, "/api/v1/public/departments/1/subdepartments"},
		{"医生列表", &publicCatalogRepoStub{doctorsErr: boom}, &publicScheduleRepoStub{}, "/api/v1/public/doctors"},
		{"医生详情", &publicCatalogRepoStub{findDoctorErr: boom}, &publicScheduleRepoStub{}, "/api/v1/public/doctors/16"},
		{"科室详情", &publicCatalogRepoStub{findDepartmentErr: boom}, &publicScheduleRepoStub{}, "/api/v1/public/departments/1"},
		{"时段列表", &publicCatalogRepoStub{}, &publicScheduleRepoStub{err: boom}, "/api/v1/public/schedules?subdepartmentId=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newPublicCatalogEngine(t, tc.catalogStub, tc.scheduleStub, publicCatalogMinioURL)
			w := scheduleRequest(engine, http.MethodGet, tc.path, "", nil)
			body := assertErrorStatus(t, w, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR")
			if body["message"] != "查询失败" {
				t.Errorf("message = %v, want 查询失败", body["message"])
			}
			if strings.Contains(w.Body.String(), "db down") {
				t.Errorf("500 响应不得泄漏内部错误细节：%s", w.Body.String())
			}
		})
	}
}

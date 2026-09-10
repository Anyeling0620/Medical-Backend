package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/catalog"
	"Medical-Web-Backend/internal/domain/schedule"
	"Medical-Web-Backend/internal/transport/http/handler"
	publiccatalogservice "Medical-Web-Backend/internal/usecase/publiccatalog"
)

// 本文件覆盖匿名公开查询域（spec/04-api-contract.md §1.2、§8）在路由层的匿名性：
//
//  1. 生产路由树（NewRouter）中这 6 条 /public/* 路由不挂任何认证/权限中间件，
//     匿名访问绝不返回 401/403，且携带无效令牌或跨域（管理域/患者域）令牌时
//     状态码与响应体与匿名访问完全一致；
//  2. 用假 repository 装配与生产同名的路由模板，让响应是 200 + 真实业务数据，
//     进一步证明 handler 链路完全不读取 Authorization 头（测试策略「匿名公开域」）。
//
// 第 1 点用 newRealmTestRouter 构造的真实路由树验证；该路由树未注入 repository，
// 因此 /public/* 的响应是 500/422（业务层拿不到数据），但足以证明请求没有被认证中间件拦下。

// publicAnonymousRoutePaths 是匿名公开查询域的路由模板，与 router.go 注册的 6 条一一对应。
var publicAnonymousRoutePaths = []string{
	"/api/v1/public/departments",
	"/api/v1/public/departments/:departmentId",
	"/api/v1/public/departments/:departmentId/subdepartments",
	"/api/v1/public/doctors",
	"/api/v1/public/doctors/:doctorId",
	"/api/v1/public/schedules",
}

// publicTokenCases 是匿名性用例覆盖的四种令牌姿态；匿名请求作为逐字节比较的基线。
func publicTokenCases(misToken, patientToken string) []struct {
	name  string
	token string
} {
	return []struct {
		name  string
		token string
	}{
		{"匿名", ""},
		{"无效令牌", "garbage-not-a-jwt"},
		{"管理域令牌", misToken},
		{"患者域令牌", patientToken},
	}
}

// TestRouterPublicRoutesAreAnonymousAndIgnoreTokens 生产路由树上：
// 6 条公开路由匿名可达（非 401/403），且无效令牌/跨域令牌不改变状态码与响应体。
func TestRouterPublicRoutesAreAnonymousAndIgnoreTokens(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)
	_, patientToken := newRealmTestRouter(t, domainauth.RealmPatient)

	for _, route := range publicAnonymousRoutePaths {
		requestPath := resolveRouteParams(route)
		baseline := performRealmRequest(router, http.MethodGet, requestPath, "")
		if baseline.Code == http.StatusUnauthorized || baseline.Code == http.StatusForbidden {
			t.Fatalf("%s 匿名访问被拒绝：status = %d（/public/* 不得挂认证/权限中间件）; body=%s",
				requestPath, baseline.Code, baseline.Body.String())
		}

		for _, tc := range publicTokenCases(misToken, patientToken) {
			w := performRealmRequest(router, http.MethodGet, requestPath, tc.token)
			if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
				t.Errorf("%s %s 访问被拒绝：status = %d（/public/* 不得挂认证/权限中间件）; body=%s",
					tc.name, requestPath, w.Code, w.Body.String())
				continue
			}
			if w.Code != baseline.Code || w.Body.String() != baseline.Body.String() {
				t.Errorf("%s %s 的响应与匿名访问不一致：status %d/%d，body %q/%q",
					tc.name, requestPath, w.Code, baseline.Code, w.Body.String(), baseline.Body.String())
			}
		}
	}
}

// publicRouterCatalogRepoStub 是 port.PublicCatalogRepository 的内存桩：固定返回一条公开数据。
type publicRouterCatalogRepoStub struct{}

// ListPublicDepartments 返回一条公开科室。
func (publicRouterCatalogRepoStub) ListPublicDepartments(_ context.Context, _ catalog.DepartmentFilter, _, _ int) ([]catalog.Department, int64, error) {
	return []catalog.Department{{ID: 1, Name: "口腔科", Outpatient: true, Description: "口腔疾病诊疗", Recommended: true}}, 1, nil
}

// FindPublicDepartment 按传入编号返回科室详情。
func (publicRouterCatalogRepoStub) FindPublicDepartment(_ context.Context, id int64) (*catalog.Department, error) {
	return &catalog.Department{ID: id, Name: "口腔科", Outpatient: true, Description: "口腔疾病诊疗", Recommended: true}, nil
}

// ListPublicSubdepartments 返回一条属于目标科室的子科室。
func (publicRouterCatalogRepoStub) ListPublicSubdepartments(_ context.Context, departmentID int64, _, _ int) ([]catalog.Subdepartment, int64, error) {
	return []catalog.Subdepartment{{ID: 2, Name: "口腔颌面外科", DepartmentID: departmentID, Location: "1号楼2层A区"}}, 1, nil
}

// ListPublicDoctors 返回一条在岗公开医生（照片为对象名，交由 handler 补全）。
func (publicRouterCatalogRepoStub) ListPublicDoctors(_ context.Context, _ catalog.PublicDoctorFilter, _, _ int) ([]catalog.PublicDoctor, int64, error) {
	return []catalog.PublicDoctor{{
		ID: 16, Name: "熊佳钰", Sex: "女", PhotoURL: "doctor/16.jpg",
		Degree: "博士", Job: "主任医师", Description: "口腔颌面外科", Recommended: true,
	}}, 1, nil
}

// FindPublicDoctor 返回带子科室与价目的医生详情。
func (publicRouterCatalogRepoStub) FindPublicDoctor(_ context.Context, id int64) (*catalog.PublicDoctorDetail, error) {
	return &catalog.PublicDoctorDetail{
		PublicDoctor: catalog.PublicDoctor{
			ID: id, Name: "熊佳钰", Sex: "女", PhotoURL: "doctor/16.jpg",
			Degree: "博士", Job: "主任医师", Description: "口腔颌面外科", Recommended: true,
		},
		Subdepartments: []catalog.PublicSubdepartmentRef{{ID: 2, Name: "口腔颌面外科"}},
		Prices:         []catalog.PublicDoctorPrice{{ID: 1, Level: "主任医师", Price1: "80.00", Price2: "200.00"}},
	}, nil
}

// publicRouterScheduleRepoStub 是 port.PublicScheduleRepository 的内存桩。
type publicRouterScheduleRepoStub struct{}

// ListPublicSchedules 返回一条可挂号时段。
func (publicRouterScheduleRepoStub) ListPublicSchedules(_ context.Context, _ schedule.PublicScheduleFilter, _, _ int) ([]schedule.PublicSchedule, int64, error) {
	return []schedule.PublicSchedule{{
		ScheduleID: 12, Date: "2026-09-20", Slot: 1, Maximum: 3, Remaining: 3, Amount: "80.00",
		Doctor:        schedule.PublicScheduleDoctor{ID: 16, Name: "熊佳钰", Job: "主任医师", Degree: "博士", PhotoURL: "doctor/16.jpg"},
		Subdepartment: schedule.PublicScheduleSubdepartment{ID: 2, Name: "口腔颌面外科"},
	}}, 1, nil
}

// newPublicRouterTestEngine 用假 repository 装配与生产同名的 6 条 /public/* 路由，
// 使响应为 200 + 真实业务数据（用于在数据通路上验证「带令牌也不改变结果」）。
func newPublicRouterTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := publiccatalogservice.NewService(
		publicRouterCatalogRepoStub{},
		publicRouterScheduleRepoStub{},
		func() time.Time { return time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC) },
	)
	publicHandler := handler.NewPublicCatalogHandler(service, "http://minio.example/")

	engine := gin.New()
	engine.GET("/api/v1/public/departments", publicHandler.ListDepartments)
	engine.GET("/api/v1/public/departments/:departmentId", publicHandler.DepartmentDetail)
	engine.GET("/api/v1/public/departments/:departmentId/subdepartments", publicHandler.Subdepartments)
	engine.GET("/api/v1/public/doctors", publicHandler.Doctors)
	engine.GET("/api/v1/public/doctors/:doctorId", publicHandler.DoctorDetail)
	engine.GET("/api/v1/public/schedules", publicHandler.Schedules)
	return engine
}

// publicAnonymousRequest 发起一次 GET 请求；token 非空时附加 Authorization: Bearer。
func publicAnonymousRequest(engine *gin.Engine, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// TestRouterPublicRoutesReturnSameDataWithAnyToken 6 条公开路由在匿名与三种令牌姿态下
// 均返回 200，且响应体逐字节一致（handler 不读令牌）。
func TestRouterPublicRoutesReturnSameDataWithAnyToken(t *testing.T) {
	_, misToken := newRealmTestRouter(t, domainauth.RealmMis)
	_, patientToken := newRealmTestRouter(t, domainauth.RealmPatient)
	engine := newPublicRouterTestEngine(t)

	targets := []string{
		"/api/v1/public/departments",
		"/api/v1/public/departments/1",
		"/api/v1/public/departments/1/subdepartments",
		"/api/v1/public/doctors",
		"/api/v1/public/doctors/16",
		"/api/v1/public/schedules?subdepartmentId=2",
	}
	for _, target := range targets {
		baseline := publicAnonymousRequest(engine, target, "")
		if baseline.Code != http.StatusOK {
			t.Fatalf("%s 匿名访问 status = %d, want 200; body=%s", target, baseline.Code, baseline.Body.String())
		}

		for _, tc := range publicTokenCases(misToken, patientToken) {
			w := publicAnonymousRequest(engine, target, tc.token)
			if w.Code != baseline.Code || w.Body.String() != baseline.Body.String() {
				t.Errorf("%s %s 的响应与匿名访问不一致：status %d/%d，body %q/%q",
					tc.name, target, w.Code, baseline.Code, w.Body.String(), baseline.Body.String())
			}
		}
	}
}

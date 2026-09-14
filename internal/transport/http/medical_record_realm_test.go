// 病历域路由守卫单测：/api/v1/medical-records/* 只接受 realm=mis 的令牌，
// 且必须携带 MEDICAL_RECORD:* 权限码（ROOT 不再旁路，授权语义为严格医生绑定），
// 用法与 router_realm_test.go、patient_card_realm_test.go 保持一致。
package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"Medical-Web-Backend/internal/config"
	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainmedicalrecord "Medical-Web-Backend/internal/domain/medical_record"
	domainuser "Medical-Web-Backend/internal/domain/user"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// medicalRecordGuardRoutes 是病历域的五个路由（路径参数用占位值）。
var medicalRecordGuardRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/medical-records"},
	{http.MethodGet, "/api/v1/medical-records/1001"},
	{http.MethodPost, "/api/v1/medical-records"},
	{http.MethodPatch, "/api/v1/medical-records/1001"},
	{http.MethodDelete, "/api/v1/medical-records/1001"},
}

// TestRouterExposesMedicalRecordRoutes 断言病历域五个路由都已挂载。
func TestRouterExposesMedicalRecordRoutes(t *testing.T) {
	router := newContractTestRouter(t)
	routes, _ := collectRoutes(router)

	// 路由表里路径参数是注册时的模式（:medicalRecordId），不是具体主键。
	declared := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/medical-records"},
		{http.MethodGet, "/api/v1/medical-records/:medicalRecordId"},
		{http.MethodPost, "/api/v1/medical-records"},
		{http.MethodPatch, "/api/v1/medical-records/:medicalRecordId"},
		{http.MethodDelete, "/api/v1/medical-records/:medicalRecordId"},
	}
	for _, route := range declared {
		if !routes[routeKey(route.method, route.path)] {
			t.Errorf("缺少路由 %s %s", route.method, route.path)
		}
	}
}

// TestRouterMedicalRecordRoutesRejectPatientRealmToken 病历接口只接受 realm=mis 的令牌：
// 携带 realm=patient 的合法令牌必须返回 401 AUTH_INVALID_TOKEN（而非 403，
// 否则说明令牌通过了 realm 校验、被错误地当成管理端主体解释）。
func TestRouterMedicalRecordRoutesRejectPatientRealmToken(t *testing.T) {
	router, patientToken := newRealmTestRouter(t, domainauth.RealmPatient)

	for _, route := range medicalRecordGuardRoutes {
		w := performRealmRequest(router, route.method, route.path, patientToken)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401; body=%s",
				route.method, route.path, w.Code, w.Body.String())
			continue
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf("%s %s code = %v, want AUTH_INVALID_TOKEN", route.method, route.path, body["code"])
		}
		if body["message"] != "访问令牌无效或已过期" {
			t.Errorf("%s %s message = %v, want 访问令牌无效或已过期",
				route.method, route.path, body["message"])
		}
	}
}

// TestRouterMedicalRecordRoutesRequirePermissions 管理端令牌缺少 MEDICAL_RECORD:* 权限时
// 必须返回 403 AUTH_FORBIDDEN（realm 校验已通过，因此不得是 401，也不得落到 handler）。
func TestRouterMedicalRecordRoutesRequirePermissions(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	for _, route := range medicalRecordGuardRoutes {
		w := performRealmRequest(router, route.method, route.path, misToken)

		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403; body=%s",
				route.method, route.path, w.Code, w.Body.String())
			continue
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "AUTH_FORBIDDEN" {
			t.Errorf("%s %s code = %v, want AUTH_FORBIDDEN", route.method, route.path, body["code"])
		}
		if body["message"] != "没有访问权限" {
			t.Errorf("%s %s message = %v, want 没有访问权限", route.method, route.path, body["message"])
		}
	}
}

// medicalRecordPermissionUserRepo 是只授予指定权限码的 UserRepository 桩。
type medicalRecordPermissionUserRepo struct {
	permissions []string
}

func (r medicalRecordPermissionUserRepo) FindByUsername(_ context.Context, _ string) (*domainuser.User, error) {
	return nil, nil
}

func (r medicalRecordPermissionUserRepo) FindByID(_ context.Context, _ int64) (*domainuser.User, error) {
	return nil, nil
}

func (r medicalRecordPermissionUserRepo) Permissions(_ context.Context, _ int64) ([]string, error) {
	return r.permissions, nil
}

// newMedicalRecordPermissionRouter 构造带 JWT 密钥与指定权限码的管理端令牌路由树；
// 病历仓储按用例传入（传 nil 便于用「是否到达 handler」区分权限拦截与业务执行）。
func newMedicalRecordPermissionRouter(
	t *testing.T,
	permissions []string,
	repository port.MedicalRecordRepository,
) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{Auth: config.AuthConfig{JWTSecret: contractTestJWTSecret}},
		medicalRecordPermissionUserRepo{permissions: permissions},
		contractTokenRepo{},
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // publicScheduleRepository
		nil, // scheduleCacheVersioner
		nil, // idempotencyStore
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		nil, // paymentRepository
		nil, // alipayGateway
		repository,
		nil, // doctorPatientRepository
	)

	claims := &userservice.AccessClaims{
		UserID:    7,
		Username:  "medical-record-doctor",
		TokenType: userservice.TokenTypeAccess,
		Realm:     domainauth.RealmMis,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "medical-record-router-jti",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(contractTestJWTSecret))
	if err != nil {
		t.Fatalf("签发管理端测试 access token 失败：%v", err)
	}
	return router, signed
}

// TestRouterMedicalRecordRouteRejectsRootOnlyToken ROOT 不再是病历接口的旁路权限：
// 只持有 ROOT 的管理端令牌访问病历列表必须 403（授权改为严格医生绑定）。
func TestRouterMedicalRecordRouteRejectsRootOnlyToken(t *testing.T) {
	router, token := newMedicalRecordPermissionRouter(t, []string{"ROOT"}, nil)

	w := performRealmRequest(router, http.MethodGet, "/api/v1/medical-records", token)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403（ROOT 不得旁路 MEDICAL_RECORD 权限）; body=%s",
			w.Code, w.Body.String())
	}
	if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_FORBIDDEN" {
		t.Errorf("code = %v, want AUTH_FORBIDDEN", code)
	}
}

// TestRouterMedicalRecordRoutePassesWithPermission 持有 MEDICAL_RECORD:SELECT 的令牌必须
// 通过 realm 与权限两道守卫并真正到达 handler：仓储缺失时返回 502，
// 而不是 401/403（也证明不需要 ROOT 权限）。
func TestRouterMedicalRecordRoutePassesWithPermission(t *testing.T) {
	router, token := newMedicalRecordPermissionRouter(t, []string{"MEDICAL_RECORD:SELECT"}, nil)

	w := performRealmRequest(router, http.MethodGet, "/api/v1/medical-records", token)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502（已到达 handler，仓储为 nil）; body=%s",
			w.Code, w.Body.String())
	}
	if code := decodeRealmRouterBody(t, w)["code"]; code != "DEPENDENCY_UNAVAILABLE" {
		t.Errorf("code = %v, want DEPENDENCY_UNAVAILABLE", code)
	}
}

// medicalRecordEmptyListRepo 是只返回空列表的病历仓储桩：
// FindDoctorIDByUserID 返回真实存在的医生编号，ListMedicalRecords 返回空切片与非 nil 切片。
type medicalRecordEmptyListRepo struct {
	port.MedicalRecordRepository

	doctorID int64
	filter   domainmedicalrecord.Filter
}

func (r *medicalRecordEmptyListRepo) FindDoctorIDByUserID(_ context.Context, _ int64) (int64, error) {
	return r.doctorID, nil
}

func (r *medicalRecordEmptyListRepo) ListMedicalRecords(
	_ context.Context,
	filter domainmedicalrecord.Filter,
	_, _ int,
) ([]domainmedicalrecord.MedicalRecord, int64, error) {
	r.filter = filter
	return []domainmedicalrecord.MedicalRecord{}, 0, nil
}

// TestRouterMedicalRecordListReachesHandlerWithPermission 带 MEDICAL_RECORD:SELECT 的管理端令牌
// 必须通过 realm 与权限两道守卫并真正落到 handler：返回 200 且 items 为空数组，
// 同时把令牌主体解析出的医生编号作为归属条件下发给仓储（不需要 ROOT 权限）。
func TestRouterMedicalRecordListReachesHandlerWithPermission(t *testing.T) {
	repo := &medicalRecordEmptyListRepo{doctorID: 16}
	router, token := newMedicalRecordPermissionRouter(t, []string{"MEDICAL_RECORD:SELECT"}, repo)

	w := performRealmRequest(router, http.MethodGet, "/api/v1/medical-records", token)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeRealmRouterBody(t, w)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("items = %v, want 空数组", body["items"])
	}
	if body["total"] != float64(0) || body["page"] != float64(1) || body["pageSize"] != float64(20) {
		t.Errorf("分页字段 = %v/%v/%v, want 0/1/20", body["total"], body["page"], body["pageSize"])
	}
	if repo.filter.OwnerDoctorID != 16 {
		t.Errorf("归属过滤 = %d, want 16（来自令牌主体解析的医生编号）", repo.filter.OwnerDoctorID)
	}
}

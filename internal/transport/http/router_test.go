package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

const contractTestJWTSecret = "router-contract-test-secret"

// contractTokenRepo 是仅供契约测试使用的 TokenRepository 桩：让携带合法
// access token 的请求能通过 RequireAccessToken 的撤销检查，其余方法保持 nil。
type contractTokenRepo struct {
	port.TokenRepository
}

func (contractTokenRepo) IsAccessTokenRevoked(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// newContractTestRouter 以 nil 仓库与零值配置构造完整路由树，
// 仅用于契约断言（不发起真实数据库/Redis 请求）。
func newContractTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{},
		nil, // userRepository
		nil, // tokenRepository
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
	)
}

// newContractTestRouterWithAccessToken 额外配置 JWT 密钥与 TokenRepository 桩，
// 并签发一个合法的 access token，用于验证“带令牌时旧路径确实返回 404”。
func newContractTestRouterWithAccessToken(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{Auth: config.AuthConfig{JWTSecret: contractTestJWTSecret}},
		nil, // userRepository
		contractTokenRepo{},
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
	)

	claims := &userservice.AccessClaims{
		TokenType: userservice.TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "contract-test-jti",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(contractTestJWTSecret))
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	return router, signed
}

// routeKey 生成 "METHOD /path" 便于做路由表集合断言。
func routeKey(method, path string) string {
	return method + " " + path
}

// collectRoutes 把路由表转成 "METHOD /path" 与纯 /path 两个集合。
func collectRoutes(router *gin.Engine) (map[string]bool, map[string]bool) {
	routes := make(map[string]bool)
	paths := make(map[string]bool)
	for _, r := range router.Routes() {
		routes[routeKey(r.Method, r.Path)] = true
		paths[r.Path] = true
	}
	return routes, paths
}

// TestRouterExposesContractRoutes 断言规范要求的认证与目录路由都存在，
// 认证统一挂载到 /api/v1/mis/auth，目录挂载到 /api/v1/catalog。
func TestRouterExposesContractRoutes(t *testing.T) {
	router := newContractTestRouter(t)
	routes, _ := collectRoutes(router)

	required := []string{
		// 认证接口（规范第 3 章）
		"POST /api/v1/mis/auth/login",
		"POST /api/v1/mis/auth/refresh",
		"POST /api/v1/mis/auth/logout",
		// 目录接口（规范第 4 章）
		"GET /api/v1/catalog/departments",
		"GET /api/v1/catalog/departments/:departmentId",
		"GET /api/v1/catalog/departments/:departmentId/subdepartments",
		"GET /api/v1/catalog/subdepartments/:subdepartmentId",
		"GET /api/v1/catalog/doctors",
		"GET /api/v1/catalog/doctors/options",
		"GET /api/v1/catalog/doctors/:doctorId",
		"GET /api/v1/catalog/doctor-prices",
		// 排班时段接口（规范 5.5 节）
		"GET /api/v1/schedule/plans/:planId/slots",
		"POST /api/v1/schedule/plans/:planId/slots",
		"PATCH /api/v1/schedule/slots/:slotId",
		"DELETE /api/v1/schedule/slots/:slotId",
	}

	for _, want := range required {
		if !routes[want] {
			t.Errorf("缺少契约路由：%s", want)
		}
	}
}

// TestRouterDoesNotExposeLegacyRoutes 断言本次接口对齐已删除的旧路径
// 不再出现在路由表中（按方法与按纯路径双重校验）。
func TestRouterDoesNotExposeLegacyRoutes(t *testing.T) {
	router := newContractTestRouter(t)
	routes, paths := collectRoutes(router)

	forbidden := []string{
		// 旧认证路径：没有 /api/v1/mis/auth 前缀
		"POST /login",
		"POST /refresh",
		"GET /logout",
		// 已删除的重复旧接口
		"GET /doctor/search",
		"GET /doctor/searchCount",
		"GET /depts",
		"GET /degrees",
		"GET /jobs",
		"GET /doctor/:id",
	}
	for _, bad := range forbidden {
		if routes[bad] {
			t.Errorf("旧路由不应存在：%s", bad)
		}
	}

	legacyPaths := []string{
		"/login", "/refresh", "/logout",
		"/doctor/search", "/doctor/searchCount",
		"/depts", "/degrees", "/jobs", "/doctor/:id",
	}
	for _, path := range legacyPaths {
		if paths[path] {
			t.Errorf("旧路径不应存在：%s", path)
		}
	}
}

// TestRouterLegacyPathsReturnNotFound 在携带合法 access token 的前提下访问
// 已删除的旧接口，应落入 gin 的 no-route 处理并返回 404。
// 注意：gin 引擎级 RequireAccessToken 也会先执行于未匹配路径，
// 因此必须带 token，匿名请求会先得到 401。
func TestRouterLegacyPathsReturnNotFound(t *testing.T) {
	router, accessToken := newContractTestRouterWithAccessToken(t)

	for _, path := range []string{
		"/doctor/search",
		"/doctor/searchCount",
		"/depts",
		"/degrees",
		"/jobs",
		"/doctor/1",
		"/login",
		"/refresh",
		"/logout",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404; body=%s", path, w.Code, w.Body.String())
		}
	}
}

// TestRouterCatalogRequiresAccessToken 断言新目录路由仍受访问令牌保护：
// 未携带令牌访问应返回 401 AUTH_INVALID_TOKEN，而不是落入 handler。
func TestRouterCatalogRequiresAccessToken(t *testing.T) {
	router := newContractTestRouter(t)

	for _, path := range []string{
		"/api/v1/catalog/departments",
		"/api/v1/catalog/doctors",
		"/api/v1/catalog/doctors/options",
		"/api/v1/catalog/doctor-prices",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401; body=%s", path, w.Code, w.Body.String())
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("GET %s decode body: %v", path, err)
			continue
		}
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf("GET %s code = %v, want AUTH_INVALID_TOKEN", path, body["code"])
		}
	}
}

// TestRouterAuthLogoutReachableWithoutRepository 断言认证路由注册在
// RequireAccessToken 之前：即便仓库全为 nil，logout 也能被 handler 处理。
// 通过区分错误文案可证明 401 来自 handler（“访问令牌无效”）而非中间件
// （“访问令牌无效或已过期”）。
func TestRouterAuthLogoutReachableWithoutRepository(t *testing.T) {
	router := newContractTestRouter(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mis/auth/logout", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/v1/mis/auth/logout status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["code"] != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", body["code"])
	}
	if body["message"] != "访问令牌无效" {
		t.Errorf("message = %v, want 访问令牌无效（证明已到达 handler，未被中间件拦截）", body["message"])
	}
}

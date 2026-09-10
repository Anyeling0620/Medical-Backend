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
	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainuser "Medical-Web-Backend/internal/domain/user"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// realmRouterUserRepo 是 UserRepository 桩：固定返回空权限。
// 这样“令牌被 realm 校验拦截”会得到 401，而“令牌被放行进入 RequirePermissions”
// 必然得到 403，从而能用状态码区分 realm 隔离是否生效。
type realmRouterUserRepo struct{}

func (realmRouterUserRepo) FindByUsername(
	_ context.Context,
	_ string,
) (*domainuser.User, error) {
	return nil, nil
}

func (realmRouterUserRepo) FindByID(
	_ context.Context,
	_ int64,
) (*domainuser.User, error) {
	return nil, nil
}

func (realmRouterUserRepo) Permissions(
	_ context.Context,
	_ int64,
) ([]string, error) {
	return nil, nil
}

// newRealmTestRouter 构造带 JWT 密钥、令牌桩与空权限用户仓库的路由树，
// 并签发指定 realm 的合法 access token（复用同一签名密钥，仅 realm 不同）。
func newRealmTestRouter(t *testing.T, realm domainauth.Realm) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{Auth: config.AuthConfig{JWTSecret: contractTestJWTSecret}},
		realmRouterUserRepo{},
		contractTokenRepo{},
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
	)

	claims := &userservice.AccessClaims{
		UserID:    7,
		Username:  "mis-admin",
		TokenType: userservice.TokenTypeAccess,
		Realm:     realm,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "realm-contract-jti-" + string(realm),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(contractTestJWTSecret))
	if err != nil {
		t.Fatalf("签发测试 access token 失败：%v", err)
	}
	return router, signed
}

// performRealmRequest 发起请求；token 非空时以 Bearer 方式附加。
func performRealmRequest(
	router *gin.Engine,
	method string,
	path string,
	token string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// decodeRealmRouterBody 解析响应体中的错误码字段。
func decodeRealmRouterBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败：%v body=%s", err, w.Body.String())
	}
	return body
}

// misRealmRoutes 是本次改造挂载了 requireMisAccess 的两组路由的代表路径。
var misRealmRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/catalog/departments"},
	{http.MethodGet, "/api/v1/catalog/doctors"},
	{http.MethodGet, "/api/v1/schedule/plans"},
	{http.MethodPost, "/api/v1/schedule/plans"},
	{http.MethodPatch, "/api/v1/schedule/plans/1"},
}

// TestRouterMisRoutesRejectPatientRealmToken 断言携带 realm=patient 的合法令牌访问
// 管理端路由时返回 401 AUTH_INVALID_TOKEN；若返回 403 说明令牌通过了 realm 校验、
// 进入了权限中间件，即隔离失效。
func TestRouterMisRoutesRejectPatientRealmToken(t *testing.T) {
	router, patientToken := newRealmTestRouter(t, domainauth.RealmPatient)

	for _, route := range misRealmRoutes {
		w := performRealmRequest(router, route.method, route.path, patientToken)

		if w.Code == http.StatusForbidden {
			t.Errorf(
				"%s %s 返回 403：患者域令牌不得进入权限中间件，body=%s",
				route.method,
				route.path,
				w.Body.String(),
			)
			continue
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf(
				"%s %s status = %d, want 401; body=%s",
				route.method,
				route.path,
				w.Code,
				w.Body.String(),
			)
			continue
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
			t.Errorf(
				"%s %s code = %v, want AUTH_INVALID_TOKEN",
				route.method,
				route.path,
				code,
			)
		}
	}
}

// TestRouterMisRoutesAcceptMisRealmToken 断言 realm=mis 的合法令牌能通过 realm 校验
// 并进入后续 RequirePermissions：桩仓库返回空权限，因此结果应为 403 AUTH_FORBIDDEN；
// 若被 realm 校验拦截则会得到 401。
func TestRouterMisRoutesAcceptMisRealmToken(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	for _, path := range []string{
		"/api/v1/catalog/departments",
		"/api/v1/schedule/plans",
	} {
		w := performRealmRequest(router, http.MethodGet, path, misToken)

		if w.Code != http.StatusForbidden {
			t.Fatalf(
				"GET %s status = %d, want 403（证明已进入权限中间件）；body=%s",
				path,
				w.Code,
				w.Body.String(),
			)
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_FORBIDDEN" {
			t.Errorf("GET %s code = %v, want AUTH_FORBIDDEN", path, code)
		}
	}
}

// TestRouterMisRoutesRejectAnonymousRequests 回归：未携带令牌访问管理端路由
// 仍是 401 AUTH_INVALID_TOKEN，而不是 403 或落到 handler。
func TestRouterMisRoutesRejectAnonymousRequests(t *testing.T) {
	router, _ := newRealmTestRouter(t, domainauth.RealmMis)

	for _, route := range misRealmRoutes {
		w := performRealmRequest(router, route.method, route.path, "")

		if w.Code != http.StatusUnauthorized {
			t.Errorf(
				"%s %s status = %d, want 401; body=%s",
				route.method,
				route.path,
				w.Code,
				w.Body.String(),
			)
			continue
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
			t.Errorf(
				"%s %s code = %v, want AUTH_INVALID_TOKEN",
				route.method,
				route.path,
				code,
			)
		}
	}
}

// TestRouterDoesNotGuardPatientPathsWithMisRealm 断言访问令牌校验已从引擎级中间件
// 改为按认证域挂在路由组上：未注册的 /api/v1/patient/* 不会被管理域校验拦成 401，
// 而是走 gin 的 no-route 404，从而为后续患者域路由（realm=patient）预留空间。
func TestRouterDoesNotGuardPatientPathsWithMisRealm(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/patient/me"},
		{http.MethodPost, "/api/v1/patient/cards"},
	} {
		for _, token := range []string{"", misToken} {
			w := performRealmRequest(router, route.method, route.path, token)

			if w.Code != http.StatusNotFound {
				t.Errorf(
					"%s %s（携带令牌=%t）status = %d, want 404; body=%s",
					route.method,
					route.path,
					token != "",
					w.Code,
					w.Body.String(),
				)
			}
		}
	}
}

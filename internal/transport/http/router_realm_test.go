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
		nil, // publicCatalogRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		nil, // paymentRepository
		nil, // alipayGateway
		nil, // medicalRecordRepository
		nil, // doctorPatientRepository
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

// TestRouterPatientMeIsGuardedByPatientRealm 断言 /api/v1/patient/me 已挂载
// realm=patient 的令牌校验：匿名与 realm=mis 的合法令牌都必须返回
// 401 AUTH_INVALID_TOKEN，且响应文案按患者域给出（证明请求到达患者域中间件，
// 而不是落入 no-route 404，也不是被管理域中间件处理）。
func TestRouterPatientMeIsGuardedByPatientRealm(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	for _, token := range []string{"", misToken} {
		w := performRealmRequest(
			router,
			http.MethodGet,
			"/api/v1/patient/me",
			token,
		)

		if w.Code != http.StatusUnauthorized {
			t.Errorf(
				"GET /api/v1/patient/me（携带令牌=%t）status = %d, want 401; body=%s",
				token != "",
				w.Code,
				w.Body.String(),
			)
			continue
		}
		body := decodeRealmRouterBody(t, w)
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf(
				"GET /api/v1/patient/me（携带令牌=%t）code = %v, want AUTH_INVALID_TOKEN",
				token != "",
				body["code"],
			)
		}
		if body["message"] != "患者访问令牌无效" {
			t.Errorf(
				"GET /api/v1/patient/me（携带令牌=%t）message = %v, want 患者访问令牌无效",
				token != "",
				body["message"],
			)
		}
	}
}

// TestRouterUnregisteredPatientPathsStayNotFound 断言 realm 校验只作用于已注册路由：
// 尚未注册的患者域路径仍由 gin 返回 404，而不是被患者域中间件拦成 401。
// 这里刻意使用一个永远不会被注册的路径：若改用「尚未实现的业务接口」作为哨兵
// （例如先前的就诊卡接口），一旦该接口落地实现，本用例就会失败。
func TestRouterUnregisteredPatientPathsStayNotFound(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	for _, token := range []string{"", misToken} {
		w := performRealmRequest(
			router,
			http.MethodGet,
			"/api/v1/patient/not-registered-path",
			token,
		)

		if w.Code != http.StatusNotFound {
			t.Errorf(
				"GET /api/v1/patient/not-registered-path（携带令牌=%t）status = %d, want 404; body=%s",
				token != "",
				w.Code,
				w.Body.String(),
			)
		}
	}
}

// TestRouterPatientAuthRejectsMisRealmAccessToken 断言患者端认证接口使用严格语义：
// 用管理端 realm 的令牌调用 logout 必须返回 401 AUTH_INVALID_TOKEN，
// 不得把管理端令牌当患者主体解释（spec/04-api-contract.md §12.5）。
func TestRouterPatientAuthRejectsMisRealmAccessToken(t *testing.T) {
	router, misToken := newRealmTestRouter(t, domainauth.RealmMis)

	w := performRealmRequest(
		router,
		http.MethodPost,
		"/api/v1/patient/auth/logout",
		misToken,
	)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf(
			"POST /api/v1/patient/auth/logout status = %d, want 401; body=%s",
			w.Code,
			w.Body.String(),
		)
	}
	if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
	}
}

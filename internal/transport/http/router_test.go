package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"Medical-Web-Backend/internal/config"
	domainauth "Medical-Web-Backend/internal/domain/auth"
	domainpayment "Medical-Web-Backend/internal/domain/payment"
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

// DeleteRefreshSessionBySessionID 让契约测试桩满足 TokenRepository 的全部方法：
// 本桩不保存会话，因此按契约返回 (false, nil)，避免内嵌 nil 接口时调用即 panic。
func (contractTokenRepo) DeleteRefreshSessionBySessionID(
	_ context.Context,
	_ string,
) (bool, error) {
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
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		nil, // paymentRepository
		nil, // alipayGateway
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
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		nil, // paymentRepository
		nil, // alipayGateway
	)

	claims := &userservice.AccessClaims{
		TokenType: userservice.TokenTypeAccess,
		Realm:     domainauth.RealmMis,
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
		// 排班计划接口（规范第 5 章）
		"GET /api/v1/schedule/plans",
		"POST /api/v1/schedule/plans",
		"PATCH /api/v1/schedule/plans/:planId",
		"DELETE /api/v1/schedule/plans/:planId",
		// 排班时段接口（规范 5.5 节）
		"GET /api/v1/schedule/plans/:planId/slots",
		"POST /api/v1/schedule/plans/:planId/slots",
		"PATCH /api/v1/schedule/slots/:slotId",
		"DELETE /api/v1/schedule/slots/:slotId",
		// 支付域接口（规范第 6 章）
		"POST /api/v1/payments",
		"POST /api/v1/payments/orders",
		"GET /api/v1/payments/:outTradeNo",
		"POST /api/v1/payments/alipay/notify",
		// 患者端认证与当前患者（规范第 7 章）
		"POST /api/v1/patient/auth/wechat-login",
		"POST /api/v1/patient/auth/refresh",
		"POST /api/v1/patient/auth/logout",
		"GET /api/v1/patient/me",
		// 患者端就诊卡接口（规范 7.4 节与 12.5 节）
		"GET /api/v1/patient/cards",
		"POST /api/v1/patient/cards",
		"GET /api/v1/patient/cards/:cardId",
		"PATCH /api/v1/patient/cards/:cardId",
		// 公开查询域（规范第 8 章）
		"GET /api/v1/public/departments",
		"GET /api/v1/public/departments/:departmentId",
		"GET /api/v1/public/departments/:departmentId/subdepartments",
		"GET /api/v1/public/doctors",
		"GET /api/v1/public/doctors/:doctorId",
		"GET /api/v1/public/schedules",
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
// 注意：访问令牌校验已改为按认证域挂在具体路由组上，未匹配路径不再经过该中间件。
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

// TestRouterScheduleRequiresAccessToken 断言排班计划路由仍受访问令牌保护：
// 未携带令牌访问返回 401 AUTH_INVALID_TOKEN，而不是落入 handler。
func TestRouterScheduleRequiresAccessToken(t *testing.T) {
	router := newContractTestRouter(t)

	for _, method := range []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPatch,
		http.MethodDelete,
	} {
		path := "/api/v1/schedule/plans"
		if method == http.MethodPatch || method == http.MethodDelete {
			path += "/1"
		}
		req := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401; body=%s", method, path, w.Code, w.Body.String())
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s decode body: %v", method, path, err)
			continue
		}
		if body["code"] != "AUTH_INVALID_TOKEN" {
			t.Errorf("%s %s code = %v, want AUTH_INVALID_TOKEN", method, path, body["code"])
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

// ---- POST /api/v1/payments/orders 权限矩阵 ----

// paymentOrdersTestUserRepo 让管理端主体拥有 ROOT 权限码，用于验证 ROOT 可直接创建支付订单；
// 其余查询沿用空实现的 realmRouterUserRepo。
type paymentOrdersTestUserRepo struct{ realmRouterUserRepo }

func (paymentOrdersTestUserRepo) Permissions(_ context.Context, _ int64) ([]string, error) {
	return []string{"ROOT"}, nil
}

// paymentOrdersTestRepo 是 port.PaymentRepository 的最小桩：返回一条支付窗口完整、
// 但尚未预下单的 UNPAID 订单，并记录归属开关，供权限矩阵用例断言管理端不限定归属。
type paymentOrdersTestRepo struct {
	item       *domainpayment.Payment
	regQueries []int64
}

func (r *paymentOrdersTestRepo) FindPaymentByRegistrationID(
	_ context.Context,
	_ int64,
	ownerPatientID int64,
) (*domainpayment.Payment, error) {
	r.regQueries = append(r.regQueries, ownerPatientID)
	return r.itemOrNotFound()
}

func (r *paymentOrdersTestRepo) FindPaymentByOutTradeNo(
	_ context.Context,
	_ string,
	_ int64,
) (*domainpayment.Payment, error) {
	return r.itemOrNotFound()
}

func (r *paymentOrdersTestRepo) EnsurePaymentWindow(_ context.Context, _ string) (*domainpayment.Payment, error) {
	return r.itemOrNotFound()
}

func (r *paymentOrdersTestRepo) SavePrepayID(_ context.Context, _ string, prepayID string) (bool, error) {
	if r.item != nil {
		r.item.PrepayID = prepayID
	}
	return true, nil
}

func (r *paymentOrdersTestRepo) MarkPaid(_ context.Context, _ string, _ string) (bool, error) {
	return true, nil
}

// itemOrNotFound 返回订单副本；未注入订单时按不存在处理，避免用例误判。
func (r *paymentOrdersTestRepo) itemOrNotFound() (*domainpayment.Payment, error) {
	if r.item == nil {
		return nil, domainpayment.ErrPaymentNotFound
	}
	copied := *r.item
	return &copied, nil
}

// paymentOrdersTestGateway 是 port.AlipayGateway 的最小桩：预下单固定返回一个二维码。
type paymentOrdersTestGateway struct {
	precreateCalls []domainpayment.PrecreateRequest
}

func (g *paymentOrdersTestGateway) Precreate(
	_ context.Context,
	req domainpayment.PrecreateRequest,
) (*domainpayment.PrecreateResult, error) {
	g.precreateCalls = append(g.precreateCalls, req)
	return &domainpayment.PrecreateResult{QRCode: "https://qr.alipay.com/bax-router-test"}, nil
}

func (g *paymentOrdersTestGateway) QueryTrade(_ context.Context, _ string) (*domainpayment.TradeQueryResult, error) {
	return &domainpayment.TradeQueryResult{}, nil
}

func (g *paymentOrdersTestGateway) VerifyNotify(_ context.Context, _ url.Values) (*domainpayment.NotifyPayload, error) {
	return nil, port.ErrNotifySignatureInvalid
}

// 编译期确认两个桩完整实现端口契约。
var (
	_ port.PaymentRepository = (*paymentOrdersTestRepo)(nil)
	_ port.AlipayGateway     = (*paymentOrdersTestGateway)(nil)
)

// signPaymentOrdersAccessToken 用契约测试密钥签发指定 realm 的合法 access token。
func signPaymentOrdersAccessToken(t *testing.T, realm domainauth.Realm) string {
	t.Helper()
	claims := &userservice.AccessClaims{
		UserID:    7,
		Username:  "mis-admin",
		TokenType: userservice.TokenTypeAccess,
		Realm:     realm,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "payment-orders-jti-" + string(realm),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(contractTestJWTSecret))
	if err != nil {
		t.Fatalf("签发支付订单接口测试 access token 失败：%v", err)
	}
	return signed
}

// newPaymentOrdersTestRouter 构造带 ROOT 权限用户仓库与支付桩的真实路由树，
// 用于验证 paymentAdminRoutes 的 realm 与权限期望。
func newPaymentOrdersTestRouter(
	t *testing.T,
	repo port.PaymentRepository,
	gateway port.AlipayGateway,
) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(
		func() map[string]any { return map[string]any{"status": "ok"} },
		config.Config{Auth: config.AuthConfig{JWTSecret: contractTestJWTSecret}},
		paymentOrdersTestUserRepo{},
		contractTokenRepo{},
		nil, // doctorRepository
		nil, // scheduleRepository
		nil, // idempotencyStore
		nil, // patientRepository
		nil, // wechatAuthenticator
		nil, // registrationRepository
		repo,
		gateway,
	)
}

// performPaymentOrdersCreate 以指定令牌 POST /api/v1/payments/orders。
func performPaymentOrdersCreate(router *gin.Engine, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/orders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestRouterPaymentOrdersPermissionMatrix 覆盖创建支付订单接口的权限矩阵（契约 §1.2、§6.9）：
// ROOT 管理端令牌成功创建（201 + Location）；非 ROOT 管理端令牌 403 AUTH_FORBIDDEN；
// 患者令牌因 realm 不匹配 401 AUTH_INVALID_TOKEN；无令牌 401。
func TestRouterPaymentOrdersPermissionMatrix(t *testing.T) {
	const body = `{"registrationId":1001}`

	// 支付窗口完整但 prepay_id 为空：命中首次预下单分支，成功时返回 201。
	now := time.Now().UTC()
	repo := &paymentOrdersTestRepo{item: &domainpayment.Payment{
		RegistrationID: 1001,
		OutTradeNo:     "202609080001",
		Amount:         "80.00",
		PaymentStatus:  domainpayment.PaymentStatusUnpaid,
		PrecreateAt:    now,
		PayDeadline:    now.Add(30 * time.Minute),
		ExpireAt:       now.Add(35 * time.Minute),
	}}
	gateway := &paymentOrdersTestGateway{}
	router := newPaymentOrdersTestRouter(t, repo, gateway)

	t.Run("ROOT 管理端令牌成功创建", func(t *testing.T) {
		token := signPaymentOrdersAccessToken(t, domainauth.RealmMis)
		w := performPaymentOrdersCreate(router, token, body)
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
		}
		if location := w.Header().Get("Location"); location != "/api/v1/payments/202609080001" {
			t.Fatalf("Location = %q，期望 /api/v1/payments/202609080001", location)
		}
		if len(gateway.precreateCalls) != 1 {
			t.Fatalf("预下单调用次数 = %d，期望 1", len(gateway.precreateCalls))
		}
		if len(repo.regQueries) != 1 || repo.regQueries[0] != 0 {
			t.Fatalf("仓储归属入参 = %v，期望管理端传 0（不限定归属）", repo.regQueries)
		}
	})

	t.Run("非 ROOT 管理端令牌被拒", func(t *testing.T) {
		nonRootRouter, nonRootToken := newRealmTestRouter(t, domainauth.RealmMis)
		w := performPaymentOrdersCreate(nonRootRouter, nonRootToken, body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_FORBIDDEN" {
			t.Fatalf("code = %v, want AUTH_FORBIDDEN", code)
		}
	})

	t.Run("患者令牌因 realm 不匹配被拒", func(t *testing.T) {
		patientRouter, patientToken := newRealmTestRouter(t, domainauth.RealmPatient)
		w := performPaymentOrdersCreate(patientRouter, patientToken, body)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
			t.Fatalf("code = %v, want AUTH_INVALID_TOKEN", code)
		}
	})

	t.Run("无令牌被拒", func(t *testing.T) {
		w := performPaymentOrdersCreate(router, "", body)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		if code := decodeRealmRouterBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
			t.Fatalf("code = %v, want AUTH_INVALID_TOKEN", code)
		}
	})
}

// 患者端认证 HTTP 层单测：wechat-login / refresh / logout / me 的状态码、
// 错误码、Cookie 行为与响应字段（spec/04-api-contract.md §7.1-§7.3、§10、§12.5）。
//
// 用内存桩替换 port.PatientRepository / port.TokenRepository / port.WeChatAuthenticator，
// gin 处于 TestMode，不依赖 PostgreSQL、Redis 与微信网络。
package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/domain/patient"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/usecase/authsession"
	patientauthservice "Medical-Web-Backend/internal/usecase/patientauth"
)

const (
	// patientAuthHandlerSecret 是本文件所有用例共用的 JWT 签名密钥。
	patientAuthHandlerSecret = "patientauth-handler-test-secret"
	// patientAuthHandlerOpenID 是微信桩默认返回的 openid。
	patientAuthHandlerOpenID = "openid-handler-test"
)

// patientAuthHandlerTokenRepo 是内存版 port.TokenRepository 桩。
type patientAuthHandlerTokenRepo struct {
	port.TokenRepository

	sessions   map[string]port.RefreshSession
	deleted    []string
	revoked    []string
	sidDeleted []string
	sidMissed  []string
	// deletedSIDs 与 deleted 一一对应，记录 DeleteRefreshSession 收到的 sessionID：
	// 生产实现靠它一并删除反查索引，桩若丢弃该参数就无法测出相关回归。
	deletedSIDs []string

	// getErr 非 nil 时 GetRefreshSession 恒定失败，用于模拟 Redis 不可用。
	getErr error
	// getCalls 记录 GetRefreshSession 的调用次数，用于断言实现没有回退到另一条令牌通道。
	getCalls int
	// sidErr 非 nil 时 DeleteRefreshSessionBySessionID 恒定失败。
	sidErr error
}

func newPatientAuthHandlerTokenRepo() *patientAuthHandlerTokenRepo {
	return &patientAuthHandlerTokenRepo{sessions: map[string]port.RefreshSession{}}
}

func (r *patientAuthHandlerTokenRepo) SaveRefreshSession(
	_ context.Context,
	session port.RefreshSession,
	_ int64,
) error {
	r.sessions[session.TokenHash] = session
	return nil
}

func (r *patientAuthHandlerTokenRepo) GetRefreshSession(
	_ context.Context,
	tokenHash string,
) (*port.RefreshSession, error) {
	r.getCalls++
	if r.getErr != nil {
		return nil, r.getErr
	}
	session, ok := r.sessions[tokenHash]
	if !ok {
		return nil, port.ErrRefreshSessionNotFound
	}
	copied := session
	return &copied, nil
}

func (r *patientAuthHandlerTokenRepo) DeleteRefreshSession(
	_ context.Context,
	tokenHash string,
	sessionID string,
) error {
	r.deleted = append(r.deleted, tokenHash)
	r.deletedSIDs = append(r.deletedSIDs, sessionID)
	delete(r.sessions, tokenHash)
	return nil
}

// DeleteRefreshSessionBySessionID 按 sessionID 在内存里反查会话并删除，
// 对应生产实现里 Redis 的 sessionID -> tokenHash 反查索引：
// 命中返回 (true, nil)，未命中返回 (false, nil)（登出保持幂等）。
func (r *patientAuthHandlerTokenRepo) DeleteRefreshSessionBySessionID(
	_ context.Context,
	sessionID string,
) (bool, error) {
	if r.sidErr != nil {
		return false, r.sidErr
	}
	if sessionID == "" {
		return false, nil
	}
	for tokenHash, session := range r.sessions {
		if session.SessionID != sessionID {
			continue
		}
		r.deleted = append(r.deleted, tokenHash)
		r.sidDeleted = append(r.sidDeleted, sessionID)
		delete(r.sessions, tokenHash)
		return true, nil
	}
	r.sidMissed = append(r.sidMissed, sessionID)
	return false, nil
}

func (r *patientAuthHandlerTokenRepo) RevokeAccessToken(
	_ context.Context,
	jti string,
	_ int64,
) error {
	r.revoked = append(r.revoked, jti)
	return nil
}

func (r *patientAuthHandlerTokenRepo) IsAccessTokenRevoked(
	_ context.Context,
	_ string,
) (bool, error) {
	return false, nil
}

// patientAuthHandlerPatientRepo 是内存版 port.PatientRepository 桩。
type patientAuthHandlerPatientRepo struct {
	port.PatientRepository

	patients map[int64]*patient.Patient
	cards    map[int64]*patient.Card
	nextID   int64

	// findPatientErr / findOrCreateErr 非 nil 时对应方法恒定失败，模拟 PostgreSQL 不可用。
	findPatientErr  error
	findOrCreateErr error
}

func newPatientAuthHandlerPatientRepo() *patientAuthHandlerPatientRepo {
	return &patientAuthHandlerPatientRepo{
		patients: map[int64]*patient.Patient{},
		cards:    map[int64]*patient.Card{},
		nextID:   20,
	}
}

func (r *patientAuthHandlerPatientRepo) FindPatientByID(
	_ context.Context,
	patientID int64,
) (*patient.Patient, error) {
	if r.findPatientErr != nil {
		return nil, r.findPatientErr
	}
	profile, ok := r.patients[patientID]
	if !ok {
		return nil, patient.ErrPatientNotFound
	}
	copied := *profile
	return &copied, nil
}

func (r *patientAuthHandlerPatientRepo) FindOrCreatePatientByOpenID(
	_ context.Context,
	openID string,
	now time.Time,
) (*patient.Patient, bool, error) {
	if openID == "" {
		return nil, false, patient.ErrOpenIDRequired
	}
	if r.findOrCreateErr != nil {
		return nil, false, r.findOrCreateErr
	}
	for _, profile := range r.patients {
		if profile.OpenID == openID {
			copied := *profile
			return &copied, false, nil
		}
	}

	id := r.nextID
	r.nextID++
	profile := &patient.Patient{
		ID:         id,
		OpenID:     openID,
		Status:     patient.StatusActive,
		CreateDate: now.Format("2006-01-02"),
	}
	r.patients[id] = profile

	copied := *profile
	return &copied, true, nil
}

func (r *patientAuthHandlerPatientRepo) FindCardByPatientID(
	_ context.Context,
	patientID int64,
) (*patient.Card, error) {
	card, ok := r.cards[patientID]
	if !ok {
		return nil, patient.ErrCardNotFound
	}
	copied := *card
	return &copied, nil
}

// seedPatient 预置一个患者账号。
func (r *patientAuthHandlerPatientRepo) seedPatient(id int64, status string) *patient.Patient {
	nickname := "小明"
	photo := "https://cdn.example/avatar.jpg"
	sex := "男"
	profile := &patient.Patient{
		ID:         id,
		OpenID:     patientAuthHandlerOpenID,
		Nickname:   &nickname,
		Photo:      &photo,
		Sex:        &sex,
		Status:     status,
		CreateDate: "2026-09-08",
	}
	r.patients[id] = profile
	return profile
}

// seedCard 为指定账号预置一张就诊卡。
func (r *patientAuthHandlerPatientRepo) seedCard(patientID int64, tel string) *patient.Card {
	card := &patient.Card{
		ID:             10,
		UserID:         patientID,
		UUID:           "CARD0000000000000000000000000010",
		Name:           "张三",
		Sex:            "男",
		PID:            "110101199001011237",
		Tel:            tel,
		Birthday:       "1990-01-01",
		MedicalHistory: []string{"无"},
		InsuranceType:  "无",
	}
	r.cards[patientID] = card
	return card
}

// patientAuthHandlerWeChat 是 port.WeChatAuthenticator 桩。
type patientAuthHandlerWeChat struct {
	openID string
	err    error
	// calls 记录 code2Session 收到的 code：openid 直通登录必须保持零调用。
	calls []string
}

func (w *patientAuthHandlerWeChat) Code2Session(
	_ context.Context,
	code string,
) (string, error) {
	w.calls = append(w.calls, code)
	return w.openID, w.err
}

// patientAuthHandlerEnv 汇总最小路由树与其依赖桩。
type patientAuthHandlerEnv struct {
	router  *gin.Engine
	service *patientauthservice.Service
	repo    *patientAuthHandlerPatientRepo
	tokens  *patientAuthHandlerTokenRepo
	wechat  *patientAuthHandlerWeChat
}

// newPatientAuthHandlerEnv 构造与 router.go 一致的患者端认证路由（默认非测试环境）：
// auth 三个接口自带凭据校验，只有 /patient/me 经过 realm=patient 的令牌中间件。
func newPatientAuthHandlerEnv(t *testing.T) *patientAuthHandlerEnv {
	t.Helper()
	return newPatientAuthHandlerEnvWithOpenIDLogin(t, false)
}

// newPatientAuthHandlerEnvWithOpenIDLogin 与 newPatientAuthHandlerEnv 同构，
// 只多了 openid 直通开关（对应 router.go 里 cfg.App.Env == "development" 的判定结果）。
func newPatientAuthHandlerEnvWithOpenIDLogin(
	t *testing.T,
	allowOpenIDLogin bool,
) *patientAuthHandlerEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repo := newPatientAuthHandlerPatientRepo()
	tokens := newPatientAuthHandlerTokenRepo()
	wechat := &patientAuthHandlerWeChat{openID: patientAuthHandlerOpenID}

	service := patientauthservice.NewService(
		repo,
		tokens,
		wechat,
		patientauthservice.Config{
			JWTSecret:  patientAuthHandlerSecret,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 24 * time.Hour,
		},
	)

	// 由调用方决定是否放行 openid 直通登录：非 development 环境恒为 false。
	h := NewPatientAuthHandler(service, false, allowOpenIDLogin)
	router := gin.New()
	router.POST("/api/v1/patient/auth/wechat-login", h.WeChatLogin)
	router.POST("/api/v1/patient/auth/refresh", h.Refresh)
	router.POST("/api/v1/patient/auth/logout", h.Logout)
	router.GET(
		"/api/v1/patient/me",
		middleware.RequireAccessToken(service, domainauth.RealmPatient),
		h.Me,
	)

	return &patientAuthHandlerEnv{
		router:  router,
		service: service,
		repo:    repo,
		tokens:  tokens,
		wechat:  wechat,
	}
}

// request 发起请求；body 非空时作为 JSON 请求体，mutate 用于附加头或 Cookie。
func (e *patientAuthHandlerEnv) request(
	method string,
	path string,
	body string,
	mutate func(*http.Request),
) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// login 直接经 use case 登录，返回本次令牌，避免用例重复解析响应体。
func (e *patientAuthHandlerEnv) login(t *testing.T) *patientauthservice.LoginResult {
	t.Helper()
	result, err := e.service.WeChatLogin(context.Background(), "wx_code_abc123")
	if err != nil {
		t.Fatalf("前置登录失败：%v", err)
	}
	return result
}

// misAccessToken 签发 realm=mis 的 access token，验证患者域拒绝跨域令牌。
func (e *patientAuthHandlerEnv) misAccessToken(t *testing.T) string {
	t.Helper()
	manager := authsession.NewManager(e.tokens, authsession.Config{
		JWTSecret:  patientAuthHandlerSecret,
		AccessTTL:  15 * time.Minute,
		RefreshTTL: time.Hour,
	})
	pair, _, err := manager.Issue(
		domainauth.RealmMis,
		authsession.Subject{ID: 7, Name: "mis-admin"},
	)
	if err != nil {
		t.Fatalf("签发管理端测试令牌失败：%v", err)
	}
	return pair.AccessToken
}

// setCookieByName 从响应的 Set-Cookie 中取出指定名字的 Cookie。
func setCookieByName(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

// parsePatientExpiresAt 断言 accessExpiresAt 是可解析的 RFC3339 时间串。
func parsePatientExpiresAt(t *testing.T, value any) time.Time {
	t.Helper()
	return parsePatientRFC3339(t, "accessExpiresAt", value)
}

// parsePatientRFC3339 断言指定响应字段是可解析的非空 RFC3339 时间串，
// 供 accessExpiresAt / refreshExpiresAt 等字段复用（契约 §7.1、§7.2）。
func parsePatientRFC3339(t *testing.T, field string, value any) time.Time {
	t.Helper()
	text, ok := value.(string)
	if !ok || text == "" {
		t.Fatalf("%s = %v，期望非空 RFC3339 字符串", field, value)
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("%s = %q，无法按 RFC3339 解析：%v", field, text, err)
	}
	return parsed
}

// refreshResponseToken 取出响应体里的 refreshToken 字段；缺失或非字符串时直接失败，
// 避免用例只断言 Cookie 通道而漏掉小程序通道（契约 §1.2、§7.2）。
func refreshResponseToken(t *testing.T, body map[string]any) string {
	t.Helper()
	token, ok := body["refreshToken"].(string)
	if !ok || token == "" {
		t.Fatalf("refreshToken = %v，期望非空字符串", body["refreshToken"])
	}
	return token
}

// withRefreshCookie 为请求附加浏览器通道的 refresh Cookie。
func withRefreshCookie(value string) func(*http.Request) {
	return func(req *http.Request) {
		req.AddCookie(&http.Cookie{
			Name:  authsession.RefreshCookieName,
			Value: value,
		})
	}
}

// --- POST /api/v1/patient/auth/wechat-login ---

// TestPatientWeChatLoginSuccessResponseShape 覆盖登录成功：响应字段齐全、
// refresh token 同时经 HttpOnly Cookie 与响应体下发（契约 §1.2、§7.1）。
func TestPatientWeChatLoginSuccessResponseShape(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	env.repo.seedCard(20, "13800138000")

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"wx_code_abc123"}`,
		nil,
	)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if body["isNewUser"] != true {
		t.Errorf("isNewUser = %v，期望 true", body["isNewUser"])
	}
	if cardID, ok := body["cardId"].(float64); !ok || cardID != 10 {
		t.Errorf("cardId = %v，期望 10", body["cardId"])
	}
	if token, ok := body["accessToken"].(string); !ok || token == "" {
		t.Errorf("accessToken = %v，期望非空字符串", body["accessToken"])
	}
	parsePatientExpiresAt(t, body["accessExpiresAt"])

	summary, ok := body["patient"].(map[string]any)
	if !ok {
		t.Fatalf("patient = %v，期望对象", body["patient"])
	}
	if id, ok := summary["id"].(float64); !ok || id != 20 {
		t.Errorf("patient.id = %v，期望 20", summary["id"])
	}
	if summary["status"] != "ACTIVE" {
		t.Errorf("patient.status = %v，期望 ACTIVE", summary["status"])
	}
	// 首次注册由仓储写入创建日期（本例为注入的 now），只校验形状为 YYYY-MM-DD。
	createDate, ok := summary["createDate"].(string)
	if !ok || createDate == "" {
		t.Fatalf("patient.createDate = %v，期望非空日期字符串", summary["createDate"])
	}
	if _, err := time.Parse("2006-01-02", createDate); err != nil {
		t.Errorf("patient.createDate = %q，无法按 2006-01-02 解析：%v", createDate, err)
	}
	// openId 属于禁止返回字段（契约 §2.2）。
	if _, exists := summary["openId"]; exists {
		t.Error("患者摘要不得包含 openId")
	}

	// refresh token 同时经 HttpOnly Cookie（浏览器）与响应体（小程序）下发：
	// 两条通道必须是同一个轮换令牌，Cookie 的 HttpOnly 等属性保持不变（契约 §1.2、§7.1）。
	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("缺少 %s Cookie；Set-Cookie=%v", authsession.RefreshCookieName, w.Header().Values("Set-Cookie"))
	}
	if !refreshCookie.HttpOnly {
		t.Error("refresh Cookie 必须是 HttpOnly")
	}
	if refreshCookie.Value == "" {
		t.Error("refresh Cookie 不得为空")
	}
	if bodyToken := refreshResponseToken(t, body); bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}

	accessCookie := setCookieByName(w, authsession.AccessCookieName)
	if accessCookie == nil || !accessCookie.HttpOnly {
		t.Errorf("缺少 HttpOnly 的 %s Cookie，实际 %+v", authsession.AccessCookieName, accessCookie)
	}
}

// TestPatientWeChatLoginReturnsRefreshTokenInBody 覆盖小程序无 Cookie 通道：
// 登录响应体必须回传与 refresh Cookie 完全一致的新 refreshToken，
// 且 refreshExpiresAt 可解析为 RFC3339 并晚于 accessExpiresAt（契约 §1.2、§7.1）。
func TestPatientWeChatLoginReturnsRefreshTokenInBody(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"wx_code_abc123"}`,
		nil,
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("缺少 %s Cookie；Set-Cookie=%v", authsession.RefreshCookieName, w.Header().Values("Set-Cookie"))
	}

	bodyToken := refreshResponseToken(t, body)
	if bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
	// 回传的令牌必须真的对应服务端会话，而不是仅仅回显了一个字符串。
	if _, ok := env.tokens.sessions[authsession.HashRefreshToken(bodyToken)]; !ok {
		t.Error("响应体回传的 refresh token 必须对应已写入的刷新会话")
	}

	accessExpiresAt := parsePatientRFC3339(t, "accessExpiresAt", body["accessExpiresAt"])
	refreshExpiresAt := parsePatientRFC3339(t, "refreshExpiresAt", body["refreshExpiresAt"])
	if !refreshExpiresAt.After(accessExpiresAt) {
		t.Errorf(
			"refreshExpiresAt = %v，期望晚于 accessExpiresAt = %v",
			refreshExpiresAt,
			accessExpiresAt,
		)
	}
}

// TestPatientWeChatLoginWithoutCardReturnsNullCardId 覆盖尚未实名建卡：cardId 为 null。
func TestPatientWeChatLoginWithoutCardReturnsNullCardId(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"wx_code_abc123"}`,
		nil,
	)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if value, exists := body["cardId"]; !exists || value != nil {
		t.Errorf("cardId = %v（exists=%t），期望显式 null", value, exists)
	}
}

// TestPatientWeChatLoginValidationErrors 覆盖请求体校验：
// 缺 code → 422 REQUEST_VALIDATION_FAILED；非法 JSON → 400 REQUEST_INVALID_JSON。
func TestPatientWeChatLoginValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		code    string
		message string
	}{
		{
			name:    "code 为空字符串",
			body:    `{"code":""}`,
			status:  http.StatusUnprocessableEntity,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "微信登录 code 不能为空",
		},
		{
			name:    "code 为纯空白",
			body:    `{"code":"   "}`,
			status:  http.StatusUnprocessableEntity,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "微信登录 code 不能为空",
		},
		{
			name:    "缺少 code 字段",
			body:    `{}`,
			status:  http.StatusUnprocessableEntity,
			code:    "REQUEST_VALIDATION_FAILED",
			message: "微信登录 code 不能为空",
		},
		{
			name:   "请求体 JSON 语法错误",
			body:   `{"code":}`,
			status: http.StatusBadRequest,
			code:   "REQUEST_INVALID_JSON",
		},
		{
			name:   "请求体为截断 JSON",
			body:   `{"code":"wx_code_abc123"`,
			status: http.StatusBadRequest,
			code:   "REQUEST_INVALID_JSON",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthHandlerEnv(t)

			w := env.request(
				http.MethodPost,
				"/api/v1/patient/auth/wechat-login",
				tc.body,
				nil,
			)
			body := assertErrorStatus(t, w, tc.status, tc.code)
			if tc.message != "" && body["message"] != tc.message {
				t.Errorf("message = %v，期望 %q", body["message"], tc.message)
			}

			// 校验失败不得下发任何 Cookie。
			if cookies := w.Result().Cookies(); len(cookies) != 0 {
				t.Errorf("校验失败不得下发 Cookie，实际 %v", cookies)
			}
		})
	}
}

// TestPatientWeChatLoginTypeMismatchReturnsInvalidJSON 覆盖字段类型错误：
// code 为数字时绑定层返回 json.UnmarshalTypeError，按 400 REQUEST_INVALID_JSON 处理。
// 这与统一请求绑定约定一致（auth.go 与 schedule 写接口都把 SyntaxError /
// UnmarshalTypeError / io.ErrUnexpectedEOF 归入 400）：只有字段取值校验失败才是
// 422 REQUEST_VALIDATION_FAILED（契约 §10）。
func TestPatientWeChatLoginTypeMismatchReturnsInvalidJSON(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":123}`,
		nil,
	)
	assertErrorStatus(t, w, http.StatusBadRequest, "REQUEST_INVALID_JSON")
}

// TestPatientWeChatLoginDisabledAccountForbidden 覆盖禁用账号：403 AUTH_FORBIDDEN，
// 且不得下发任何令牌 Cookie。
func TestPatientWeChatLoginDisabledAccountForbidden(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	env.repo.seedPatient(20, patient.StatusDisabled)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"wx_code_abc123"}`,
		nil,
	)

	assertErrorStatus(t, w, http.StatusForbidden, "AUTH_FORBIDDEN")
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("禁用账号不得下发 Cookie，实际 %v", cookies)
	}
	if len(env.tokens.sessions) != 0 {
		t.Error("禁用账号不得写入 refresh 会话")
	}
}

// TestPatientWeChatLoginWeChatUnavailableBadGateway 覆盖微信不可用：
// 502 DEPENDENCY_UNAVAILABLE（可重试）。
func TestPatientWeChatLoginWeChatUnavailableBadGateway(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	env.wechat.err = port.ErrWeChatUnavailable

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"wx_code_abc123"}`,
		nil,
	)

	assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
}

// TestPatientWeChatLoginWeChatCodeInvalidUnprocessable 覆盖微信判定 code 不可用：
// 422 REQUEST_VALIDATION_FAILED（契约 §7.1）。
func TestPatientWeChatLoginWeChatCodeInvalidUnprocessable(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	env.wechat.err = port.ErrWeChatCodeInvalid

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"wx_code_used"}`,
		nil,
	)

	assertErrorStatus(t, w, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED")
}

// --- openid 直通登录（仅测试阶段，APP_ENV=development）---

// TestPatientWeChatLoginWithOpenIDInDevelopment 覆盖测试阶段直通登录：
// allowOpenIDLogin=true（APP_ENV=development）时只提交 openid 即可成功登录，
// 全程不调用微信 code2Session，并按该 openid 建号（契约 §7.1）。
func TestPatientWeChatLoginWithOpenIDInDevelopment(t *testing.T) {
	env := newPatientAuthHandlerEnvWithOpenIDLogin(t, true)
	const openID = "openid-handler-direct"
	body := `{"openid":"` + openID + `"}`

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		body,
		nil,
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(env.wechat.calls) != 0 {
		t.Errorf("openid 直通不得调用微信 code2Session，实际 %v", env.wechat.calls)
	}

	resp := decodeBody(t, w)
	if resp["isNewUser"] != true {
		t.Errorf("isNewUser = %v，期望 true", resp["isNewUser"])
	}
	summary, ok := resp["patient"].(map[string]any)
	if !ok {
		t.Fatalf("patient = %v，期望对象", resp["patient"])
	}
	if id, ok := summary["id"].(float64); !ok || id != 20 {
		t.Errorf("patient.id = %v，期望 20", summary["id"])
	}
	if _, exists := summary["openId"]; exists {
		t.Error("患者摘要不得包含 openId")
	}
	if token, ok := resp["accessToken"].(string); !ok || token == "" {
		t.Errorf("accessToken = %v，期望非空字符串", resp["accessToken"])
	}
	parsePatientExpiresAt(t, resp["accessExpiresAt"])
	if cardID, exists := resp["cardId"]; !exists || cardID != nil {
		t.Errorf("cardId = %v，期望 null", resp["cardId"])
	}

	// 必须按请求体提交的 openid 建号，而不是微信桩的默认 openid。
	profile := env.repo.patients[20]
	if profile == nil || profile.OpenID != openID {
		t.Fatalf("账号 = %+v，期望 openid=%q", profile, openID)
	}

	// refresh 通道与 code 登录保持一致：Cookie 与响应体是同一个已落库的令牌。
	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil || refreshCookie.Value == "" {
		t.Fatalf(
			"直通登录必须下发 refresh Cookie，实际 %v",
			w.Header().Values("Set-Cookie"),
		)
	}
	bodyToken := refreshResponseToken(t, resp)
	if bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
	if _, exists := env.tokens.sessions[authsession.HashRefreshToken(bodyToken)]; !exists {
		t.Error("响应体回传的 refresh token 必须对应已写入的刷新会话")
	}

	// 同一 openid 再次直通登录必须命中同一账号。
	again := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		body,
		nil,
	)
	if again.Code != http.StatusOK {
		t.Fatalf(
			"再次登录 status = %d, want 200; body=%s",
			again.Code,
			again.Body.String(),
		)
	}
	if againBody := decodeBody(t, again); againBody["isNewUser"] != false {
		t.Errorf("已存在账号 isNewUser = %v，期望 false", againBody["isNewUser"])
	}
	if len(env.repo.patients) != 1 {
		t.Errorf("账号数量 = %d，期望 1", len(env.repo.patients))
	}
}

// TestPatientWeChatLoginWhitespaceCodeFallsBackToOpenID 覆盖 code 为纯空白时按未提交处理：
// development 下「纯空白 code + 合法 openid」仍必须走 openid 直通，
// 而不是报「微信登录 code 不能为空」。
func TestPatientWeChatLoginWhitespaceCodeFallsBackToOpenID(t *testing.T) {
	env := newPatientAuthHandlerEnvWithOpenIDLogin(t, true)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"code":"   ","openid":"openid-handler-blank-code"}`,
		nil,
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(env.wechat.calls) != 0 {
		t.Errorf("纯空白 code 不得调用微信 code2Session，实际 %v", env.wechat.calls)
	}
	profile := env.repo.patients[20]
	if profile == nil || profile.OpenID != "openid-handler-blank-code" {
		t.Fatalf("账号 = %+v，期望按 openid 直通建号", profile)
	}
}

// TestPatientWeChatLoginOpenIDRejectedOutsideDevelopment 覆盖非测试环境
// （allowOpenIDLogin=false）提交 openid：422 REQUEST_VALIDATION_FAILED，
// 且不得建号、不得调用微信、不得下发 Cookie。
func TestPatientWeChatLoginOpenIDRejectedOutsideDevelopment(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/wechat-login",
		`{"openid":"openid-not-allowed"}`,
		nil,
	)
	resp := assertErrorStatus(
		t,
		w,
		http.StatusUnprocessableEntity,
		"REQUEST_VALIDATION_FAILED",
	)
	if resp["message"] != "当前环境不支持使用 openid 登录" {
		t.Errorf(
			"message = %v，期望 %q",
			resp["message"],
			"当前环境不支持使用 openid 登录",
		)
	}
	if len(env.repo.patients) != 0 {
		t.Errorf("非测试环境不得按 openid 建号，实际 %d 个账号", len(env.repo.patients))
	}
	if len(env.tokens.sessions) != 0 {
		t.Error("非测试环境不得写入 refresh 会话")
	}
	if len(env.wechat.calls) != 0 {
		t.Errorf("非测试环境不得调用微信 code2Session，实际 %v", env.wechat.calls)
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("拒绝 openid 登录时不得下发 Cookie，实际 %v", cookies)
	}
}

// TestPatientWeChatLoginBlankCredentialsUnprocessable 覆盖 code 与 openid 都未提交（含纯空白）：
// 无论是否放行 openid 直通，都按「凭据缺失」返回 422，
// 文案与 code 缺失保持一致（契约 §12.5）。
func TestPatientWeChatLoginBlankCredentialsUnprocessable(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"code":""}`,
		`{"code":"   "}`,
		`{"openid":""}`,
		`{"openid":"   "}`,
		`{"code":"","openid":""}`,
		`{"code":"   ","openid":"   "}`,
	}

	for _, allowOpenIDLogin := range []bool{false, true} {
		for _, body := range bodies {
			t.Run(
				fmt.Sprintf("allowOpenIDLogin=%v/%s", allowOpenIDLogin, body),
				func(t *testing.T) {
					env := newPatientAuthHandlerEnvWithOpenIDLogin(
						t,
						allowOpenIDLogin,
					)

					w := env.request(
						http.MethodPost,
						"/api/v1/patient/auth/wechat-login",
						body,
						nil,
					)
					resp := assertErrorStatus(
						t,
						w,
						http.StatusUnprocessableEntity,
						"REQUEST_VALIDATION_FAILED",
					)
					if resp["message"] != "微信登录 code 不能为空" {
						t.Errorf(
							"message = %v，期望 %q",
							resp["message"],
							"微信登录 code 不能为空",
						)
					}
					if len(env.repo.patients) != 0 {
						t.Error("凭据缺失不得建号")
					}
					if len(env.tokens.sessions) != 0 {
						t.Error("凭据缺失不得写入 refresh 会话")
					}
					if len(env.wechat.calls) != 0 {
						t.Errorf(
							"凭据缺失不得调用微信 code2Session，实际 %v",
							env.wechat.calls,
						)
					}
					if cookies := w.Result().Cookies(); len(cookies) != 0 {
						t.Errorf("凭据缺失不得下发 Cookie，实际 %v", cookies)
					}
				},
			)
		}
	}
}

// TestPatientWeChatLoginInvalidOpenIDUnprocessable 覆盖 openid 形状校验失败：
// development 下超过 128 字符由 use case 拒绝，handler 映射 422「微信登录 openid 无效」；
// 非 development 下环境开关先行拦截，仍返回 422「当前环境不支持使用 openid 登录」。
func TestPatientWeChatLoginInvalidOpenIDUnprocessable(t *testing.T) {
	cases := []struct {
		name             string
		allowOpenIDLogin bool
		message          string
	}{
		{
			name:             "development 下超长 openid",
			allowOpenIDLogin: true,
			message:          "微信登录 openid 无效",
		},
		{
			name:             "非 development 下超长 openid 先被环境拦截",
			allowOpenIDLogin: false,
			message:          "当前环境不支持使用 openid 登录",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthHandlerEnvWithOpenIDLogin(t, tc.allowOpenIDLogin)

			w := env.request(
				http.MethodPost,
				"/api/v1/patient/auth/wechat-login",
				`{"openid":"`+strings.Repeat("a", 129)+`"}`,
				nil,
			)
			resp := assertErrorStatus(
				t,
				w,
				http.StatusUnprocessableEntity,
				"REQUEST_VALIDATION_FAILED",
			)
			if resp["message"] != tc.message {
				t.Errorf("message = %v，期望 %q", resp["message"], tc.message)
			}
			if len(env.repo.patients) != 0 {
				t.Error("openid 非法不得建号")
			}
			if len(env.wechat.calls) != 0 {
				t.Errorf("openid 非法不得调用微信 code2Session，实际 %v", env.wechat.calls)
			}
			if cookies := w.Result().Cookies(); len(cookies) != 0 {
				t.Errorf("校验失败不得下发 Cookie，实际 %v", cookies)
			}
		})
	}
}

// TestPatientWeChatLoginPrefersCodeOverOpenID 覆盖同时提交 code 与 openid：
// 一律以 code 为准、openid 完全忽略；openid 是否合法与是否放行直通都不影响结果。
func TestPatientWeChatLoginPrefersCodeOverOpenID(t *testing.T) {
	cases := []struct {
		name   string
		openID string
	}{
		{name: "openid 合法", openID: "openid-must-be-ignored"},
		{name: "openid 超长", openID: strings.Repeat("a", 129)},
	}

	for _, allowOpenIDLogin := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(
				fmt.Sprintf("allowOpenIDLogin=%v/%s", allowOpenIDLogin, tc.name),
				func(t *testing.T) {
					env := newPatientAuthHandlerEnvWithOpenIDLogin(
						t,
						allowOpenIDLogin,
					)

					w := env.request(
						http.MethodPost,
						"/api/v1/patient/auth/wechat-login",
						`{"code":"wx_code_abc123","openid":"`+tc.openID+`"}`,
						nil,
					)
					if w.Code != http.StatusOK {
						t.Fatalf(
							"status = %d, want 200; body=%s",
							w.Code,
							w.Body.String(),
						)
					}
					if len(env.wechat.calls) != 1 ||
						env.wechat.calls[0] != "wx_code_abc123" {
						t.Errorf(
							"有 code 时必须走微信换取，实际调用 %v",
							env.wechat.calls,
						)
					}
					if resp := decodeBody(t, w); resp["isNewUser"] != true {
						t.Errorf("isNewUser = %v，期望 true", resp["isNewUser"])
					}
					// 账号必须落在微信桩返回的 openid 上，而不是请求体里的值。
					profile := env.repo.patients[20]
					if profile == nil || profile.OpenID != patientAuthHandlerOpenID {
						t.Fatalf(
							"账号 = %+v，期望微信桩返回的 openid %q",
							profile,
							patientAuthHandlerOpenID,
						)
					}
					if len(env.repo.patients) != 1 {
						t.Errorf(
							"不得按请求体 openid 建号，实际 %d 个账号",
							len(env.repo.patients),
						)
					}
				},
			)
		}
	}
}

// --- POST /api/v1/patient/auth/refresh ---

// TestPatientRefreshWithBodyToken 覆盖请求体携带 refresh token 的成功刷新：
// 轮换后的 refresh token 同时写入 Cookie 与响应体（契约 §7.2）。
func TestPatientRefreshWithBodyToken(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
		nil,
	)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if token, ok := body["accessToken"].(string); !ok || token == "" {
		t.Errorf("accessToken = %v，期望非空字符串", body["accessToken"])
	}
	parsePatientExpiresAt(t, body["accessExpiresAt"])
	// 小程序无 Cookie 通道，轮换后的 refresh token 必须回传响应体（契约 §7.2）。
	bodyRefreshToken := refreshResponseToken(t, body)
	parsePatientRFC3339(t, "refreshExpiresAt", body["refreshExpiresAt"])

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil || !refreshCookie.HttpOnly {
		t.Fatalf("刷新后必须下发 HttpOnly 的 refresh Cookie，实际 %+v", refreshCookie)
	}
	if refreshCookie.Value == login.Tokens.RefreshToken {
		t.Error("刷新必须轮换 refresh token，不得复用旧值")
	}
	if bodyRefreshToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于轮换后 refresh Cookie 的值 %q",
			bodyRefreshToken,
			refreshCookie.Value,
		)
	}
	if !containsString(env.tokens.deleted, authsession.HashRefreshToken(login.Tokens.RefreshToken)) {
		t.Error("旧 refresh 会话必须被撤销")
	}
	loginClaims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析登录 access token 失败：%v", err)
	}
	if !containsString(env.tokens.deletedSIDs, loginClaims.SessionID) {
		t.Errorf(
			"轮换必须把会话 sessionID 传给仓储，实际 %v",
			env.tokens.deletedSIDs,
		)
	}
}

// TestPatientRefreshWithCookieToken 覆盖 Cookie 携带 refresh token 的成功刷新
// （小程序之外的浏览器通道）。
func TestPatientRefreshWithCookieToken(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		"",
		func(req *http.Request) {
			req.AddCookie(&http.Cookie{
				Name:  authsession.RefreshCookieName,
				Value: login.Tokens.RefreshToken,
			})
		},
	)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if token, ok := body["accessToken"].(string); !ok || token == "" {
		t.Errorf("accessToken = %v，期望非空字符串", body["accessToken"])
	}
}

// TestPatientRefreshInvalidTokenUnauthorized 覆盖 refresh 失败：一律 401 AUTH_INVALID_REFRESH_TOKEN。
func TestPatientRefreshInvalidTokenUnauthorized(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		mutate func(*http.Request)
	}{
		{
			name: "请求体携带未知令牌",
			body: `{"refreshToken":"unknown-refresh-token"}`,
		},
		{
			name: "Cookie 携带未知令牌",
			mutate: func(req *http.Request) {
				req.AddCookie(&http.Cookie{
					Name:  authsession.RefreshCookieName,
					Value: "unknown-refresh-token",
				})
			},
		},
		{
			name: "完全没有 refresh token",
			body: `{}`,
		},
		{
			name: "空请求体",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthHandlerEnv(t)

			w := env.request(
				http.MethodPost,
				"/api/v1/patient/auth/refresh",
				tc.body,
				tc.mutate,
			)
			assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_REFRESH_TOKEN")
		})
	}
}

// TestPatientRefreshFallsBackToCookieWhenBodyTokenInvalid 覆盖兼容性回退：
// 请求体令牌无效时必须继续尝试 Cookie 通道，Cookie 有效则 200 并轮换该会话，
// 不能因为请求体令牌失效就把持有有效 Cookie 的浏览器判成未认证（契约 §7.2）。
func TestPatientRefreshFallsBackToCookieWhenBodyTokenInvalid(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"unknown-refresh-token"}`,
		withRefreshCookie(login.Tokens.RefreshToken),
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("刷新后必须下发 %s Cookie", authsession.RefreshCookieName)
	}
	if refreshCookie.Value == login.Tokens.RefreshToken {
		t.Error("回退 Cookie 成功后必须轮换 Cookie 会话")
	}
	if !containsString(env.tokens.deleted, authsession.HashRefreshToken(login.Tokens.RefreshToken)) {
		t.Errorf("Cookie 会话必须被撤销，实际 deleted=%v", env.tokens.deleted)
	}
	if bodyToken := refreshResponseToken(t, body); bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于轮换后 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
}

// TestPatientRefreshWithBodyTokenIgnoresInvalidCookie 覆盖反向组合：
// 请求体令牌有效、Cookie 无效或已过期时，请求体通道照常成功 200（契约 §7.2）。
func TestPatientRefreshWithBodyTokenIgnoresInvalidCookie(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
		withRefreshCookie("expired-or-unknown-refresh-token"),
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("刷新后必须下发 %s Cookie", authsession.RefreshCookieName)
	}
	bodyToken := refreshResponseToken(t, body)
	if bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于轮换后 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
	if bodyToken == login.Tokens.RefreshToken {
		t.Error("请求体通道必须轮换出新令牌")
	}
}

// TestPatientRefreshBothChannelsInvalidUnauthorized 覆盖两条通道都无效：
// 请求体与 Cookie 都拿不出有效令牌时才返回 401 AUTH_INVALID_REFRESH_TOKEN，
// 且不得产生任何会话副作用或下发 Cookie（契约 §7.2）。
func TestPatientRefreshBothChannelsInvalidUnauthorized(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"unknown-refresh-token"}`,
		withRefreshCookie("another-unknown-refresh-token"),
	)
	assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_REFRESH_TOKEN")

	if len(env.tokens.deleted) != 0 {
		t.Errorf("凭据无效不得产生轮换副作用，实际 deleted=%v", env.tokens.deleted)
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("凭据无效不得下发 Cookie，实际 %v", cookies)
	}
}

// TestPatientRefreshTreatsWhitespaceBodyTokenAsAbsent 覆盖空白值语义：
// 请求体 refreshToken 为纯空白时视为未携带，必须能回退到 Cookie 通道，
// 而不是把空白串当成凭据去校验后直接 401（契约 §7.2「空请求体等同只带 Cookie」）。
func TestPatientRefreshTreatsWhitespaceBodyTokenAsAbsent(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"   "}`,
		withRefreshCookie(login.Tokens.RefreshToken),
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	// 只有 Cookie 一条候选通道，因此只应发生一次轮换。
	if len(env.tokens.deleted) != 1 {
		t.Errorf("空白请求体令牌不得进入候选列表，实际 deleted=%v", env.tokens.deleted)
	}
	if !containsString(env.tokens.deleted, authsession.HashRefreshToken(login.Tokens.RefreshToken)) {
		t.Errorf("Cookie 会话必须被轮换，实际 deleted=%v", env.tokens.deleted)
	}
	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("刷新后必须下发 %s Cookie", authsession.RefreshCookieName)
	}
	if bodyToken := refreshResponseToken(t, body); bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于轮换后 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
}

// TestPatientRefreshDependencyFailureDoesNotFallBackToCookie 覆盖依赖故障的优先级：
// 会话存储读取失败必须立即上报 502 DEPENDENCY_UNAVAILABLE，不得继续尝试 Cookie 通道，
// 更不得把故障伪装成 401 让客户端误以为需要重新登录（契约 §7.2、§10）。
func TestPatientRefreshDependencyFailureDoesNotFallBackToCookie(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)
	env.tokens.getErr = errors.New("redis unavailable")

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
		withRefreshCookie(login.Tokens.RefreshToken),
	)
	assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")

	// 依赖故障必须立即结束候选循环：若实现继续尝试 Cookie 通道，这里会多读一次会话。
	if env.tokens.getCalls != 1 {
		t.Errorf("依赖故障时只应尝试一次会话读取，实际 %d 次", env.tokens.getCalls)
	}
	if len(env.tokens.deleted) != 0 {
		t.Errorf("依赖故障不得产生轮换副作用，实际 deleted=%v", env.tokens.deleted)
	}
	if _, ok := env.tokens.sessions[authsession.HashRefreshToken(login.Tokens.RefreshToken)]; !ok {
		t.Error("依赖故障时 Cookie 会话必须保持原状，不得被撤销")
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("依赖故障不得下发新 Cookie，实际 %v", cookies)
	}
}

// TestPatientRefreshWithBodyTokenRotatesAndReturnsToken 覆盖请求体通道的轮换语义：
// 成功刷新后响应体 refreshToken 必须等于新下发的 refresh Cookie 值、不得等于旧令牌，
// 且 refreshExpiresAt 可解析为 RFC3339 并晚于 accessExpiresAt（契约 §7.2）。
func TestPatientRefreshWithBodyTokenRotatesAndReturnsToken(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
		nil,
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("刷新后必须下发 %s Cookie", authsession.RefreshCookieName)
	}
	bodyToken := refreshResponseToken(t, body)
	if bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于轮换后 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
	if bodyToken == login.Tokens.RefreshToken {
		t.Error("响应体 refreshToken 必须是轮换后的新令牌，不得复用旧值")
	}

	accessExpiresAt := parsePatientRFC3339(t, "accessExpiresAt", body["accessExpiresAt"])
	refreshExpiresAt := parsePatientRFC3339(t, "refreshExpiresAt", body["refreshExpiresAt"])
	if !refreshExpiresAt.After(accessExpiresAt) {
		t.Errorf(
			"refreshExpiresAt = %v，期望晚于 accessExpiresAt = %v",
			refreshExpiresAt,
			accessExpiresAt,
		)
	}
}

// TestPatientRefreshWithCookieRotatesAndReturnsTokenInBody 覆盖浏览器 Cookie 通道：
// 仅凭 Cookie 刷新时同样在响应体回传轮换后的 refreshToken，并与新 Cookie 一致
// （小程序与浏览器共用同一响应结构，契约 §7.2）。
func TestPatientRefreshWithCookieRotatesAndReturnsTokenInBody(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		"",
		withRefreshCookie(login.Tokens.RefreshToken),
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil {
		t.Fatalf("刷新后必须下发 %s Cookie", authsession.RefreshCookieName)
	}
	if refreshCookie.Value == login.Tokens.RefreshToken {
		t.Error("Cookie 通道刷新必须轮换 refresh token，不得复用旧值")
	}
	if bodyToken := refreshResponseToken(t, body); bodyToken != refreshCookie.Value {
		t.Errorf(
			"响应体 refreshToken = %q，期望等于轮换后 refresh Cookie 的值 %q",
			bodyToken,
			refreshCookie.Value,
		)
	}
	parsePatientRFC3339(t, "refreshExpiresAt", body["refreshExpiresAt"])
}

// TestPatientRefreshPrefersBodyTokenOverCookie 覆盖两条通道同时携带有效令牌：
// 请求体优先，只轮换请求体令牌对应的会话；Cookie 中的旧会话必须保持可用且仍能继续刷新，
// 否则浏览器会被小程序提交的请求体令牌提前踢下线（契约 §7.2）。
func TestPatientRefreshPrefersBodyTokenOverCookie(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	cookieLogin := env.login(t) // 浏览器会话：只经 Cookie 提交
	bodyLogin := env.login(t)   // 小程序会话：经请求体提交

	cookieHash := authsession.HashRefreshToken(cookieLogin.Tokens.RefreshToken)
	bodyHash := authsession.HashRefreshToken(bodyLogin.Tokens.RefreshToken)
	cookieSession, ok := env.tokens.sessions[cookieHash]
	if !ok {
		t.Fatal("前置：Cookie 会话必须已写入")
	}
	bodySession, ok := env.tokens.sessions[bodyHash]
	if !ok {
		t.Fatal("前置：请求体会话必须已写入")
	}

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+bodyLogin.Tokens.RefreshToken+`"}`,
		withRefreshCookie(cookieLogin.Tokens.RefreshToken),
	)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	newToken := refreshResponseToken(t, body)
	newHash := authsession.HashRefreshToken(newToken)

	// 被轮换的必须是请求体那条会话：摘要与 sessionID 双向确认。
	if !containsString(env.tokens.deleted, bodyHash) {
		t.Errorf("请求体令牌对应的会话必须被轮换，实际 deleted=%v", env.tokens.deleted)
	}
	if containsString(env.tokens.deleted, cookieHash) {
		t.Errorf("请求体优先时不得轮换 Cookie 会话，实际 deleted=%v", env.tokens.deleted)
	}
	if !containsString(env.tokens.deletedSIDs, bodySession.SessionID) {
		t.Errorf(
			"轮换的 sessionID 必须来自请求体会话 %q，实际 %v",
			bodySession.SessionID,
			env.tokens.deletedSIDs,
		)
	}
	if containsString(env.tokens.deletedSIDs, cookieSession.SessionID) {
		t.Errorf(
			"Cookie 会话 %q 不得被轮换，实际 %v",
			cookieSession.SessionID,
			env.tokens.deletedSIDs,
		)
	}
	if _, ok := env.tokens.sessions[bodyHash]; ok {
		t.Error("请求体令牌对应的旧会话必须已撤销")
	}

	// 响应体回传的新令牌必须对应本次轮换产生的新会话，而不是 Cookie 会话。
	newSession, ok := env.tokens.sessions[newHash]
	if !ok {
		t.Fatalf("响应体 refreshToken 必须对应已写入的新会话，实际 sessions=%v", env.tokens.sessions)
	}
	if newSession.SessionID == cookieSession.SessionID {
		t.Error("响应体新令牌不得复用 Cookie 会话的 sessionID")
	}
	stillCookie, ok := env.tokens.sessions[cookieHash]
	if !ok {
		t.Fatal("Cookie 中的旧会话必须保持原状（未被轮换）")
	}
	if stillCookie.SessionID != cookieSession.SessionID {
		t.Errorf(
			"Cookie 会话必须保持原 sessionID %q，实际 %q",
			cookieSession.SessionID,
			stillCookie.SessionID,
		)
	}
	if newToken == cookieLogin.Tokens.RefreshToken {
		t.Error("响应体不得回传 Cookie 通道的旧令牌")
	}

	// Cookie 会话必须仍可用于刷新：由 Cookie 通道再刷新一次，能成功即证明它未被消费。
	w2 := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		"",
		withRefreshCookie(cookieLogin.Tokens.RefreshToken),
	)
	if w2.Code != http.StatusOK {
		t.Fatalf("Cookie 会话二次刷新 status = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	if !containsString(env.tokens.deleted, cookieHash) {
		t.Errorf("Cookie 会话二次刷新时必须轮换它，实际 deleted=%v", env.tokens.deleted)
	}
	// 二次刷新必须换出另一枚令牌，证明它来自 Cookie 会话而不是首次的请求体会话。
	if thirdToken := refreshResponseToken(t, decodeBody(t, w2)); thirdToken == newToken {
		t.Error("Cookie 会话二次刷新必须轮换出新令牌，不得复用首次刷新的令牌")
	}
}

// snapshotSessionHashes 复制桩里的 refresh 会话摘要集合，用于断言轮换前后的会话差异。
func snapshotSessionHashes(env *patientAuthHandlerEnv) map[string]struct{} {
	hashes := make(map[string]struct{}, len(env.tokens.sessions))
	for hash := range env.tokens.sessions {
		hashes[hash] = struct{}{}
	}
	return hashes
}

// assertDisabledRefreshForbidden 断言禁用账号刷新的公共语义：403 AUTH_FORBIDDEN、
// 不下发任何 Set-Cookie；并且除被轮换掉的那条会话外，轮换前的会话必须逐条保持原状
// （即没有写入新会话）。
//
// 服务层 Refresh 先 RotateRefreshSession 再校验 IsActive：403 分支下旧会话已被撤销、
// 且不会再签发新令牌，这是期望行为（见 patientauth.Service.Refresh 的对应注释）。
func assertDisabledRefreshForbidden(
	t *testing.T,
	env *patientAuthHandlerEnv,
	w *httptest.ResponseRecorder,
	before map[string]struct{},
	rotatedRefreshToken string,
) {
	t.Helper()

	body := assertErrorStatus(t, w, http.StatusForbidden, "AUTH_FORBIDDEN")
	if body["message"] != "账号已被禁用" {
		t.Errorf("message = %v，期望 账号已被禁用", body["message"])
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("禁用账号不得下发 Cookie，实际 %v", cookies)
	}

	rotatedHash := authsession.HashRefreshToken(rotatedRefreshToken)
	if !containsString(env.tokens.deleted, rotatedHash) {
		t.Errorf("被轮换的旧会话必须已撤销，实际 deleted=%v", env.tokens.deleted)
	}
	if _, ok := env.tokens.sessions[rotatedHash]; ok {
		t.Errorf("被轮换的旧会话不得继续存在，实际 sessions=%v", env.tokens.sessions)
	}
	for hash := range env.tokens.sessions {
		if _, existed := before[hash]; !existed {
			t.Errorf("禁用账号不得写入新会话，实际多出 %s", hash)
		}
	}
	for hash := range before {
		if hash == rotatedHash {
			continue
		}
		if _, ok := env.tokens.sessions[hash]; !ok {
			t.Errorf("未参与轮换的会话 %s 必须保持原状", hash)
		}
	}
}

// TestPatientRefreshDisabledAccountForbiddenWithBodyToken 覆盖「禁用账号 + 有效请求体令牌」：
// 403 AUTH_FORBIDDEN，不下发 Cookie 也不写新会话（契约 §7.2、§10）。
func TestPatientRefreshDisabledAccountForbiddenWithBodyToken(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)
	before := snapshotSessionHashes(env)
	// 登录后禁用账号：此时 refresh 会话仍然有效，用于覆盖「禁用 + 有效 refresh token」组合。
	env.repo.seedPatient(20, patient.StatusDisabled)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
		nil,
	)
	assertDisabledRefreshForbidden(t, env, w, before, login.Tokens.RefreshToken)
}

// TestPatientRefreshDisabledAccountForbiddenWithCookieToken 覆盖「禁用账号 + 有效 Cookie 令牌」：
// 与请求体通道相同的 403 语义，Cookie 既不被覆盖也不被清理（契约 §7.2、§10）。
func TestPatientRefreshDisabledAccountForbiddenWithCookieToken(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)
	before := snapshotSessionHashes(env)
	env.repo.seedPatient(20, patient.StatusDisabled)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		"",
		withRefreshCookie(login.Tokens.RefreshToken),
	)
	assertDisabledRefreshForbidden(t, env, w, before, login.Tokens.RefreshToken)
}

// TestPatientRefreshDisabledAccountDoesNotFallBackToCookie 覆盖「禁用账号 + 两条通道都携带有效令牌」
// （两个不同会话）：403 必须立即结束候选循环，Cookie 通道的会话不得被消费，
// 否则禁用账号的刷新会把浏览器那条仍然有效的会话一并踢掉（契约 §7.2、§10）。
func TestPatientRefreshDisabledAccountDoesNotFallBackToCookie(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	cookieLogin := env.login(t) // 浏览器会话：Cookie 通道
	bodyLogin := env.login(t)   // 小程序会话：请求体通道
	before := snapshotSessionHashes(env)
	env.repo.seedPatient(20, patient.StatusDisabled)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/refresh",
		`{"refreshToken":"`+bodyLogin.Tokens.RefreshToken+`"}`,
		withRefreshCookie(cookieLogin.Tokens.RefreshToken),
	)

	// 请求体优先：被轮换的只能是请求体那条会话，Cookie 会话必须原样保留。
	assertDisabledRefreshForbidden(t, env, w, before, bodyLogin.Tokens.RefreshToken)
	if env.tokens.getCalls != 1 {
		t.Errorf("403 分支不得继续尝试下一条通道，实际读取会话 %d 次", env.tokens.getCalls)
	}
}

// TestPatientRefreshTokenCandidatesSkipBlankAndPreferBody 直接覆盖候选收集规则（契约 §7.2）：
// 请求体优先、Cookie 备用；空值与纯空白来源不得进入候选列表。
// 服务层自身也会 TrimSpace 并短路，因此只有在这一层单测才能固定「空白请求体不产生候选」。
func TestPatientRefreshTokenCandidatesSkipBlankAndPreferBody(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		cookie string
		want   string // 候选用 | 连接，便于同时比较顺序与取值
	}{
		{
			name:   "请求体优先且两侧都去空白",
			body:   "  body-token  ",
			cookie: "cookie-token",
			want:   "body-token|cookie-token",
		},
		{
			name:   "纯空白请求体视为未携带",
			body:   "   ",
			cookie: "cookie-token",
			want:   "cookie-token",
		},
		{
			name:   "空请求体视为未携带",
			body:   "",
			cookie: "cookie-token",
			want:   "cookie-token",
		},
		{
			name:   "纯空白 Cookie 视为未携带",
			body:   "body-token",
			cookie: "   ",
			want:   "body-token",
		},
		{
			name:   "两条通道都为空时没有候选",
			body:   " ",
			cookie: "",
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(
				http.MethodPost,
				"/api/v1/patient/auth/refresh",
				nil,
			)
			if tc.cookie != "" {
				c.Request.AddCookie(&http.Cookie{
					Name:  authsession.RefreshCookieName,
					Value: tc.cookie,
				})
			}

			got := refreshTokenCandidates(tc.body, c)
			if joined := strings.Join(got, "|"); joined != tc.want {
				t.Errorf("refreshTokenCandidates = %q，期望 %q", joined, tc.want)
			}
		})
	}
}

// --- POST /api/v1/patient/auth/logout ---

// TestPatientLogoutWithoutTokenUnauthorized 覆盖严格语义：无 access token 时
// 401 AUTH_INVALID_TOKEN，文案按患者域给出（契约 §12.5）。
func TestPatientLogoutWithoutTokenUnauthorized(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(http.MethodPost, "/api/v1/patient/auth/logout", "", nil)

	body := assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
	if body["message"] != "患者访问令牌无效" {
		t.Errorf("message = %v，期望 患者访问令牌无效", body["message"])
	}
}

// TestPatientLogoutWithMisRealmTokenUnauthorized 覆盖 realm 不匹配：
// 管理端令牌调用患者接口返回 401 AUTH_INVALID_TOKEN。
func TestPatientLogoutWithMisRealmTokenUnauthorized(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/logout",
		"",
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+env.misAccessToken(t))
		},
	)

	body := assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
	if body["message"] != "患者访问令牌无效" {
		t.Errorf("message = %v，期望 患者访问令牌无效", body["message"])
	}
	if len(env.tokens.revoked) != 0 || len(env.tokens.deleted) != 0 {
		t.Errorf(
			"跨域登出不得产生副作用，revoked=%v deleted=%v",
			env.tokens.revoked,
			env.tokens.deleted,
		)
	}
}

// TestPatientLogoutWithValidTokenNoContentAndClearsCookies 覆盖成功登出：
// 204、撤销 jti 与同域 refresh 会话，并清理两个 Cookie。
func TestPatientLogoutWithValidTokenNoContentAndClearsCookies(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)
	claims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析患者 access token 失败：%v", err)
	}

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/logout",
		"",
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
			req.AddCookie(&http.Cookie{
				Name:  authsession.RefreshCookieName,
				Value: login.Tokens.RefreshToken,
			})
		},
	)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 响应不得带响应体，实际 %s", w.Body.String())
	}
	if !containsString(env.tokens.revoked, claims.ID) {
		t.Errorf("必须撤销 jti %q，实际 %v", claims.ID, env.tokens.revoked)
	}
	if !containsString(
		env.tokens.deleted,
		authsession.HashRefreshToken(login.Tokens.RefreshToken),
	) {
		t.Errorf("必须撤销 refresh 会话，实际 %v", env.tokens.deleted)
	}
	// 本用例同时提交了 refresh token，会话已按摘要撤销，按 sid 反查应记未命中；
	// 只带 access token 的路径见 TestPatientLogoutWithOnlyAccessTokenRevokesSession。
	if !containsString(env.tokens.sidMissed, claims.SessionID) {
		t.Errorf(
			"按 sid 反查应记未命中，实际 命中=%v 未命中=%v",
			env.tokens.sidDeleted,
			env.tokens.sidMissed,
		)
	}

	for _, name := range []string{
		authsession.AccessCookieName,
		authsession.RefreshCookieName,
	} {
		cookie := setCookieByName(w, name)
		if cookie == nil {
			t.Errorf("登出必须清理 %s Cookie", name)
			continue
		}
		if cookie.Value != "" || cookie.MaxAge >= 0 {
			t.Errorf("%s 应被清理（空值且 MaxAge<0），实际 %+v", name, cookie)
		}
	}
}

// TestPatientLogoutWithBodyRefreshTokenRevokesSession 覆盖小程序登出通道：
// 请求头携带有效 access token、请求体携带 refresh token 时必须 204、清理 Cookie，
// 且会话确实按请求体令牌的摘要撤销（getCalls 证明读了该令牌，而非只靠 claims.sid 兜底）
// （契约 §7.2、§12.5）。
func TestPatientLogoutWithBodyRefreshTokenRevokesSession(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)
	claims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析患者 access token 失败：%v", err)
	}

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/logout",
		`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
		},
	)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 响应不得带响应体，实际 %s", w.Body.String())
	}
	if env.tokens.getCalls != 1 {
		t.Errorf("必须按请求体令牌读取一次会话，实际 %d 次", env.tokens.getCalls)
	}
	refreshHash := authsession.HashRefreshToken(login.Tokens.RefreshToken)
	if !containsString(env.tokens.deleted, refreshHash) {
		t.Errorf("必须按请求体提交的 refresh token 撤销会话，实际 deleted=%v", env.tokens.deleted)
	}
	if _, ok := env.tokens.sessions[refreshHash]; ok {
		t.Error("登出后 refresh 会话不得继续存在")
	}
	if !containsString(env.tokens.revoked, claims.ID) {
		t.Errorf("必须撤销 access token 的 jti %q，实际 %v", claims.ID, env.tokens.revoked)
	}
	for _, name := range []string{
		authsession.AccessCookieName,
		authsession.RefreshCookieName,
	} {
		if cookie := setCookieByName(w, name); cookie == nil || cookie.Value != "" {
			t.Errorf("登出必须清理 %s Cookie，实际 %+v", name, cookie)
		}
	}
}

// TestPatientLogoutWithInvalidBodyTokenStillNoContent 覆盖候选失配的登出：
// 请求体 refresh token 无效、Cookie 有效时仍必须 204（登出幂等，不因候选失配失败），
// 会话最终被撤销即可——由摘要还是 claims.sid 完成不作要求（契约 §7.2、§12.5）。
func TestPatientLogoutWithInvalidBodyTokenStillNoContent(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)
	claims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析患者 access token 失败：%v", err)
	}

	w := env.request(
		http.MethodPost,
		"/api/v1/patient/auth/logout",
		`{"refreshToken":"unknown-refresh-token"}`,
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
			req.AddCookie(&http.Cookie{
				Name:  authsession.RefreshCookieName,
				Value: login.Tokens.RefreshToken,
			})
		},
	)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 响应不得带响应体，实际 %s", w.Body.String())
	}
	refreshHash := authsession.HashRefreshToken(login.Tokens.RefreshToken)
	if _, ok := env.tokens.sessions[refreshHash]; ok {
		t.Error("登出后 refresh 会话不得继续存在")
	}
	if !containsString(env.tokens.revoked, claims.ID) {
		t.Errorf("必须撤销 access token 的 jti %q，实际 %v", claims.ID, env.tokens.revoked)
	}
}

// --- GET /api/v1/patient/me ---

// TestPatientMeSuccess 覆盖已实名建卡：响应含 cardId/cardCount/tel，且不含 openId。
func TestPatientMeSuccess(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	env.repo.seedCard(20, "13800138000")
	login := env.login(t)

	w := env.request(
		http.MethodGet,
		"/api/v1/patient/me",
		"",
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
		},
	)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if id, ok := body["id"].(float64); !ok || id != 20 {
		t.Errorf("id = %v，期望 20", body["id"])
	}
	if cardID, ok := body["cardId"].(float64); !ok || cardID != 10 {
		t.Errorf("cardId = %v，期望 10", body["cardId"])
	}
	if count, ok := body["cardCount"].(float64); !ok || count != 1 {
		t.Errorf("cardCount = %v，期望 1", body["cardCount"])
	}
	if body["tel"] != "13800138000" {
		t.Errorf("tel = %v，期望明文 13800138000", body["tel"])
	}
	if body["status"] != "ACTIVE" {
		t.Errorf("status = %v，期望 ACTIVE", body["status"])
	}
	if _, exists := body["openId"]; exists {
		t.Error("me 响应不得包含 openId")
	}
}

// TestPatientMeWithoutCard 覆盖尚未实名建卡：cardId=null、cardCount=0、tel=null。
func TestPatientMeWithoutCard(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	w := env.request(
		http.MethodGet,
		"/api/v1/patient/me",
		"",
		func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
		},
	)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)

	if value, exists := body["cardId"]; !exists || value != nil {
		t.Errorf("cardId = %v（exists=%t），期望显式 null", value, exists)
	}
	if count, ok := body["cardCount"].(float64); !ok || count != 0 {
		t.Errorf("cardCount = %v，期望 0", body["cardCount"])
	}
	if value, exists := body["tel"]; !exists || value != nil {
		t.Errorf("tel = %v（exists=%t），期望显式 null", value, exists)
	}
}

// TestPatientMeUnauthorized 覆盖未认证分支：无令牌、无效令牌与 mis realm 令牌
// 一律 401 AUTH_INVALID_TOKEN，文案为「患者访问令牌无效」。
func TestPatientMeUnauthorized(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *http.Request)
	}{
		{
			name: "无令牌",
		},
		{
			name: "无效令牌",
			mutate: func(_ *testing.T, req *http.Request) {
				req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
			},
		},
		{
			name: "mis realm 令牌",
			mutate: func(t *testing.T, req *http.Request) {
				env := newPatientAuthHandlerEnv(t)
				req.Header.Set("Authorization", "Bearer "+env.misAccessToken(t))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthHandlerEnv(t)

			mutate := func(req *http.Request) {
				if tc.mutate != nil {
					tc.mutate(t, req)
				}
			}
			w := env.request(http.MethodGet, "/api/v1/patient/me", "", mutate)

			body := assertErrorStatus(t, w, http.StatusUnauthorized, "AUTH_INVALID_TOKEN")
			if body["message"] != "患者访问令牌无效" {
				t.Errorf("message = %v，期望 患者访问令牌无效", body["message"])
			}
		})
	}
}

// containsString 判断切片中是否包含目标值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TestPatientWeChatLoginTruncatedJSONReturnsInvalidJSON 覆盖截断 JSON：
// 解码返回 io.ErrUnexpectedEOF 时必须映射为 400 REQUEST_INVALID_JSON
// （契约 §10，与 schedule 写接口的判定一致），不得落到 422 的 code 文案。
func TestPatientWeChatLoginTruncatedJSONReturnsInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "只有一个左花括号", body: `{`},
		{name: "对象未闭合", body: `{"code":"wx_code_abc123"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthHandlerEnv(t)

			w := env.request(
				http.MethodPost,
				"/api/v1/patient/auth/wechat-login",
				tc.body,
				nil,
			)
			body := assertErrorStatus(t, w, http.StatusBadRequest, "REQUEST_INVALID_JSON")
			if body["message"] != "请求体不是合法的 JSON" {
				t.Errorf("message = %v，期望 请求体不是合法的 JSON", body["message"])
			}
		})
	}
}

// TestPatientRefreshMalformedJSONReturnsInvalidJSON 覆盖 refresh 请求体解析失败：
// 只有「空请求体」按缺省处理（继续走 Cookie 通道），其余解析错误必须返回
// 400 REQUEST_INVALID_JSON，不得静默降级成 401（契约 §10）。
func TestPatientRefreshMalformedJSONReturnsInvalidJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "截断 JSON", body: `{`},
		{name: "语法错误", body: `{"refreshToken":}`},
		{name: "字段类型错误", body: `{"refreshToken":123}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newPatientAuthHandlerEnv(t)

			w := env.request(
				http.MethodPost,
				"/api/v1/patient/auth/refresh",
				tc.body,
				nil,
			)
			assertErrorStatus(t, w, http.StatusBadRequest, "REQUEST_INVALID_JSON")
		})
	}
}

// TestPatientLogoutWithOnlyAccessTokenRevokesSession 覆盖只携带 access token 的登出：
// 服务端必须按 claims.sid 撤销 refresh 会话（契约 §7.2），响应 204 并清理 Cookie；
// 重复登出仍返回 204（会话已不存在，按未命中处理，保持幂等）。
func TestPatientLogoutWithOnlyAccessTokenRevokesSession(t *testing.T) {
	env := newPatientAuthHandlerEnv(t)
	login := env.login(t)

	claims, err := env.service.ParseAccessToken(
		login.Tokens.AccessToken,
		domainauth.RealmPatient,
	)
	if err != nil {
		t.Fatalf("解析患者 access token 失败：%v", err)
	}
	if claims.SessionID == "" {
		t.Fatal("access token 必须携带 sid")
	}

	bearer := func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		w := env.request(http.MethodPost, "/api/v1/patient/auth/logout", "", bearer)
		if w.Code != http.StatusNoContent {
			t.Fatalf(
				"第 %d 次登出 status = %d, want 204; body=%s",
				attempt,
				w.Code,
				w.Body.String(),
			)
		}
	}

	if !containsString(env.tokens.sidDeleted, claims.SessionID) {
		t.Errorf("必须按 claims.sid 撤销 refresh 会话，实际 %v", env.tokens.sidDeleted)
	}
	if !containsString(env.tokens.sidMissed, claims.SessionID) {
		t.Errorf("重复登出应按未命中处理，实际 %v", env.tokens.sidMissed)
	}
	if !containsString(env.tokens.revoked, claims.ID) {
		t.Errorf("必须撤销 access token 的 jti，实际 %v", env.tokens.revoked)
	}
	refreshHash := authsession.HashRefreshToken(login.Tokens.RefreshToken)
	if _, ok := env.tokens.sessions[refreshHash]; ok {
		t.Error("refresh 会话必须已被撤销")
	}
}

// TestPatientDependencyFailuresMapToBadGateway 覆盖依赖故障在四个患者端接口上的映射：
// login/refresh/logout/me 都必须返回 502 DEPENDENCY_UNAVAILABLE（契约 §10），
// 不能伪装成 401/500。
func TestPatientDependencyFailuresMapToBadGateway(t *testing.T) {
	t.Run("login 微信不可用", func(t *testing.T) {
		env := newPatientAuthHandlerEnv(t)
		env.wechat.err = port.ErrWeChatUnavailable

		w := env.request(
			http.MethodPost,
			"/api/v1/patient/auth/wechat-login",
			`{"code":"wx_code_abc123"}`,
			nil,
		)
		assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
	})

	t.Run("login 患者仓储不可用", func(t *testing.T) {
		env := newPatientAuthHandlerEnv(t)
		env.repo.findOrCreateErr = errors.New("postgres unavailable")

		w := env.request(
			http.MethodPost,
			"/api/v1/patient/auth/wechat-login",
			`{"code":"wx_code_abc123"}`,
			nil,
		)
		assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
	})

	t.Run("refresh 会话存储不可用", func(t *testing.T) {
		env := newPatientAuthHandlerEnv(t)
		login := env.login(t)
		env.tokens.getErr = errors.New("redis unavailable")

		w := env.request(
			http.MethodPost,
			"/api/v1/patient/auth/refresh",
			`{"refreshToken":"`+login.Tokens.RefreshToken+`"}`,
			nil,
		)
		assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
	})

	t.Run("logout 会话存储不可用", func(t *testing.T) {
		env := newPatientAuthHandlerEnv(t)
		login := env.login(t)
		env.tokens.sidErr = errors.New("redis unavailable")

		w := env.request(
			http.MethodPost,
			"/api/v1/patient/auth/logout",
			"",
			func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
			},
		)
		assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
	})

	t.Run("me 患者仓储不可用", func(t *testing.T) {
		env := newPatientAuthHandlerEnv(t)
		login := env.login(t)
		env.repo.findPatientErr = errors.New("postgres unavailable")

		w := env.request(
			http.MethodGet,
			"/api/v1/patient/me",
			"",
			func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+login.Tokens.AccessToken)
			},
		)
		assertErrorStatus(t, w, http.StatusBadGateway, "DEPENDENCY_UNAVAILABLE")
	})
}

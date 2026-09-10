// 患者端认证 HTTP 层单测：wechat-login / refresh / logout / me 的状态码、
// 错误码、Cookie 行为与响应字段（spec/04-api-contract.md §7.1-§7.3、§10、§12.5）。
//
// 用内存桩替换 port.PatientRepository / port.TokenRepository / port.WeChatAuthenticator，
// gin 处于 TestMode，不依赖 PostgreSQL、Redis 与微信网络。
package handler

import (
	"context"
	"errors"
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
}

func (w *patientAuthHandlerWeChat) Code2Session(
	_ context.Context,
	_ string,
) (string, error) {
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

// newPatientAuthHandlerEnv 构造与 router.go 一致的患者端认证路由：
// auth 三个接口自带凭据校验，只有 /patient/me 经过 realm=patient 的令牌中间件。
func newPatientAuthHandlerEnv(t *testing.T) *patientAuthHandlerEnv {
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

	h := NewPatientAuthHandler(service, false)
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
	text, ok := value.(string)
	if !ok || text == "" {
		t.Fatalf("accessExpiresAt = %v，期望非空 RFC3339 字符串", value)
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("accessExpiresAt = %q，无法按 RFC3339 解析：%v", text, err)
	}
	return parsed
}

// --- POST /api/v1/patient/auth/wechat-login ---

// TestPatientWeChatLoginSuccessResponseShape 覆盖登录成功：响应字段齐全、
// 不泄露 refresh token，且 refresh token 只经 HttpOnly Cookie 下发。
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

	// refresh token 只能出现在 Cookie 中。
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
	if _, exists := body["refreshToken"]; exists {
		t.Error("响应体不得包含 refreshToken 字段")
	}
	if strings.Contains(w.Body.String(), refreshCookie.Value) {
		t.Error("响应体不得包含 refresh token 原文")
	}

	accessCookie := setCookieByName(w, authsession.AccessCookieName)
	if accessCookie == nil || !accessCookie.HttpOnly {
		t.Errorf("缺少 HttpOnly 的 %s Cookie，实际 %+v", authsession.AccessCookieName, accessCookie)
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

// --- POST /api/v1/patient/auth/refresh ---

// TestPatientRefreshWithBodyToken 覆盖请求体携带 refresh token 的成功刷新：
// 轮换后的 refresh token 仍只经 Cookie 下发。
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
	if _, exists := body["refreshToken"]; exists {
		t.Error("响应体不得包含 refreshToken 字段")
	}

	refreshCookie := setCookieByName(w, authsession.RefreshCookieName)
	if refreshCookie == nil || !refreshCookie.HttpOnly {
		t.Fatalf("刷新后必须下发 HttpOnly 的 refresh Cookie，实际 %+v", refreshCookie)
	}
	if refreshCookie.Value == login.Tokens.RefreshToken {
		t.Error("刷新必须轮换 refresh token，不得复用旧值")
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

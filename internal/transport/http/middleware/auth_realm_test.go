package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

const realmMiddlewareTestSecret = "middleware-realm-test-secret"

// realmMiddlewareTokenRepo 只实现撤销查询，使中间件用例无需真实 Redis。
type realmMiddlewareTokenRepo struct {
	port.TokenRepository
	revoked bool
}

func (r realmMiddlewareTokenRepo) IsAccessTokenRevoked(
	_ context.Context,
	_ string,
) (bool, error) {
	return r.revoked, nil
}

// DeleteRefreshSessionBySessionID 让中间件测试桩满足 TokenRepository 的全部方法：
// 本桩不保存会话，因此按契约返回 (false, nil)，避免内嵌 nil 接口时调用即 panic。
func (r realmMiddlewareTokenRepo) DeleteRefreshSessionBySessionID(
	_ context.Context,
	_ string,
) (bool, error) {
	return false, nil
}

// signMiddlewareAccessToken 用中间件测试密钥签发指定 realm 的 access token；
// realm 传空字符串可模拟改造前未携带 realm 字段的旧令牌。
func signMiddlewareAccessToken(t *testing.T, realm domainauth.Realm) string {
	t.Helper()
	claims := &userservice.AccessClaims{
		UserID:    7,
		Username:  "mis-admin",
		TokenType: userservice.TokenTypeAccess,
		Realm:     realm,
		SessionID: "realm-middleware-session",
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        "realm-middleware-jti-" + string(realm),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(realmMiddlewareTestSecret))
	if err != nil {
		t.Fatalf("签发测试 access token 失败：%v", err)
	}
	return signed
}

// newMisRealmMiddlewareEnv 构造“RequireAccessToken(RealmMis) + 终点 handler”的最小引擎。
// 终点 handler 被调用即证明中间件放行；返回的 bool 指针用于断言是否到达后续处理。
func newMisRealmMiddlewareEnv(t *testing.T) (*gin.Engine, *bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	service := userservice.NewService(
		nil,
		realmMiddlewareTokenRepo{},
		userservice.Config{
			JWTSecret:  realmMiddlewareTestSecret,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: time.Hour,
		},
	)

	reached := false
	engine := gin.New()
	engine.Use(RequireAccessToken(service, domainauth.RealmMis))
	engine.GET("/mis-probe", func(c *gin.Context) {
		reached = true
		value, exists := c.Get(ClaimsKey)
		claims, ok := value.(*userservice.AccessClaims)
		if !exists || !ok || claims == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code": "TEST_CLAIMS_MISSING",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"realm": claims.Realm})
	})
	return engine, &reached
}

// performMisProbe 发起 /mis-probe 请求，mutate 用于附加 Authorization 头或 Cookie。
func performMisProbe(
	engine *gin.Engine,
	mutate func(*http.Request),
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/mis-probe", nil)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

// withBearer 返回为请求附加 Bearer 令牌的 mutate 函数。
func withBearer(token string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// decodeMisProbeBody 解析 /mis-probe 的响应体。
func decodeMisProbeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应体失败：%v body=%s", err, w.Body.String())
	}
	return body
}

// TestRequireAccessTokenRejectsPatientRealmTokenOnMisRoute 断言携带 realm=patient 的
// 合法令牌访问管理端路由时必须返回 401 AUTH_INVALID_TOKEN（绝不是 403），
// 且不得进入后续中间件/handler。
func TestRequireAccessTokenRejectsPatientRealmTokenOnMisRoute(t *testing.T) {
	engine, reached := newMisRealmMiddlewareEnv(t)

	patientToken := signMiddlewareAccessToken(t, domainauth.RealmPatient)
	w := performMisProbe(engine, withBearer(patientToken))

	if w.Code == http.StatusForbidden {
		t.Fatalf("患者域令牌访问管理端路由不得返回 403（说明 realm 未隔离），body=%s", w.Body.String())
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if code := decodeMisProbeBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
	}
	if *reached {
		t.Error("realm 不匹配的令牌不得进入后续 handler")
	}
}

// TestRequireAccessTokenRejectsPatientRealmCookie 断言 Cookie 备用通道同样按 realm 校验：
// medical_access_token 中携带患者域令牌访问管理端路由仍是 401。
func TestRequireAccessTokenRejectsPatientRealmCookie(t *testing.T) {
	engine, reached := newMisRealmMiddlewareEnv(t)

	patientToken := signMiddlewareAccessToken(t, domainauth.RealmPatient)
	w := performMisProbe(engine, func(req *http.Request) {
		req.AddCookie(&http.Cookie{
			Name:  userservice.AccessCookieName,
			Value: patientToken,
		})
	})

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if code := decodeMisProbeBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
	}
	if *reached {
		t.Error("realm 不匹配的 Cookie 令牌不得进入后续 handler")
	}
}

// TestRequireAccessTokenAcceptsMisRealmToken 断言 realm=mis 的合法令牌可放行，
// 并把含 realm 的 claims 写入上下文供后续中间件使用。
func TestRequireAccessTokenAcceptsMisRealmToken(t *testing.T) {
	engine, reached := newMisRealmMiddlewareEnv(t)

	misToken := signMiddlewareAccessToken(t, domainauth.RealmMis)
	w := performMisProbe(engine, withBearer(misToken))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !*reached {
		t.Fatal("realm=mis 的令牌应放行到后续 handler")
	}
	if realm := decodeMisProbeBody(t, w)["realm"]; realm != "mis" {
		t.Errorf("上下文 claims.realm = %v, want mis", realm)
	}
}

// TestRequireAccessTokenRejectsTokenWithoutRealm 断言缺少 realm 字段的旧 access token
// 在新契约下按无效令牌处理。
func TestRequireAccessTokenRejectsTokenWithoutRealm(t *testing.T) {
	engine, reached := newMisRealmMiddlewareEnv(t)

	w := performMisProbe(engine, withBearer(signMiddlewareAccessToken(t, "")))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if code := decodeMisProbeBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
	}
	if *reached {
		t.Error("缺少 realm 的令牌不得进入后续 handler")
	}
}

// TestRequireAccessTokenRejectsAnonymousRequest 回归：匿名请求仍返回 401。
func TestRequireAccessTokenRejectsAnonymousRequest(t *testing.T) {
	engine, reached := newMisRealmMiddlewareEnv(t)

	w := performMisProbe(engine, nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if *reached {
		t.Error("匿名请求不得进入后续 handler")
	}
}

// TestRequireAccessTokenRejectsRevokedMisRealmToken 回归：即使 realm 匹配，
// 已撤销（命中黑名单）的 access token 仍必须被拒绝。
func TestRequireAccessTokenRejectsRevokedMisRealmToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	service := userservice.NewService(
		nil,
		realmMiddlewareTokenRepo{revoked: true},
		userservice.Config{
			JWTSecret:  realmMiddlewareTestSecret,
			AccessTTL:  15 * time.Minute,
			RefreshTTL: time.Hour,
		},
	)

	reached := false
	engine := gin.New()
	engine.Use(RequireAccessToken(service, domainauth.RealmMis))
	engine.GET("/mis-probe", func(c *gin.Context) {
		reached = true
		c.Status(http.StatusOK)
	})

	w := performMisProbe(engine, withBearer(signMiddlewareAccessToken(t, domainauth.RealmMis)))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if code := decodeMisProbeBody(t, w)["code"]; code != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", code)
	}
	if reached {
		t.Error("已撤销的令牌不得进入后续 handler")
	}
}

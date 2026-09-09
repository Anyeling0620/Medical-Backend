package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// newAuthLogoutRouter 以 nil 仓库与 userservice 零值配置构造仅含 logout 路由的
// engine，用于对 AuthHandler.Logout 语义做单测，不触碰任何数据库/Redis。
func newAuthLogoutRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service := userservice.NewService(nil, nil, userservice.Config{})
	h := NewAuthHandler(service, false)
	e := gin.New()
	e.POST("/api/v1/mis/auth/logout", h.Logout)
	return e
}

// postLogout 发起 logout 请求；mutate 用于为请求附加 Cookie / Authorization 头。
func postLogout(e *gin.Engine, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mis/auth/logout", nil)
	if mutate != nil {
		mutate(req)
	}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// TestLogoutWithoutAnyTokenReturnsUnauthorized 无任何令牌时返回 401，
// 错误体 code 必须是 AUTH_INVALID_TOKEN。
func TestLogoutWithoutAnyTokenReturnsUnauthorized(t *testing.T) {
	e := newAuthLogoutRouter(t)
	w := postLogout(e, nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["code"] != "AUTH_INVALID_TOKEN" {
		t.Errorf("code = %v, want AUTH_INVALID_TOKEN", body["code"])
	}
	if body["message"] != "访问令牌无效" {
		t.Errorf("message = %v, want 访问令牌无效", body["message"])
	}
}

// TestLogoutWithRefreshCookieReturnsNoContent 携带 medical_refresh_token Cookie
// 时固定返回 204（access token 缺失也允许清理会话）。
func TestLogoutWithRefreshCookieReturnsNoContent(t *testing.T) {
	e := newAuthLogoutRouter(t)
	w := postLogout(e, func(req *http.Request) {
		req.AddCookie(&http.Cookie{
			Name:  userservice.RefreshCookieName,
			Value: "refresh-token-abc",
		})
	})

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// TestLogoutWithInvalidBearerAccessTokenReturnsNoContent 仅携带无效 Bearer
// access token 时按幂等成功处理，返回 204。
func TestLogoutWithInvalidBearerAccessTokenReturnsNoContent(t *testing.T) {
	e := newAuthLogoutRouter(t)
	w := postLogout(e, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer not-a-valid-jwt")
	})

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// TestLogoutWithRefreshCookieAndInvalidAccessTokenReturnsNoContent 契约要求：
// 只要携带 refresh cookie（即使 access 已过期/无效），logout 也必须返回 204。
func TestLogoutWithRefreshCookieAndInvalidAccessTokenReturnsNoContent(t *testing.T) {
	e := newAuthLogoutRouter(t)
	w := postLogout(e, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer expired-or-invalid-jwt")
		req.AddCookie(&http.Cookie{
			Name:  userservice.RefreshCookieName,
			Value: "refresh-token-abc",
		})
	})

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// TestLogoutWithInvalidAccessCookieReturnsNoContent 仅携带无效 access Cookie
// 时同样按幂等成功返回 204，不暴露令牌是否存在。
func TestLogoutWithInvalidAccessCookieReturnsNoContent(t *testing.T) {
	e := newAuthLogoutRouter(t)
	w := postLogout(e, func(req *http.Request) {
		req.AddCookie(&http.Cookie{
			Name:  userservice.AccessCookieName,
			Value: "not-a-valid-jwt",
		})
	})

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", w.Code, w.Body.String())
	}
}

// TestLogoutClearsCookiesOnSuccess 成功后应清理 access/refresh 两个 Cookie。
func TestLogoutClearsCookiesOnSuccess(t *testing.T) {
	e := newAuthLogoutRouter(t)
	w := postLogout(e, func(req *http.Request) {
		req.AddCookie(&http.Cookie{
			Name:  userservice.RefreshCookieName,
			Value: "refresh-token-abc",
		})
	})

	setCookies := w.Result().Cookies()
	if len(setCookies) != 2 {
		t.Fatalf("set-cookie count = %d, want 2; headers=%v", len(setCookies), w.Header())
	}
	cleared := map[string]bool{}
	for _, c := range setCookies {
		if c.MaxAge < 0 || c.Value == "" {
			cleared[c.Name] = true
		}
	}
	if !cleared[userservice.AccessCookieName] || !cleared[userservice.RefreshCookieName] {
		t.Errorf("成功 logout 必须清理 %s 与 %s，实际=%v", userservice.AccessCookieName, userservice.RefreshCookieName, setCookies)
	}
}

package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/transport/http/middleware"
	"Medical-Web-Backend/internal/transport/http/request"
	"Medical-Web-Backend/internal/transport/http/response"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

type AuthHandler struct {
	service *userservice.Service
	secure  bool
}

func NewAuthHandler(
	service *userservice.Service,
	secure bool,
) *AuthHandler {
	return &AuthHandler{
		service: service,
		secure:  secure,
	}
}

// Login 处理 POST /api/v1/mis/auth/login。
func (h *AuthHandler) Login(c *gin.Context) {
	var req request.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.bindError(c, err)
		return
	}

	result, err := h.service.Authenticate(
		c.Request.Context(),
		req.Username,
		req.Password,
	)
	if err != nil {
		h.loginError(c, err)
		return
	}

	h.setTokenCookies(c, result.Tokens)

	c.JSON(http.StatusOK, authResponse(result))
}

// Refresh 处理 POST /api/v1/mis/auth/refresh，refresh token 只从 HttpOnly Cookie 读取。
func (h *AuthHandler) Refresh(c *gin.Context) {
	refreshToken, err := c.Cookie(userservice.RefreshCookieName)
	if err != nil || refreshToken == "" {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_REFRESH_TOKEN", "刷新令牌无效或已过期")
		return
	}

	result, err := h.service.Refresh(
		c.Request.Context(),
		refreshToken,
	)
	if err != nil {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_REFRESH_TOKEN", "刷新令牌无效或已过期")
		return
	}

	h.setTokenCookies(c, result.Tokens)
	c.JSON(http.StatusOK, authResponse(result))
}

// Logout 处理 POST /api/v1/mis/auth/logout，成功固定返回 204 No Content。
// 不经过 RequireAccessToken：只要携带 refresh token，即使 access token 已过期
// 也能清理会话；access token 无效/已注销按幂等成功处理；两者都没有时返回 401。
func (h *AuthHandler) Logout(c *gin.Context) {
	accessToken := middleware.BearerToken(c.GetHeader("Authorization"))
	if accessToken == "" {
		accessToken, _ = c.Cookie(userservice.AccessCookieName)
	}

	refreshToken, _ := c.Cookie(
		userservice.RefreshCookieName,
	)

	// 优先用 refresh token 清理会话，忽略已过期或无效的 access token。
	if refreshToken != "" {
		if err := h.service.Logout(
			c.Request.Context(),
			"",
			refreshToken,
		); err != nil {
			h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "退出登录失败")
			return
		}
		h.clearTokenCookies(c)
		c.Status(http.StatusNoContent)
		return
	}

	if accessToken == "" {
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_TOKEN", "访问令牌无效")
		return
	}

	// 仅携带 access token 时，注销已失效 token 不报错，保证 logout 幂等。
	if err := h.service.Logout(
		c.Request.Context(),
		accessToken,
		"",
	); err != nil &&
		!errors.Is(err, userservice.ErrInvalidToken) {
		h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "退出登录失败")
		return
	}

	h.clearTokenCookies(c)
	c.Status(http.StatusNoContent)
}

func authResponse(
	result *userservice.LoginResult,
) response.LoginResponse {
	return response.LoginResponse{
		User: response.UserResponse{
			ID:           result.User.ID,
			Username:     result.User.Username,
			Name:         result.User.Name,
			DepartmentID: result.User.DepartmentID,
			Job:          result.User.Job,
		},
		Permissions:     result.Permissions,
		AccessExpiresAt: result.Tokens.AccessExpiresAt.UTC().Format(time.RFC3339),
	}
}

func (h *AuthHandler) setTokenCookies(
	c *gin.Context,
	pair userservice.TokenPair,
) {
	setCookie(
		c,
		userservice.AccessCookieName,
		pair.AccessToken,
		cookieAge(pair.AccessExpiresAt),
		h.secure,
	)

	setCookie(
		c,
		userservice.RefreshCookieName,
		pair.RefreshToken,
		cookieAge(pair.RefreshExpiresAt),
		h.secure,
	)
}

func (h *AuthHandler) clearTokenCookies(c *gin.Context) {
	setCookie(
		c,
		userservice.AccessCookieName,
		"",
		-1,
		h.secure,
	)

	setCookie(
		c,
		userservice.RefreshCookieName,
		"",
		-1,
		h.secure,
	)
}

func setCookie(
	c *gin.Context,
	name string,
	value string,
	maxAge int,
	secure bool,
) {
	cookie := (&http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}).String()

	c.Writer.Header().Add("Set-Cookie", cookie)
}

func cookieAge(expiresAt time.Time) int {
	seconds := int(time.Until(expiresAt).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

// bindError 区分 JSON 语法错误（400）与字段缺失/类型错误（422）。
func (h *AuthHandler) bindError(c *gin.Context, err error) {
	var syntaxError *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &syntaxError) ||
		errors.As(err, &typeError) {
		h.writeError(c, http.StatusBadRequest, "REQUEST_INVALID_JSON", "请求体不是合法的 JSON")
		return
	}
	h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", "用户名和密码不能为空")
}

// loginError 把认证失败映射为契约错误码；凭据/停用用户统一 401，不区分具体原因。
func (h *AuthHandler) loginError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userservice.ErrInvalidCredentials),
		errors.Is(err, userservice.ErrInactiveUser):
		h.writeError(c, http.StatusUnauthorized, "AUTH_INVALID_CREDENTIALS", "用户名或密码错误")
	case errors.Is(err, userservice.ErrInvalidInput):
		h.writeError(c, http.StatusUnprocessableEntity, "REQUEST_VALIDATION_FAILED", "用户名和密码不能为空")
	default:
		h.writeError(c, http.StatusInternalServerError, "INTERNAL_SERVER_ERROR", "登录失败，请稍后重试")
	}
}

func (h *AuthHandler) writeError(
	c *gin.Context,
	status int,
	code string,
	message string,
) {
	c.JSON(status, gin.H{
		"code":    code,
		"message": message,
	})
}

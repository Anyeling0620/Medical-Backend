package handler

import (
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

func (h *AuthHandler) Login(c *gin.Context) {
	var req request.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": userservice.ErrInvalidInput.Error(),
		})
		return
	}

	result, err := h.service.Authenticate(
		c.Request.Context(),
		req.Username,
		req.Password,
	)
	if err != nil {
		h.respondError(c, err)
		return
	}

	h.setTokenCookies(c, result.Tokens)

	c.JSON(http.StatusOK, authResponse(result))
}

func (h *AuthHandler) Refresh(c *gin.Context) {
	refreshToken, err := c.Cookie(userservice.RefreshCookieName)
	if err != nil || refreshToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": userservice.ErrInvalidToken.Error(),
		})
		return
	}

	result, err := h.service.Refresh(
		c.Request.Context(),
		refreshToken,
	)
	if err != nil {
		h.respondError(c, err)
		return
	}

	h.setTokenCookies(c, result.Tokens)
	c.JSON(http.StatusOK, authResponse(result))
}

func (h *AuthHandler) Logout(c *gin.Context) {
	accessToken := middleware.BearerToken(c.GetHeader("Authorization"))
	if accessToken == "" {
		accessToken, _ = c.Cookie(userservice.AccessCookieName)
	}

	refreshToken, _ := c.Cookie(
		userservice.RefreshCookieName,
	)

	err := h.service.Logout(
		c.Request.Context(),
		accessToken,
		refreshToken,
	)

	if err != nil &&
		!errors.Is(err, userservice.ErrInvalidToken) {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "logout failed",
		})
		return
	}

	h.clearTokenCookies(c)
	c.JSON(http.StatusOK, nil)
}

func authResponse(
	result *userservice.LoginResult,
) response.LoginResponse {
	return response.LoginResponse{
		User: response.UserResponse{
			ID:       result.User.ID,
			Username: result.User.Username,
		},
		Permissions: result.Permissions,
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

func (h *AuthHandler) respondError(
	c *gin.Context,
	err error,
) {
	status := http.StatusUnauthorized

	if errors.Is(err, userservice.ErrInvalidInput) {
		status = http.StatusBadRequest
	}

	c.JSON(status, gin.H{
		"error": err.Error(),
	})
}

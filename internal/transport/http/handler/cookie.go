package handler

// 本文件集中管理两个认证域共用的会话 Cookie 逻辑。
//
// 管理端（浏览器）与患者端（小程序）复用同一组 Cookie 名与安全属性
// （spec/04-api-contract.md §1.2）：小程序用请求头携带 access token、用请求体携带
// refresh token，浏览器两条通道都走 Cookie，因此属性只允许在这里定义一份。
// 患者端登录/刷新响应体额外回传 refresh token 供小程序保存（契约 §7.1、§7.2），
// 但 Cookie 仍是浏览器侧唯一的令牌载体。

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/usecase/authsession"
)

// setTokenCookies 写入 access/refresh 两个 HttpOnly Cookie。
// Cookie 是浏览器通道的令牌载体；小程序无法使用 Cookie，
// 因此调用方还会把轮换后的 refresh token 放进响应体（契约 §7.1、§7.2）。
func setTokenCookies(
	c *gin.Context,
	pair authsession.TokenPair,
	secure bool,
) {
	setCookie(
		c,
		authsession.AccessCookieName,
		pair.AccessToken,
		cookieAge(pair.AccessExpiresAt),
		secure,
	)

	setCookie(
		c,
		authsession.RefreshCookieName,
		pair.RefreshToken,
		cookieAge(pair.RefreshExpiresAt),
		secure,
	)
}

// clearTokenCookies 清除两个 Cookie：登出成功与幂等重复登出都走这里。
func clearTokenCookies(c *gin.Context, secure bool) {
	setCookie(c, authsession.AccessCookieName, "", -1, secure)
	setCookie(c, authsession.RefreshCookieName, "", -1, secure)
}

// setCookie 追加一个 HttpOnly、SameSite=Lax 的 Cookie。
// 生产环境由 JWT_COOKIE_SECURE 打开 Secure 属性。
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

// cookieAge 把过期时刻换算为 Max-Age，至少 1 秒，避免下发即刻过期的 Cookie。
func cookieAge(expiresAt time.Time) int {
	seconds := int(time.Until(expiresAt).Seconds())
	if seconds < 1 {
		return 1
	}
	return seconds
}

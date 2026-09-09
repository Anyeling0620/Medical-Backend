package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

const ClaimsKey = "auth.claims"

func RequireAccessToken(service *userservice.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var claims *userservice.AccessClaims

		// 优先读取 Authorization 请求头。
		// 如果请求头中的 token 无效，再尝试读取 Cookie 中的 access token。
		for _, rawToken := range accessTokenCandidates(c) {
			parsedClaims, err := service.ParseAccessToken(rawToken)
			if err != nil {
				continue
			}

			claims = parsedClaims
			break
		}

		if claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		// 检查 access token 是否已经被注销或加入 Redis 黑名单。
		revoked, err := service.IsRevoked(
			c.Request.Context(),
			claims.ID,
		)
		if err != nil || revoked {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

// accessTokenCandidates 返回请求中可能存在的 access token。
// Authorization 优先，Cookie 作为备用来源。
func accessTokenCandidates(c *gin.Context) []string {
	candidates := make([]string, 0, 2)

	// 读取：
	// Authorization: Bearer eyJ...
	if token := BearerToken(c.GetHeader("Authorization")); token != "" {
		candidates = append(candidates, token)
	}

	// 读取：
	// Cookie: medical_access_token=eyJ...
	if token, err := c.Cookie(userservice.AccessCookieName); err == nil {
		if token != "" {
			candidates = append(candidates, token)
		}
	}

	return candidates
}

func BearerToken(header string) string {
	parts := strings.Fields(header)

	if len(parts) == 2 &&
		strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}

	return ""
}

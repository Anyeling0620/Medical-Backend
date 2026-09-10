package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	domainauth "Medical-Web-Backend/internal/domain/auth"
	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// RequirePermissions checks whether the authenticated user has at least one
// permission from the allowed list.
// 按 spec/02-architecture.md「认证与令牌边界」，权限中间件必须先断言 realm=mis，
// 再按 claims 中的主体查询管理端权限表；禁止用患者主体查询管理端权限，
// 否则共享业务路由一旦漏挂 realm 门禁，患者 ID 可能命中同号管理用户而越权。
func RequirePermissions(
	userRepository port.UserRepository,
	allowed []string,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, exists := c.Get(ClaimsKey)
		claims, ok := value.(*userservice.AccessClaims)

		if !exists || !ok || claims == nil ||
			claims.Realm != domainauth.RealmMis {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    "AUTH_INVALID_TOKEN",
				"message": "访问令牌无效或已过期",
			})
			return
		}

		if userRepository == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "INTERNAL_SERVER_ERROR",
				"message": "权限校验失败",
			})
			return
		}

		permissions, err := userRepository.Permissions(
			c.Request.Context(),
			claims.UserID,
		)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "INTERNAL_SERVER_ERROR",
				"message": "权限校验失败",
			})
			return
		}

		if !hasAllowedPermission(permissions, allowed) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    "AUTH_FORBIDDEN",
				"message": "没有访问权限",
			})
			return
		}

		c.Next()
	}
}

func hasAllowedPermission(
	permissions []string,
	allowed []string,
) bool {
	allowedSet := make(map[string]struct{}, len(allowed))

	for _, permission := range allowed {
		allowedSet[permission] = struct{}{}
	}

	for _, permission := range permissions {
		if _, exists := allowedSet[permission]; exists {
			return true
		}
	}

	return false
}

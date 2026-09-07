package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/port"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

// RequirePermissions checks whether the authenticated user has at least one
// permission from the allowed list.
func RequirePermissions(
	userRepository port.UserRepository,
	allowed []string,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, exists := c.Get(ClaimsKey)
		claims, ok := value.(*userservice.AccessClaims)

		if !exists || !ok || claims == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": userservice.ErrInvalidToken.Error(),
			})
			return
		}

		if userRepository == nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "权限校验失败",
			})
			return
		}

		permissions, err := userRepository.Permissions(
			c.Request.Context(),
			claims.UserID,
		)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "权限校验失败",
			})
			return
		}

		if !hasAllowedPermission(permissions, allowed) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "没有访问权限",
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

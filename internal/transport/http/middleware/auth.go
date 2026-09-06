package middleware

import (
	"net/http"
	"strings"

	userservice "Medical-Web-Backend/internal/usecase/misuser"
	"github.com/gin-gonic/gin"
)

const ClaimsKey = "auth.claims"

func RequireAccessToken(service *userservice.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := bearerToken(c.GetHeader("Authorization"))
		if raw == "" {
			raw, _ = c.Cookie(userservice.AccessCookieName)
		}
		claims, err := service.ParseAccessToken(raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": userservice.ErrInvalidToken.Error()})
			return
		}
		revoked, err := service.IsRevoked(c.Request.Context(), claims.ID)
		if err != nil || revoked {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": userservice.ErrInvalidToken.Error()})
			return
		}
		c.Set(ClaimsKey, claims)
		c.Next()
	}
}

func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

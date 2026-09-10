package middleware

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
)

const (
	localhostCORSHost    = "localhost"
	localhostCORSMinPort = 0
	localhostCORSMaxPort = 65535
)

// AllowLocalhostFrontend allows browser requests from the local frontend dev
// servers while keeping credentialed CORS restricted to the expected ports.
func AllowLocalhostFrontend() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if !isAllowedLocalhostOrigin(origin) {
			c.Next()
			return
		}

		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Vary", "Origin")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func isAllowedLocalhostOrigin(origin string) bool {
	// 空 Origin 不处理
	if origin == "" {
		return false
	}

	parsed, err := url.ParseRequestURI(origin)
	if err != nil {
		return false
	}

	// 只检查 hostname 必须是 localhost
	if parsed.Hostname() != localhostCORSHost {
		return false
	}

	// 允许任何端口
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return false // 没有端口也不放行
	}

	return port >= localhostCORSMinPort && port <= localhostCORSMaxPort
}

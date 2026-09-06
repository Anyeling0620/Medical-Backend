package http

import "github.com/gin-gonic/gin"

// NewRouter creates the HTTP router used by the application bootstrap.
func NewRouter(health func() map[string]any) *gin.Engine {
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())
	router.SetTrustedProxies(nil)
	healthHandler := func(c *gin.Context) {
		c.JSON(200, health())
	}
	router.GET("/healthz", healthHandler)
	router.GET("/health", healthHandler)
	router.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"service": "medical-backend", "status": "ok"})
	})
	return router
}

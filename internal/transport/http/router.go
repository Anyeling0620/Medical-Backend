package http

import "github.com/gin-gonic/gin"

// NewRouter creates the HTTP router used by the application bootstrap.
func NewRouter() *gin.Engine {
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	return router
}

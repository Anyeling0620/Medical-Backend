package http

import (
	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/handler"
	userservice "Medical-Web-Backend/internal/usecase/user"
	"github.com/gin-gonic/gin"
	"log"
)

// NewRouter creates the HTTP router used by the application bootstrap.
func NewRouter(health func() map[string]any, _ config.Config, userRepository port.UserRepository) *gin.Engine {
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())
	err := router.SetTrustedProxies(nil)
	if err != nil {
		log.Printf("setup trusted proxies error: %v", err)
	}
	healthHandler := func(c *gin.Context) {
		c.JSON(200, health())
	}
	router.GET("/health", healthHandler)
	router.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"service": "medical-backend", "status": "ok"})
	})

	authHandler := handler.NewAuthHandler(userservice.NewService(userRepository))
	router.POST("/login", authHandler.Login)
	return router
}

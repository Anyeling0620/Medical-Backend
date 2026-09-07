package http

import (
	"Medical-Web-Backend/internal/transport/http/middleware"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/transport/http/handler"
	doctorservice "Medical-Web-Backend/internal/usecase/doctor"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
)

func NewRouter(
	health func() map[string]any,
	cfg config.Config,
	userRepository port.UserRepository,
	tokenRepository port.TokenRepository,
	doctorRepository port.DoctorRepository,
) *gin.Engine {
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())

	router.Use(middleware.AllowLocalhostFrontend())

	if err := router.SetTrustedProxies(nil); err != nil {
		log.Printf("setup trusted proxies error: %v", err)
	}

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, health())
	})

	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service": "medical-backend",
			"status":  "ok",
		})
	})

	service := userservice.NewService(
		userRepository,
		tokenRepository,
		userservice.Config{
			JWTSecret:    cfg.Auth.JWTSecret,
			AccessTTL:    cfg.Auth.AccessTTL,
			RefreshTTL:   cfg.Auth.RefreshTTL,
			CookieSecure: cfg.Auth.CookieSecure,
		},
	)

	authHandler := handler.NewAuthHandler(
		service,
		cfg.Auth.CookieSecure,
	)

	router.POST("/login", authHandler.Login)
	router.POST("/refresh", authHandler.Refresh)

	router.Use(middleware.RequireAccessToken(service))
	router.GET("/logout", authHandler.Logout)

	doctorHandler := handler.NewDoctorHandler(doctorservice.NewService(doctorRepository))
	router.GET("/doctor/search", doctorHandler.Search)
	router.GET("/doctor/searchCount", doctorHandler.SearchCount)

	return router
}

package http

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/repo"
	"Medical-Web-Backend/internal/transport/http/handler"
	"Medical-Web-Backend/internal/transport/http/middleware"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
	"Medical-Web-Backend/internal/utils"
)

func NewRouter(
	health func() map[string]any,
	cfg config.Config,
	userRepository port.UserRepository,
	tokenRepository port.TokenRepository,
	doctorRepository *repo.PostgresDoctorRepository,
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

	doctorHandler := handler.NewDoctorHandler(doctorRepository, utils.MinioPublicURL(cfg))
	catalogHandler := handler.NewCatalogHandler(doctorRepository, userRepository, utils.MinioPublicURL(cfg))

	router.GET("/depts", doctorHandler.ListDepts)
	router.GET("/degrees", doctorHandler.ListDegrees)
	router.GET("/jobs", doctorHandler.ListJobs)

	router.Use(middleware.RequireAccessToken(service))

	router.GET("/logout", authHandler.Logout)
	router.GET("/doctor/search", doctorHandler.Search)
	router.GET("/doctor/searchCount", doctorHandler.SearchCount)
	catalogRoutes := router.Group("/api/v1/catalog")
	catalogRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "CATALOG:SELECT"}))
	catalogRoutes.GET("/departments", catalogHandler.ListDepartments)
	catalogRoutes.GET("/departments/:departmentId", catalogHandler.Detail)
	catalogRoutes.GET("/departments/:departmentId/subdepartments", catalogHandler.Subdepartments)
	catalogRoutes.GET("/subdepartments/:subdepartmentId", catalogHandler.SubdepartmentDetail)
	catalogRoutes.GET("/doctors", catalogHandler.Doctors)
	catalogRoutes.GET("/doctors/options", catalogHandler.DoctorOptions)
	catalogRoutes.GET("/doctors/:doctorId", catalogHandler.DoctorDetail)
	catalogRoutes.GET("/doctor-prices", catalogHandler.DoctorPrices)

	router.GET(
		"/doctor/:id",
		middleware.RequirePermissions(
			userRepository,
			[]string{"ROOT", "DOCTOR:SELECT"},
		),
		doctorHandler.Detail,
	)

	return router
}

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
	scheduleservice "Medical-Web-Backend/internal/usecase/schedule"
	"Medical-Web-Backend/internal/utils"
)

func NewRouter(
	health func() map[string]any,
	cfg config.Config,
	userRepository port.UserRepository,
	tokenRepository port.TokenRepository,
	doctorRepository *repo.PostgresDoctorRepository,
	scheduleRepository port.PlanRepository,
	idempotencyStore port.IdempotencyStore,
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

	// 认证接口统一挂在 /api/v1/mis/auth 下，logout 不经过 RequireAccessToken，
	// 以便支持“access token 已过期但携带 refresh token”的清理场景。
	authRoutes := router.Group("/api/v1/mis/auth")
	authRoutes.POST("/login", authHandler.Login)
	authRoutes.POST("/refresh", authHandler.Refresh)
	authRoutes.POST("/logout", authHandler.Logout)

	catalogHandler := handler.NewCatalogHandler(doctorRepository, userRepository, utils.MinioPublicURL(cfg))

	scheduleService := scheduleservice.NewService(scheduleRepository, nil)
	scheduleHandler := handler.NewScheduleHandler(scheduleService, idempotencyStore)

	router.Use(middleware.RequireAccessToken(service))

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

	// 排班计划：查询走 SCHEDULE:SELECT，写操作走 SCHEDULE:WRITE（均允许 ROOT）。
	scheduleSelectRoutes := router.Group("/api/v1/schedule")
	scheduleSelectRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "SCHEDULE:SELECT"}))
	scheduleSelectRoutes.GET("/plans", scheduleHandler.ListPlans)

	scheduleWriteRoutes := router.Group("/api/v1/schedule")
	scheduleWriteRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "SCHEDULE:WRITE"}))
	scheduleWriteRoutes.POST("/plans", scheduleHandler.CreatePlan)
	scheduleWriteRoutes.PATCH("/plans/:planId", scheduleHandler.UpdatePlan)
	scheduleWriteRoutes.DELETE("/plans/:planId", scheduleHandler.DeletePlan)

	return router
}

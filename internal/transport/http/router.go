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
	"Medical-Web-Backend/internal/usecase/schedule"
	"Medical-Web-Backend/internal/utils"
)

func NewRouter(
	health func() map[string]any,
	cfg config.Config,
	userRepository port.UserRepository,
	tokenRepository port.TokenRepository,
	doctorRepository *repo.PostgresDoctorRepository,
	scheduleRepository *repo.PostgresScheduleRepository,
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
		c.JSON(http.StatusOK, gin.H{"service": "medical-backend", "status": "ok"})
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
	authHandler := handler.NewAuthHandler(service, cfg.Auth.CookieSecure)
	authRoutes := router.Group("/api/v1/mis/auth")
	authRoutes.POST("/login", authHandler.Login)
	authRoutes.POST("/refresh", authHandler.Refresh)
	authRoutes.POST("/logout", authHandler.Logout)
	catalogHandler := handler.NewCatalogHandler(doctorRepository, userRepository, utils.MinioPublicURL(cfg))
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

	// 排班时段接口：查询需 SCHEDULE:SELECT 或 ROOT，写操作需 SCHEDULE:WRITE 或 ROOT（契约第 5 章）。
	// POST 创建走幂等协调（store），PATCH/DELETE 不需要 Idempotency-Key。
	scheduleService := schedule.NewService(scheduleRepository, nil)
	scheduleSlotHandler := handler.NewScheduleSlotHandler(scheduleService, idempotencyStore)
	scheduleQuery := router.Group("/api/v1/schedule")
	scheduleQuery.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "SCHEDULE:SELECT"}))
	scheduleQuery.GET("/plans/:planId/slots", scheduleSlotHandler.ListSlots)
	scheduleWrite := router.Group("/api/v1/schedule")
	scheduleWrite.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "SCHEDULE:WRITE"}))
	scheduleWrite.POST("/plans/:planId/slots", scheduleSlotHandler.CreateSlot)
	scheduleWrite.PATCH("/slots/:slotId", scheduleSlotHandler.UpdateSlotMaximum)
	scheduleWrite.DELETE("/slots/:slotId", scheduleSlotHandler.DeleteSlot)

	return router
}

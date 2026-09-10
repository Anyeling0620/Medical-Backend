package http

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/config"
	domainauth "Medical-Web-Backend/internal/domain/auth"
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
	scheduleService := scheduleservice.NewService(scheduleRepository, nil)
	scheduleHandler := handler.NewScheduleHandler(scheduleService, idempotencyStore)
	scheduleSlotHandler := handler.NewScheduleSlotHandler(scheduleService, idempotencyStore)
	// 受保护接口按认证域分组挂载访问令牌校验，而不是挂在引擎级：
	// 引擎级中间件无法区分 realm，会让后续新增的 /api/v1/patient/* 被管理域校验误拦。
	// 管理端接口（/mis、/catalog、/schedule）只接受 realm=mis 的令牌。
	requireMisAccess := middleware.RequireAccessToken(
		service,
		domainauth.RealmMis,
	)

	catalogRoutes := router.Group("/api/v1/catalog")
	catalogRoutes.Use(requireMisAccess)
	catalogRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "CATALOG:SELECT"}))
	catalogRoutes.GET("/departments", catalogHandler.ListDepartments)
	catalogRoutes.GET("/departments/:departmentId", catalogHandler.Detail)
	catalogRoutes.GET("/departments/:departmentId/subdepartments", catalogHandler.Subdepartments)
	catalogRoutes.GET("/subdepartments/:subdepartmentId", catalogHandler.SubdepartmentDetail)
	catalogRoutes.GET("/doctors", catalogHandler.Doctors)
	catalogRoutes.GET("/doctors/options", catalogHandler.DoctorOptions)
	catalogRoutes.GET("/doctors/:doctorId", catalogHandler.DoctorDetail)
	catalogRoutes.GET("/doctor-prices", catalogHandler.DoctorPrices)

	// 排班：查询走 SCHEDULE:SELECT，写操作走 SCHEDULE:WRITE（均允许 ROOT）。
	// 计划接口在前，时段接口复用同一服务实例；两类接口共用 SCHEDULE:SELECT/WRITE 权限。
	scheduleSelectRoutes := router.Group("/api/v1/schedule")
	scheduleSelectRoutes.Use(requireMisAccess)
	scheduleSelectRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "SCHEDULE:SELECT"}))
	scheduleSelectRoutes.GET("/plans", scheduleHandler.ListPlans)
	scheduleSelectRoutes.GET("/plans/:planId/slots", scheduleSlotHandler.ListSlots)

	scheduleWriteRoutes := router.Group("/api/v1/schedule")
	scheduleWriteRoutes.Use(requireMisAccess)
	scheduleWriteRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT", "SCHEDULE:WRITE"}))
	scheduleWriteRoutes.POST("/plans", scheduleHandler.CreatePlan)
	scheduleWriteRoutes.PATCH("/plans/:planId", scheduleHandler.UpdatePlan)
	scheduleWriteRoutes.DELETE("/plans/:planId", scheduleHandler.DeletePlan)
	scheduleWriteRoutes.POST("/plans/:planId/slots", scheduleSlotHandler.CreateSlot)
	scheduleWriteRoutes.PATCH("/slots/:slotId", scheduleSlotHandler.UpdateSlotMaximum)
	scheduleWriteRoutes.DELETE("/slots/:slotId", scheduleSlotHandler.DeleteSlot)

	return router
}

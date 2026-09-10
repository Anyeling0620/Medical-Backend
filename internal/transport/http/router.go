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
	patientauthservice "Medical-Web-Backend/internal/usecase/patientauth"
	patientcardservice "Medical-Web-Backend/internal/usecase/patientcard"
	publiccatalogservice "Medical-Web-Backend/internal/usecase/publiccatalog"
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
	patientRepository port.PatientRepository,
	wechatAuthenticator port.WeChatAuthenticator,
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

	// 患者端认证：wechat-login/refresh/logout 自带凭据校验，不经过访问令牌中间件；
	// 只有 GET /patient/me 需要 realm=patient 的访问令牌。
	// 两个域的令牌共用同一 Redis 会话层，隔离只由 realm 决定。
	patientService := patientauthservice.NewService(
		patientRepository,
		tokenRepository,
		wechatAuthenticator,
		patientauthservice.Config{
			JWTSecret:  cfg.Auth.JWTSecret,
			AccessTTL:  cfg.Auth.AccessTTL,
			RefreshTTL: cfg.Auth.RefreshTTL,
		},
	)
	patientAuthHandler := handler.NewPatientAuthHandler(
		patientService,
		cfg.Auth.CookieSecure,
	)
	patientAuthRoutes := router.Group("/api/v1/patient/auth")
	patientAuthRoutes.POST("/wechat-login", patientAuthHandler.WeChatLogin)
	patientAuthRoutes.POST("/refresh", patientAuthHandler.Refresh)
	patientAuthRoutes.POST("/logout", patientAuthHandler.Logout)

	requirePatientAccess := middleware.RequireAccessToken(
		patientService,
		domainauth.RealmPatient,
	)
	patientRoutes := router.Group("/api/v1/patient")
	patientRoutes.Use(requirePatientAccess)
	patientRoutes.GET("/me", patientAuthHandler.Me)

	// 就诊卡：主体只能是令牌中的当前患者，cardId 只用于定位资源，
	// 他人卡与不存在的卡统一 404（spec/04-api-contract.md §7.4、§12.5）。
	// 与 /patient/me 一样复用 realm=patient 的访问令牌校验，不额外要求管理端权限。
	patientCardHandler := handler.NewPatientCardHandler(
		patientcardservice.NewService(patientRepository),
	)
	patientRoutes.GET("/cards", patientCardHandler.List)
	patientRoutes.POST("/cards", patientCardHandler.Create)
	patientRoutes.GET("/cards/:cardId", patientCardHandler.Detail)
	patientRoutes.PATCH("/cards/:cardId", patientCardHandler.Update)

	// 公开查询域（/api/v1/public/*）：匿名只读，不读取也不要求令牌，
	// 因此不挂 RequireAccessToken / RequirePermissions：携带无效或跨域令牌也必须正常返回
	// （测试策略「匿名公开域」）。数据可见性与字段裁剪由 publiccatalog 用例与公开域 repository 保证。
	publicService := publiccatalogservice.NewService(doctorRepository, scheduleRepository, nil)
	publicHandler := handler.NewPublicCatalogHandler(publicService, utils.MinioPublicURL(cfg))
	publicRoutes := router.Group("/api/v1/public")
	publicRoutes.GET("/departments", publicHandler.ListDepartments)
	publicRoutes.GET("/departments/:departmentId", publicHandler.DepartmentDetail)
	publicRoutes.GET("/departments/:departmentId/subdepartments", publicHandler.Subdepartments)
	publicRoutes.GET("/doctors", publicHandler.Doctors)
	publicRoutes.GET("/doctors/:doctorId", publicHandler.DoctorDetail)
	publicRoutes.GET("/schedules", publicHandler.Schedules)

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

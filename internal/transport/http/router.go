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
	doctorpatientservice "Medical-Web-Backend/internal/usecase/doctorpatient"
	medicalrecordservice "Medical-Web-Backend/internal/usecase/medical_record"
	userservice "Medical-Web-Backend/internal/usecase/misuser"
	patientauthservice "Medical-Web-Backend/internal/usecase/patientauth"
	patientcardservice "Medical-Web-Backend/internal/usecase/patientcard"
	paymentservice "Medical-Web-Backend/internal/usecase/payment"
	publiccatalogservice "Medical-Web-Backend/internal/usecase/publiccatalog"
	registrationservice "Medical-Web-Backend/internal/usecase/registration"
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
	publicScheduleRepository port.PublicScheduleRepository,
	scheduleCacheVersioner port.ScheduleCacheVersioner,
	idempotencyStore port.IdempotencyStore,
	patientRepository port.PatientRepository,
	wechatAuthenticator port.WeChatAuthenticator,
	registrationRepository port.RegistrationRepository,
	paymentRepository port.PaymentRepository,
	alipayGateway port.AlipayGateway,
	medicalRecordRepository port.MedicalRecordRepository,
	doctorPatientRepository port.DoctorPatientRepository,
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
		// openid 直通登录（跳过微信 code2Session）只是测试阶段的便利通道：
		// 只有 APP_ENV=development 才放行，其余环境一律只能走 code 换取（契约 §7.1）。
		cfg.App.Env == "development",
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
	// 时段查询走 T4b 的读缓存装饰器；未注入（测试、缓存关闭或 Redis 不可用）时回退到直查仓储，
	// 保证两条路径的响应形状一致、行为与改动前一致。
	if publicScheduleRepository == nil {
		publicScheduleRepository = scheduleRepository
	}
	publicService := publiccatalogservice.NewService(doctorRepository, publicScheduleRepository, nil)
	publicHandler := handler.NewPublicCatalogHandler(publicService, utils.MinioPublicURL(cfg))
	publicRoutes := router.Group("/api/v1/public")
	publicRoutes.GET("/departments", publicHandler.ListDepartments)
	publicRoutes.GET("/departments/:departmentId", publicHandler.DepartmentDetail)
	publicRoutes.GET("/departments/:departmentId/subdepartments", publicHandler.Subdepartments)
	publicRoutes.GET("/doctors", publicHandler.Doctors)
	publicRoutes.GET("/doctors/:doctorId", publicHandler.DoctorDetail)
	publicRoutes.GET("/schedules", publicHandler.Schedules)

	catalogHandler := handler.NewCatalogHandler(doctorRepository, userRepository, utils.MinioPublicURL(cfg))
	// 排班写路径通过端口递增版本号使公开排班缓存失效，usecase 不直接依赖 Redis（T4b）。
	scheduleService := scheduleservice.NewService(scheduleRepository, nil,
		scheduleservice.WithScheduleCacheVersioner(scheduleCacheVersioner))
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

	// 挂号域（/api/v1/registrations/*）是双 realm 共享业务接口
	// （spec/04-api-contract.md §1.2、§6）：管理端令牌必须带权限编码，
	// 患者端令牌只能操作本人就诊卡名下的资源，越权与不存在统一按资源不存在处理。
	//
	// 因此这里不能复用单一 realm 的 RequireAccessToken：
	//   - RequireSharedAccess 同时接受 patient/mis 两种 realm 的访问令牌；
	//   - RequirePermissionOrPatient 对管理端令牌校验权限编码，对患者令牌放行并由用例按 404 隐藏越权。
	// 权限编码以数据库 mis_permission 已有记录为准（契约 §1.2）：SELECT 用 REGISTRATION:SELECT，
	// 建单同时接受规格约定的 REGISTRATION:WRITE 与库中实际存在的 REGISTRATION:INSERT。
	// 支付用例先于挂号用例构造：建单流程（契约 §6.2）在建单事务提交后调用同一段预下单模块，
	// 因此挂号用例需要注入由支付用例实现的 port.PaymentPrecreator（禁止各写一套预下单口径）。
	paymentService := paymentservice.NewService(
		paymentRepository,
		alipayGateway,
		paymentservice.Config{
			PaymentSubject:   cfg.Alipay.Subject,
			NotifyURL:        cfg.Alipay.NotifyURL,
			LogDroppedNotify: cfg.App.Env == "development",
		},
	)
	registrationService := registrationservice.NewService(
		registrationRepository,
		patientRepository,
		patientRepository,
		paymentService,
		nil,
		registrationservice.WithScheduleCacheVersioner(scheduleCacheVersioner),
	)
	registrationHandler := handler.NewRegistrationHandler(registrationService, idempotencyStore)
	requireRegistrationSelect := middleware.RequirePermissionOrPatient(
		userRepository,
		[]string{"ROOT", "REGISTRATION:SELECT"},
	)
	requireRegistrationWrite := middleware.RequirePermissionOrPatient(
		userRepository,
		[]string{"ROOT", "REGISTRATION:WRITE", "REGISTRATION:INSERT"},
	)
	registrationRoutes := router.Group("/api/v1/registrations")
	registrationRoutes.Use(middleware.RequireSharedAccess(patientService, service))
	registrationRoutes.POST("/eligibility", requireRegistrationSelect, registrationHandler.Eligibility)
	registrationRoutes.POST("", requireRegistrationWrite, registrationHandler.Create)
	registrationRoutes.GET("", requireRegistrationSelect, registrationHandler.List)
	registrationRoutes.GET("/:registrationId", requireRegistrationSelect, registrationHandler.Detail)

	// 支付域（/api/v1/payments/*）同为双 realm 共享业务接口（契约 §1.2、§6.5–§6.7）。
	//
	// 支付宝异步通知入口必须公网可达、且不得落在统一鉴权中间件之后：它不读取任何用户
	// token，信任来源只有 RSA2 验签，因此单独挂在引擎级路由上（契约 §6.7 部署约束）。
	// 其余两个接口接受 patient/mis 两种 realm，权限编码用读取类 PAYMENT:SELECT
	// （契约 §6.5 已把取支付参数降级为幂等读取语义）。
	// paymentService 已在挂号域之前构造（建单流程需要注入预下单能力）。
	paymentHandler := handler.NewPaymentHandler(paymentService)
	router.POST("/api/v1/payments/alipay/notify", paymentHandler.Notify)

	// 创建支付订单接口（POST /api/v1/payments/orders）是支付域的管理端专用例外：
	// 只有 ROOT 权限码可以直接调用；患者令牌在令牌校验阶段被拒（realm 不匹配返回 401），
	// 其他管理端用户在权限校验阶段被拒（403）。建单流程（契约 §6.2）在服务内部调用
	// 同一段用例创建支付订单，不经过本接口（契约 §1.2、§6.9）。
	paymentAdminRoutes := router.Group("/api/v1/payments")
	paymentAdminRoutes.Use(requireMisAccess)
	paymentAdminRoutes.Use(middleware.RequirePermissions(userRepository, []string{"ROOT"}))
	paymentAdminRoutes.POST("/orders", paymentHandler.Create)

	paymentRoutes := router.Group("/api/v1/payments")
	paymentRoutes.Use(middleware.RequireSharedAccess(patientService, service))
	requirePaymentSelect := middleware.RequirePermissionOrPatient(
		userRepository,
		[]string{"ROOT", "PAYMENT:SELECT"},
	)
	paymentRoutes.POST("", requirePaymentSelect, paymentHandler.Read)
	paymentRoutes.GET("/:outTradeNo", requirePaymentSelect, paymentHandler.Detail)

	// 病历域（/api/v1/medical-records/*）只服务管理端令牌（realm=mis）：
	// 医生只能书写「自己负责的挂号记录」下的病历——用例按令牌主体查 mis_user.ref_id 得到
	// 医生编号，越权与不存在统一 404；未绑定医生身份的账号（含 ROOT）一律 403
	// AUTH_FORBIDDEN，数据范围绝不退化为全量（契约 §1.2、§6.10 与病历一节）。
	//
	// 权限码沿用「模块:动作」约定新建 MEDICAL_RECORD:SELECT/INSERT/UPDATE/DELETE，
	// 这里刻意不把 ROOT 放进放行名单：在数据库维护者把这些权限记录写入 mis_permission
	// 并授权给医生角色之前，任何账号（含 ROOT）都会在权限中间件被 403 拦下（建库 SQL 见契约 §13.3）。
	medicalRecordService := medicalrecordservice.NewService(medicalRecordRepository)
	medicalRecordHandler := handler.NewMedicalRecordHandler(medicalRecordService, idempotencyStore)
	medicalRecordRoutes := router.Group("/api/v1/medical-records")
	medicalRecordRoutes.Use(requireMisAccess)
	medicalRecordRoutes.GET("", middleware.RequirePermissions(
		userRepository, []string{"MEDICAL_RECORD:SELECT"}), medicalRecordHandler.List)
	medicalRecordRoutes.GET("/:medicalRecordId", middleware.RequirePermissions(
		userRepository, []string{"MEDICAL_RECORD:SELECT"}), medicalRecordHandler.Detail)
	medicalRecordRoutes.POST("", middleware.RequirePermissions(
		userRepository, []string{"MEDICAL_RECORD:INSERT"}), medicalRecordHandler.Create)
	medicalRecordRoutes.PATCH("/:medicalRecordId", middleware.RequirePermissions(
		userRepository, []string{"MEDICAL_RECORD:UPDATE"}), medicalRecordHandler.Update)
	medicalRecordRoutes.DELETE("/:medicalRecordId", middleware.RequirePermissions(
		userRepository, []string{"MEDICAL_RECORD:DELETE"}), medicalRecordHandler.Delete)
	// 医生工作台（/api/v1/mis/doctor/*）：只有登录主体在 mis_user.ref_id 上绑定了
	// doctor.id 的账号才能查看，且只能看到自己接诊过的患者——数据范围由用例按
	// 令牌主体推导，与客户端提交的参数无关（契约 §1.2、§6.10）。
	// 权限编码复用挂号读取权限 REGISTRATION:SELECT（库中「医生」角色已具备），
	// ROOT 作为超级权限一并放行。
	doctorPatientHandler := handler.NewDoctorPatientHandler(
		doctorpatientservice.NewService(userRepository, doctorPatientRepository),
	)
	doctorPatientRoutes := router.Group("/api/v1/mis/doctor")
	doctorPatientRoutes.Use(requireMisAccess)
	doctorPatientRoutes.Use(middleware.RequirePermissions(
		userRepository,
		[]string{"ROOT", "REGISTRATION:SELECT"},
	))
	doctorPatientRoutes.GET("/patients", doctorPatientHandler.List)

	return router
}

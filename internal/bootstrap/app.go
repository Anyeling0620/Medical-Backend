package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"Medical-Web-Backend/internal/config"
	"Medical-Web-Backend/internal/port"
	"Medical-Web-Backend/internal/repo"
	httptransport "Medical-Web-Backend/internal/transport/http"
	paymentservice "Medical-Web-Backend/internal/usecase/payment"
	"Medical-Web-Backend/internal/worker"
	"github.com/gin-gonic/gin"
)

type App struct {
	cfg          config.Config
	server       *gin.Engine
	clients      clients
	dependencies map[string]dependencyState
	// orderExpiryWorker 是订单过期收口任务的调度器；开关关闭或 PostgreSQL 不可用时为 nil
	// （此时 bootstrap 会输出告警：未付款订单占用的号源不会被自动释放）。
	orderExpiryWorker *worker.OrderExpiryWorker
}

func NewApp(cfg config.Config) (*App, error) {
	if cfg.HTTP.Port < 1 || cfg.HTTP.Port > 65535 {
		return nil, fmt.Errorf("HTTP_PORT must be between 1 and 65535")
	}

	// 生产环境必须配置微信登录凭据：openid 只能由微信签发，
	// 缺少凭据时患者登录不可用，属于启动期配置错误而不是运行期偶发故障。
	if cfg.App.Env == "production" &&
		(cfg.WeChat.AppID == "" || cfg.WeChat.Secret == "") {
		return nil, fmt.Errorf(
			"WECHAT_APPID and WECHAT_SECRET must be configured in production",
		)
	}

	// 判断是否为生产环境
	if cfg.App.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	// APP_ENV 的缺省值就是 development（含变量为空串的情形），而 development 会放行
	// 「直接提交 openid 登录」（见 internal/transport/http/router.go）：
	// 启动时显式提示，避免非本地环境漏配 APP_ENV 时被静默启用。
	if cfg.App.Env == "development" {
		log.Printf("警告：APP_ENV=development，患者登录允许直接提交 openid，仅供本地与测试环境使用")
	}
	// 支付宝凭据缺失时支付接口返回 502 PAYMENT_PROVIDER_UNAVAILABLE：
	// 与微信凭据不同，这不是启动期致命错误（其它业务域仍可正常工作），因此只提示。
	if cfg.Alipay.AppID == "" || cfg.Alipay.PrivateKey == "" {
		log.Printf("警告：未配置 ALIPAY_APP_ID/ALIPAY_APP_PRIVATE_SECRET，支付接口将返回 PAYMENT_PROVIDER_UNAVAILABLE")
	}
	// 公钥缺失只影响「验签」这条信任链：异步通知会直接返回 502，同步响应验签会被跳过，
	// 两条路径的降级结果不同，因此与私钥缺失分开提示，避免漏配时无法定位。
	if cfg.Alipay.PublicKey == "" {
		log.Printf("警告：未配置 ALIPAY_PUBLIC_SECRET，支付宝异步通知验签与同步响应验签将不可用")
	}
	if cfg.Alipay.NotifyURL == "" {
		log.Printf("提示：未配置 NOTIFY_URL，支付宝预下单不传 notify_url，支付状态降级为主动查询模式")
	} else if !isHTTPSNotifyURL(cfg.Alipay.NotifyURL) {
		// 契约 §6.7 要求通知地址必须是 HTTPS 且不带查询参数；
		// 不满足时支付宝可能拒收/不下发通知，这里只提示，不阻断启动（其它业务域仍可工作）。
		log.Printf("警告：NOTIFY_URL 必须是 HTTPS 且不带查询参数，当前值可能无法收到支付宝异步通知：%s", cfg.Alipay.NotifyURL)
	}
	connectedClients, dependencies := connectDependencies(cfg)
	for name, dependency := range dependencies {
		if dependency.Connected {
			log.Printf("dependency %s connected", name)
		} else {
			log.Printf("dependency %s unavailable: %s", name, dependency.Error)
		}
	}
	// 支付仓储与支付宝网关先构造出来：HTTP 路由与订单收口任务共用同一批无状态实例，
	// 避免两处各建一套配置（收口任务的状态迁移与通知/主动查询共用同一段实现，见 §6.8）。
	paymentRepository := repo.NewPostgresPaymentRepository(connectedClients.postgres)
	alipayGateway := repo.NewAlipayGateway(repo.AlipayOptions{
		AppID:      cfg.Alipay.AppID,
		PrivateKey: cfg.Alipay.PrivateKey,
		PublicKey:  cfg.Alipay.PublicKey,
		GatewayURL: cfg.Alipay.GatewayURL,
		SellerIDs:  cfg.Alipay.SellerIDList(),
		Timeout:    cfg.Alipay.Timeout,
	})

	// 公开域目录缓存：只包住公开读路径（/api/v1/public/* 的科室、子科室、医生），
	// 管理端 catalog 域继续用原始仓储读最新数据。Redis 未连接时不安装装饰器，
	// 避免把「持有 nil 指针的接口值」传进缓存层。
	catalogSource := repo.NewPostgresDoctorRepository(connectedClients.postgres)
	var publicCatalogRepository port.PublicCatalogRepository = catalogSource
	if cfg.PublicCatalogCache.Enabled && connectedClients.redis != nil {
		cachedCatalog, cacheErr := repo.NewCachedPublicCatalogRepository(
			catalogSource,
			connectedClients.redis,
			repo.PublicCatalogCacheOptions{
				Enabled:     cfg.PublicCatalogCache.Enabled,
				TTL:         cfg.PublicCatalogCache.TTL,
				JitterRatio: cfg.PublicCatalogCache.Jitter,
			},
		)
		if cacheErr != nil {
			return nil, cacheErr
		}
		publicCatalogRepository = cachedCatalog
	}
	server := httptransport.NewRouter(func() map[string]any {
		status := "ok"
		for _, dependency := range dependencies {
			if !dependency.Connected {
				status = "degraded"
				break
			}
		}
		return map[string]any{"status": status, "dependencies": dependencies}
	}, cfg,
		repo.NewPostgresUserRepository(connectedClients.postgres),
		repo.NewRedisTokenRepository(connectedClients.redis),
		catalogSource,
		publicCatalogRepository,
		repo.NewPostgresScheduleRepository(connectedClients.postgres),
		repo.NewRedisIdempotencyStore(connectedClients.redis),
		repo.NewPostgresPatientRepository(connectedClients.postgres),
		repo.NewWeChatCode2SessionClient(cfg.WeChat.AppID, cfg.WeChat.Secret),
		repo.NewPostgresRegistrationRepository(connectedClients.postgres),
		paymentRepository,
		alipayGateway,
		repo.NewPostgresMedicalRecordRepository(connectedClients.postgres),
		repo.NewPostgresDoctorPatientRepository(connectedClients.postgres),
	)

	app := &App{cfg: cfg, server: server, clients: connectedClients, dependencies: dependencies}
	app.orderExpiryWorker = newOrderExpiryWorker(cfg, connectedClients.postgres, paymentRepository, alipayGateway)
	return app, nil
}

// newOrderExpiryWorker 构造订单过期收口任务：扫描已过 expire_at 且仍未付款的订单，
// 先 alipay.trade.query 补记 PAID、必要时 alipay.trade.cancel 关单，再在收口事务里置
// EXPIRED 并释放计划级与时段级号源（契约 §6.8、创建订单与支付业务说明.md 第 7 节）。
//
// 三种情况不启动任务，并且都必须告警：任务关闭、PostgreSQL 不可用、或其业务依赖缺失。
// 不启动的后果是未付款订单会一直占用号源、判重也会拒绝重新挂号，属于已知并被接受的运行风险
// （本期不实现读路径惰性判定与失活告警）。
func newOrderExpiryWorker(
	cfg config.Config,
	postgres *sql.DB,
	paymentRepository port.PaymentRepository,
	alipayGateway *repo.AlipayGateway,
) *worker.OrderExpiryWorker {
	if !cfg.Worker.OrderExpiryEnabled {
		log.Printf("警告：ORDER_EXPIRY_WORKER_ENABLED=false，订单过期收口任务已关闭，" +
			"未付款订单占用的号源不会被释放")
		return nil
	}
	if postgres == nil {
		log.Printf("警告：PostgreSQL 不可用，订单过期收口任务未启动，未付款订单占用的号源不会被释放")
		return nil
	}
	// 支付宝凭据缺失属于启动期配置错误（而不是运行期抖动）：此时收口任务无法向支付宝确认
	// 订单是否已支付，若照常收口就会在不知道支付结果的情况下释放号源。这里选择不启动并告警，
	// 与支付接口「凭据缺失即返回 502 PAYMENT_PROVIDER_UNAVAILABLE」的降级口径一致。
	// 运行期的网络抖动 / 支付宝不可用仍按业务说明第 7、14.2 节处理：重试后继续收口。
	if err := alipayGateway.Ready(); err != nil {
		log.Printf("警告：支付宝网关配置无效，订单过期收口任务未启动："+
			"无法确认订单支付结果，不释放号源（未付款订单会继续占用号源）：%v", err)
		return nil
	}
	collector := paymentservice.NewExpiryCollector(
		paymentRepository,
		alipayGateway,
		paymentservice.SweepConfig{
			BatchSize:          cfg.Worker.OrderExpiryBatchSize,
			QueryAttempts:      cfg.Worker.OrderExpiryQueryAttempts,
			QueryRetryInterval: cfg.Worker.OrderExpiryQueryRetryInterval,
			OrderTimeout:       cfg.Worker.OrderExpiryOrderTimeout,
		},
	)
	return worker.NewOrderExpiryWorker(collector, worker.OrderExpiryConfig{
		Interval: cfg.Worker.OrderExpiryInterval,
	})
}

// isHTTPSNotifyURL 校验支付宝异步通知地址是否符合契约 §6.7：
// 必须是 https 且不带查询参数（支付宝对通知地址有这两项约束）。
func isHTTPSNotifyURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return parsed.Scheme == "https" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func (a *App) Run() error {
	defer func() {
		if a.clients.redis != nil {
			_ = a.clients.redis.Close()
		}
		if a.clients.postgres != nil {
			_ = a.clients.postgres.Close()
		}
	}()
	// 订单收口任务与 HTTP 服务同生命周期：Run 返回时先取消任务并在限时内等它收尾，
	// 再做关闭数据库连接等清理（defer 后进先出，所以这段注册在清理之后、执行在清理之前），
	// 避免任务在连接关闭后仍发起查询。
	//
	// 注意：收到 SIGINT/SIGTERM 时进程直接退出，不经过这里——本期不实现优雅停机，
	// 在途的一轮扫描随进程一起结束（每笔订单的外部调用都有独立时限，不会长期挂住）。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if a.orderExpiryWorker != nil {
		var workerDone sync.WaitGroup
		workerDone.Add(1)
		go func() {
			defer workerDone.Done()
			a.orderExpiryWorker.Run(ctx)
		}()
		defer func() {
			cancel()
			waitWorkerShutdown(&workerDone, orderExpiryShutdownGrace)
		}()
	}
	return a.server.Run(fmt.Sprintf("%s:%d", a.cfg.HTTP.Host, a.cfg.HTTP.Port))
}

// orderExpiryShutdownGrace 是退出时等待收口任务收尾的时限：任务每笔订单的外部调用都有上限，
// 正常会在下一笔开始前看到 ctx 取消并退出，因此这里只做有限等待，不阻塞进程退出。
const orderExpiryShutdownGrace = 5 * time.Second

// waitWorkerShutdown 限时等待后台任务退出，超时只告警（不让进程卡在清理路径上）。
func waitWorkerShutdown(done *sync.WaitGroup, timeout time.Duration) {
	finished := make(chan struct{})
	go func() {
		done.Wait()
		close(finished)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		log.Printf("警告：订单收口任务在 %s 内未退出，继续关闭依赖并退出进程", timeout)
	}
}

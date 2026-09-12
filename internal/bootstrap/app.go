package bootstrap

import (
	"Medical-Web-Backend/internal/repo"
	"fmt"
	"log"
	"net/url"
	"strings"

	"Medical-Web-Backend/internal/config"
	httptransport "Medical-Web-Backend/internal/transport/http"
	"github.com/gin-gonic/gin"
)

type App struct {
	cfg          config.Config
	server       *gin.Engine
	clients      clients
	dependencies map[string]dependencyState
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
		repo.NewPostgresDoctorRepository(connectedClients.postgres),
		repo.NewPostgresScheduleRepository(connectedClients.postgres),
		repo.NewRedisIdempotencyStore(connectedClients.redis),
		repo.NewPostgresPatientRepository(connectedClients.postgres),
		repo.NewWeChatCode2SessionClient(cfg.WeChat.AppID, cfg.WeChat.Secret),
		repo.NewPostgresRegistrationRepository(connectedClients.postgres),
		repo.NewPostgresPaymentRepository(connectedClients.postgres),
		repo.NewAlipayGateway(repo.AlipayOptions{
			AppID:      cfg.Alipay.AppID,
			PrivateKey: cfg.Alipay.PrivateKey,
			PublicKey:  cfg.Alipay.PublicKey,
			GatewayURL: cfg.Alipay.GatewayURL,
			SellerIDs:  cfg.Alipay.SellerIDList(),
			Timeout:    cfg.Alipay.Timeout,
		}),
		repo.NewPostgresMedicalRecordRepository(connectedClients.postgres),
	)

	return &App{cfg: cfg, server: server, clients: connectedClients, dependencies: dependencies}, nil
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
	return a.server.Run(fmt.Sprintf("%s:%d", a.cfg.HTTP.Host, a.cfg.HTTP.Port))
}

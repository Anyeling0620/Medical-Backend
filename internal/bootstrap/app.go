package bootstrap

import (
	"Medical-Web-Backend/internal/repo"
	"fmt"
	"log"

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
	)

	return &App{cfg: cfg, server: server, clients: connectedClients, dependencies: dependencies}, nil
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

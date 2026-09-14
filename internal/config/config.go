package config

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config contains runtime settings loaded from environment variables.
type Config struct {
	App      AppConfig
	HTTP     HTTPConfig
	Redis    RedisConfig
	Postgres PostgresConfig
	MinIO    MinIOConfig
	Auth     AuthConfig
	WeChat   WeChatConfig
	Alipay   AlipayConfig
	Worker   WorkerConfig
}

// WorkerConfig 是进程内后台任务（当前只有订单过期收口任务）的开关与节奏。
//
// 收口任务的规则见 04-api-contract.md §6.8 与 创建订单与支付业务说明.md 第 7 节：
// 每 10~30 秒一轮、进程启动先扫一轮、查询失败重试后仍继续收口。关闭任务会让未付款订单
// 一直占用号源，因此只在明确知道后果时才应关闭（bootstrap 启动时会输出告警）。
type WorkerConfig struct {
	// OrderExpiryEnabled 是订单过期收口任务的开关。
	OrderExpiryEnabled bool `env:"ORDER_EXPIRY_WORKER_ENABLED" envDefault:"true"`
	// OrderExpiryInterval 是扫描间隔，缺省 20 秒（规格建议 10~30 秒）。
	OrderExpiryInterval time.Duration `env:"ORDER_EXPIRY_INTERVAL" envDefault:"20s"`
	// OrderExpiryBatchSize 是单轮扫描的订单上限。
	OrderExpiryBatchSize int `env:"ORDER_EXPIRY_BATCH_SIZE" envDefault:"100"`
	// OrderExpiryQueryAttempts 是单笔订单调用 alipay.trade.query 的最大尝试次数（含首次）；
	// 超过阈值仍失败时继续收口，保证号源释放不被支付宝可用性阻塞。
	OrderExpiryQueryAttempts int `env:"ORDER_EXPIRY_QUERY_ATTEMPTS" envDefault:"3"`
	// OrderExpiryQueryRetryInterval 是两次查询尝试之间的等待间隔。
	OrderExpiryQueryRetryInterval time.Duration `env:"ORDER_EXPIRY_QUERY_RETRY_INTERVAL" envDefault:"500ms"`
	// OrderExpiryOrderTimeout 是单笔订单外部调用（查询 + 关单）的总时限。
	// 必须大于「ORDER_EXPIRY_QUERY_ATTEMPTS × ALIPAY_HTTP_TIMEOUT + 重试等待」并留出一次关单的余量，
	// 否则重试与关单会被单笔时限掐掉（缺省组合为 3×5s 查询 + 一次 5s 关单，故取 30s）。
	OrderExpiryOrderTimeout time.Duration `env:"ORDER_EXPIRY_ORDER_TIMEOUT" envDefault:"30s"`
}

type AppConfig struct {
	Env string `env:"APP_ENV" envDefault:"development"`
}

type HTTPConfig struct {
	Host string `env:"HTTP_HOST" envDefault:"0.0.0.0"`
	// 默认端口必须与 .env/.env.example 的 HTTP_PORT、deploy/Dockerfile 的 EXPOSE、
	// deploy/docker-compose.yaml 的端口映射保持一致（当前统一为 9080）：
	// 否则漏配 HTTP_PORT 时容器会监听 8080，而宿主机映射的是 9080，容器起了却连不通。
	Port int `env:"HTTP_PORT" envDefault:"9080"`
}

type RedisConfig struct {
	Addr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	Username string `env:"REDIS_USERNAME"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
}

type PostgresConfig struct {
	Addr     string `env:"PGSQL_ADDR" envDefault:"localhost:5432"`
	Username string `env:"PGSQL_USERNAME" envDefault:"postgres"`
	Password string `env:"PGSQL_PASSWORD"`
	Database string `env:"PGSQL_DATABASE" envDefault:"postgres"`
	SSLMode  string `env:"PGSQL_SSLMODE" envDefault:"disable"`
}

type MinIOConfig struct {
	Endpoint string `env:"MINIO_ENDPOINT" envDefault:"localhost:9000"`
	UseSSL   bool   `env:"MINIO_USE_SSL" envDefault:"false"`
	Bucket   string `env:"MINIO_BUCKET" envDefault:"medical"`
}

// WeChatConfig 是微信小程序登录（code2Session）所需的应用凭据。
// 生产环境必须配置；开发环境允许为空，此时 code 通道返回 502 DEPENDENCY_UNAVAILABLE，
// 但仍可用 openid 直通登录联调（仅 APP_ENV=development 放行，见 patientauth.LoginByOpenID）。
type WeChatConfig struct {
	AppID  string `env:"WECHAT_APPID"`
	Secret string `env:"WECHAT_SECRET"`
}

// AlipayConfig 是支付宝当面付（alipay.trade.precreate / alipay.trade.query /
// 异步通知验签）所需配置。凭据缺失不阻断启动：调用时返回 502
// PAYMENT_PROVIDER_UNAVAILABLE（与微信适配器同一取舍，见 bootstrap.NewApp 的启动提示）。
type AlipayConfig struct {
	// AppID 是支付宝开放平台分配给开发者的应用 ID。
	AppID string `env:"ALIPAY_APP_ID"`
	// PrivateKey 是应用私钥（PKCS#1/PKCS#8 的 base64 或 PEM），用于请求签名。
	PrivateKey string `env:"ALIPAY_APP_PRIVATE_SECRET"`
	// PublicKey 是支付宝公钥，用于异步通知验签与同步响应验签。
	PublicKey string `env:"ALIPAY_PUBLIC_SECRET"`
	// GatewayURL 是网关地址：生产 https://openapi.alipay.com/gateway.do，
	// 沙箱 https://openapi-sandbox.dl.alipaydev.com/gateway.do。
	GatewayURL string `env:"ALIPAY_GATEWAY_URL" envDefault:"https://openapi.alipay.com/gateway.do"`
	// Subject 是支付宝订单标题，不可含 / = & 等特殊字符。
	Subject string `env:"ALIPAY_SUBJECT" envDefault:"医院挂号费"`
	// SellerIDs 是允许的 seller_id 集合（逗号分隔）；为空表示不校验该字段（配置缺失时降级）。
	SellerIDs string `env:"ALIPAY_SELLER_IDS"`
	// NotifyURL 是支付宝异步通知地址（必须是 HTTPS 且不带查询参数）；
	// 为空时不向支付宝传该参数，系统降级为主动查询模式（契约 §6.7）。
	NotifyURL string `env:"NOTIFY_URL"`
	// Timeout 是单次网关调用超时。
	Timeout time.Duration `env:"ALIPAY_HTTP_TIMEOUT" envDefault:"5s"`
}

// SellerIDList 把逗号分隔的 seller_id 白名单切分为切片，去掉空白项。
func (c AlipayConfig) SellerIDList() []string {
	if strings.TrimSpace(c.SellerIDs) == "" {
		return nil
	}
	parts := strings.Split(c.SellerIDs, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func Load() (Config, error) {
	// Load local development values when a .env file is present. In production,
	// environment variables supplied by the process/container remain sufficient.
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, err
	}

	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

type AuthConfig struct {
	JWTSecret    string        `env:"JWT_SECRET" envDefault:"巴巴博一"`
	AccessTTL    time.Duration `env:"JWT_ACCESS_TTL" envDefault:"15m"`
	RefreshTTL   time.Duration `env:"JWT_REFRESH_TTL" envDefault:"168h"`
	CookieSecure bool          `env:"JWT_COOKIE_SECURE" envDefault:"false"`
}

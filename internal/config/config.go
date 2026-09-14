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
	// PublicCatalogCache 是匿名公开域目录（科室 / 子科室 / 医生）的逻辑过期缓存配置。
	PublicCatalogCache PublicCatalogCacheConfig
}

type AppConfig struct {
	Env string `env:"APP_ENV" envDefault:"development"`
}

type HTTPConfig struct {
	Host string `env:"HTTP_HOST" envDefault:"0.0.0.0"`
	Port int    `env:"HTTP_PORT" envDefault:"8080"`
}

type RedisConfig struct {
	Addr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	Username string `env:"REDIS_USERNAME"`
	Password string `env:"REDIS_PASSWORD"`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
}

// PublicCatalogCacheConfig 是公开域目录缓存（/api/v1/public/* 的科室、子科室、医生）的配置。
// 该缓存是纯性能优化：总开关关闭、Redis 不可用或载荷损坏时都会退化为直查数据库，
// 因此不需要像幂等存储那样在依赖不可用时阻断请求。
type PublicCatalogCacheConfig struct {
	// Enabled 是缓存总开关：置 false 即完全回到无缓存行为（回滚手段）。
	Enabled bool `env:"PUBLIC_CATALOG_CACHE_ENABLED" envDefault:"true"`
	// TTL 是基础逻辑过期时长；物理 TTL 取其 2 倍，以支持「先返回旧值、异步重建」。
	TTL time.Duration `env:"PUBLIC_CATALOG_CACHE_TTL" envDefault:"30m"`
	// Jitter 是 TTL 抖动比例，0.2 表示 ±20%，避免同一批键同时逻辑过期引发回源尖峰。
	Jitter float64 `env:"PUBLIC_CATALOG_CACHE_JITTER" envDefault:"0.2"`
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

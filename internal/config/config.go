package config

import (
	"errors"
	"math"
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
	Cache    CacheConfig
}

// 公开域读缓存（T4）的缺省值与安全边界。
// 排班余量必须新鲜，因此 TTL 上限被硬限制在 10 秒以内：即使运维把配置改大也不允许
// 出现「可挂但下单被拒」的长窗口。
const (
	// DefaultScheduleCacheTTL 是公开排班缓存的缺省基准 TTL（设计文档建议 5~10 秒）。
	DefaultScheduleCacheTTL = 8 * time.Second
	// MinScheduleCacheTTL / MaxScheduleCacheTTL 是排班缓存 TTL 的收敛区间。
	MinScheduleCacheTTL = 1 * time.Second
	MaxScheduleCacheTTL = 10 * time.Second
	// DefaultCacheJitterRatio 是 TTL 抖动的缺省比例（±20%）。
	DefaultCacheJitterRatio = 0.2
	// MaxCacheJitterRatio 是抖动比例上限，避免抖动把 TTL 拉成负数或过长。
	MaxCacheJitterRatio = 0.5
	// DefaultCacheRedisTimeout 是单次 Redis 操作的缺省超时。
	DefaultCacheRedisTimeout = 100 * time.Millisecond
	// MaxCacheRedisTimeout 是单次 Redis 操作超时的上限：慢 Redis 不得把请求一起拖慢。
	MaxCacheRedisTimeout = 1 * time.Second
)

// CacheConfig 是公开域读缓存的运行时配置。
//
// 定位：缓存是**性能依赖，不是正确性依赖**。Redis 不可用时读路径必须 fail open 直查数据库，
// 与幂等存储不可用时返回 503（fail closed）的取舍方向相反，实现时不要照抄幂等那套。
type CacheConfig struct {
	// Enabled 是读缓存总开关；关闭后公开域查询一律直查数据库。
	Enabled bool `env:"CACHE_ENABLED" envDefault:"true"`
	// ScheduleTTL 是公开排班（含余量）缓存的基准 TTL。
	ScheduleTTL time.Duration `env:"CACHE_SCHEDULE_TTL" envDefault:"8s"`
	// ScheduleJitter 是 TTL 抖动比例：实际 TTL = base*(1±jitter)，避免同一时刻集体过期（雪崩）。
	// 0 是合法取值，表示显式关闭抖动；缺省值由 envDefault 提供，仅在越界时回落缺省。
	ScheduleJitter float64 `env:"CACHE_SCHEDULE_TTL_JITTER" envDefault:"0.2"`
	// RedisTimeout 是单次缓存 Redis 操作的超时。
	RedisTimeout time.Duration `env:"CACHE_REDIS_TIMEOUT" envDefault:"100ms"`
}

// Normalize 把越界或缺失的缓存配置收敛到安全区间，返回可直接使用的副本。
// 放在配置层而不是仓储层，保证所有调用方（含测试）拿到的是同一套口径。
func (c CacheConfig) Normalize() CacheConfig {
	// NaN 必须单独判：NaN 与任何数比较都是 false，会绕过区间校验，
	// 随后 MaxScheduleCacheTTL/(1+NaN) 会把基准 TTL 压成负数，静默关闭缓存。
	if math.IsNaN(c.ScheduleJitter) || c.ScheduleJitter < 0 || c.ScheduleJitter > MaxCacheJitterRatio {
		c.ScheduleJitter = DefaultCacheJitterRatio
	}
	if c.ScheduleTTL < MinScheduleCacheTTL || c.ScheduleTTL > MaxScheduleCacheTTL {
		c.ScheduleTTL = DefaultScheduleCacheTTL
	}
	// 抖动会把实际 TTL 拉长到 base*(1+jitter)：这里按抖动比例反推基准上限，
	// 让「陈旧窗口不超过 MaxScheduleCacheTTL」这句话在配置层就成立，
	// 而不是只在默认配置下成立（基准 10s + 抖动 20% 会写出 12s 的键）。
	if maxBase := time.Duration(float64(MaxScheduleCacheTTL) / (1 + c.ScheduleJitter)); c.ScheduleTTL > maxBase {
		c.ScheduleTTL = maxBase
	}
	// 兜底保证后置条件「TTL ∈ [MinScheduleCacheTTL, MaxScheduleCacheTTL]」恒成立：
	// 当前常量下上面两步不会把基准压到 1s 以下，这里防的是将来改小 MaxScheduleCacheTTL
	// 或调大 MaxCacheJitterRatio 时把 TTL 压成 0 甚至负数（Redis 的 0 表示永不过期）。
	if c.ScheduleTTL < MinScheduleCacheTTL {
		c.ScheduleTTL = MinScheduleCacheTTL
	}
	if c.RedisTimeout <= 0 || c.RedisTimeout > MaxCacheRedisTimeout {
		c.RedisTimeout = DefaultCacheRedisTimeout
	}
	return c
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

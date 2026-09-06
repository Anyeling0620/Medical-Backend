package config

import (
	"errors"
	"os"
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
	RocketMQ RocketMQConfig
	OTel     OTelConfig
	Log      LogConfig
	Auth     AuthConfig
}

type AppConfig struct {
	Name string `env:"APP_NAME" envDefault:"medical-backend"`
	Env  string `env:"APP_ENV" envDefault:"development"`
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
	Endpoint  string `env:"MINIO_ENDPOINT" envDefault:"localhost:9000"`
	AccessKey string `env:"MINIO_ACCESS_KEY"`
	SecretKey string `env:"MINIO_SECRET_KEY"`
	UseSSL    bool   `env:"MINIO_USE_SSL" envDefault:"false"`
	Bucket    string `env:"MINIO_BUCKET" envDefault:"medical"`
}

type RocketMQConfig struct {
	Endpoint  string `env:"ROCKETMQ_ENDPOINT" envDefault:"localhost:8081"`
	Namespace string `env:"ROCKETMQ_NAMESPACE"`
	AccessKey string `env:"ROCKETMQ_ACCESS_KEY"`
	SecretKey string `env:"ROCKETMQ_SECRET_KEY"`
	Topic     string `env:"ROCKETMQ_TOPIC" envDefault:"medical_events"`
}

type OTelConfig struct {
	Endpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"localhost:4317"`
	Service  string `env:"OTEL_SERVICE_NAME" envDefault:"medical-backend"`
	Enabled  bool   `env:"OTEL_ENABLED" envDefault:"false"`
}

type LogConfig struct {
	Level  string `env:"LOG_LEVEL" envDefault:"info"`
	Format string `env:"LOG_FORMAT" envDefault:"json"`
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

package bootstrap

import (
	"context"
	"database/sql"
	"time"

	"github.com/redis/go-redis/v9"

	"Medical-Web-Backend/internal/config"
)

type dependencyState struct {
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

type clients struct {
	redis    *redis.Client
	postgres *sql.DB
}

func connectDependencies(cfg config.Config) (clients, map[string]dependencyState) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	states := make(map[string]dependencyState, 2)
	var result clients
	postgresDB, err := connectPostgres(ctx, cfg.Postgres)
	if err != nil {
		states["postgres"] = dependencyState{Error: err.Error()}
	} else {
		result.postgres = postgresDB
		states["postgres"] = dependencyState{Connected: true}
	}

	redisClient := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Username: cfg.Redis.Username, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		states["redis"] = dependencyState{Error: err.Error()}
		_ = redisClient.Close()
	} else {
		result.redis = redisClient
		states["redis"] = dependencyState{Connected: true}
	}

	return result, states
}

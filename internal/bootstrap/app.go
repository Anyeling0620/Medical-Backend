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

	// 判断是否为生产环境
	if cfg.App.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
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

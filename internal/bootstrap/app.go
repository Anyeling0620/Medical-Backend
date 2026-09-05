package bootstrap

import (
	"fmt"

	"github.com/gin-gonic/gin"

	"Medical-Web-Backend/internal/config"
	httptransport "Medical-Web-Backend/internal/transport/http"
)

type App struct {
	cfg    config.Config
	server *gin.Engine
}

func NewApp(cfg config.Config) (*App, error) {
	if cfg.HTTP.Port < 1 || cfg.HTTP.Port > 65535 {
		return nil, fmt.Errorf("HTTP_PORT must be between 1 and 65535")
	}

	if cfg.App.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	server := httptransport.NewRouter()

	return &App{cfg: cfg, server: server}, nil
}

func (a *App) Run() error {
	return a.server.Run(fmt.Sprintf("%s:%d", a.cfg.HTTP.Host, a.cfg.HTTP.Port))
}

package bootstrap

import (
	"context"
	"database/sql"
	"net/url"

	_ "github.com/jackc/pgx/v5/stdlib"

	"Medical-Web-Backend/internal/config"
)

func connectPostgres(ctx context.Context, cfg config.PostgresConfig) (*sql.DB, error) {
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.Username, cfg.Password),
		Host:   cfg.Addr,
		Path:   cfg.Database,
	}
	query := dsn.Query()
	query.Set("sslmode", cfg.SSLMode)
	dsn.RawQuery = query.Encode()

	db, err := sql.Open("pgx", dsn.String())
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

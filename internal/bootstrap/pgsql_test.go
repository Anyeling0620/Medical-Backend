package bootstrap

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"Medical-Web-Backend/internal/config"
)

func TestPostgresConnection(t *testing.T) {
	if os.Getenv("PGSQL_INTEGRATION_TEST") != "1" {
		t.Skip("set PGSQL_INTEGRATION_TEST=1 to test the configured PostgreSQL server")
	}
	if err := godotenv.Load("../../.env"); err != nil {
		t.Fatalf("load .env: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	db, err := connectPostgres(ctx, cfg.Postgres)
	if err != nil {
		t.Fatalf("connect PostgreSQL: %v", err)
	}
	defer db.Close()

	var value int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil {
		t.Fatalf("query PostgreSQL: %v", err)
	}
	if value != 1 {
		t.Fatalf("SELECT 1 returned %d", value)
	}
}

package bootstrap

import (
	"database/sql"
	"testing"
	"time"

	"Medical-Web-Backend/internal/config"
)

func TestApplyConnectionPoolSetsMaxOpenConnections(t *testing.T) {
	db, err := sql.Open("pgx", "postgres://")
	if err != nil {
		t.Fatalf("open pgx database handle: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})

	cfg := config.PostgresConfig{
		MaxOpenConns:    17,
		MaxIdleConns:    9,
		ConnMaxLifetime: 30 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}
	applyConnectionPool(db, cfg)

	if got, want := db.Stats().MaxOpenConnections, cfg.MaxOpenConns; got != want {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, want)
	}
}

func TestNormalizeMaxIdleConns(t *testing.T) {
	tests := []struct {
		name          string
		maxOpenConns  int
		maxIdleConns  int
		wantIdleConns int
	}{
		{
			name:          "keeps idle limit when it does not exceed open limit",
			maxOpenConns:  30,
			maxIdleConns:  20,
			wantIdleConns: 20,
		},
		{
			name:          "clamps idle limit to open limit",
			maxOpenConns:  30,
			maxIdleConns:  40,
			wantIdleConns: 30,
		},
		{
			name:          "keeps idle limit when open limit is unlimited",
			maxOpenConns:  0,
			maxIdleConns:  40,
			wantIdleConns: 40,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeMaxIdleConns(tt.maxOpenConns, tt.maxIdleConns); got != tt.wantIdleConns {
				t.Fatalf("normalizeMaxIdleConns(%d, %d) = %d, want %d", tt.maxOpenConns, tt.maxIdleConns, got, tt.wantIdleConns)
			}
		})
	}
}

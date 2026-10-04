package database

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDirectURL (audit #21): migrations must bypass the Neon pooler so the
// session-level advisory lock and unlock run on the same server connection.
func TestDirectURL(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@ep-cool-1234-pooler.eu-central-1.aws.neon.tech/db?sslmode=require": "postgres://u:p@ep-cool-1234.eu-central-1.aws.neon.tech/db?sslmode=require",
		"postgres://u:p@ep-cool-1234-pooler.eu.neon.tech:5432/db":                          "postgres://u:p@ep-cool-1234.eu.neon.tech:5432/db",
		"postgres://u:p@ep-cool-1234.eu.neon.tech/db":                                      "postgres://u:p@ep-cool-1234.eu.neon.tech/db",
		"postgres://postgres:pg@localhost:5433/juz?sslmode=disable":                        "postgres://postgres:pg@localhost:5433/juz?sslmode=disable",
	}
	for in, want := range cases {
		if got := DirectURL(in); got != want {
			t.Errorf("DirectURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyPoolOptions(t *testing.T) {
	url := "postgres://u:p@localhost:5432/db"
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	applyPoolOptions(cfg, url, PoolOptions{MaxConns: 20, MinConns: 5, MaxConnLifetime: 30 * time.Minute, MaxConnIdleTime: 5 * time.Minute})
	if cfg.MaxConns != 20 || cfg.MinConns != 5 || cfg.MaxConnLifetime != 30*time.Minute || cfg.MaxConnIdleTime != 5*time.Minute {
		t.Fatalf("pool options not applied: %+v", cfg)
	}
	url2 := url + "?pool_max_conns=3"
	cfg2, _ := pgxpool.ParseConfig(url2)
	applyPoolOptions(cfg2, url2, PoolOptions{MaxConns: 20, MinConns: 5})
	if cfg2.MaxConns != 3 || cfg2.MinConns != 3 {
		t.Fatalf("URL must win and MinConns <= MaxConns: max=%d min=%d", cfg2.MaxConns, cfg2.MinConns)
	}
}

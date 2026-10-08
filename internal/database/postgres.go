// Package database manages the PostgreSQL connection pool and migrations.
package database

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/migrations"
)

// Connect creates a pgx connection pool with the pgx default size and
// verifies connectivity (used by tests and tools).
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return ConnectPool(ctx, databaseURL, 0)
}

// PoolOptions tunes the pgx pool (R-10a). Zero fields keep the URL / pgx
// defaults; explicit pool_* parameters in the URL always win.
type PoolOptions struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// ConnectPoolOpts is ConnectPool with the full set of pool options.
func ConnectPoolOpts(ctx context.Context, databaseURL string, o PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	applyPoolOptions(cfg, databaseURL, o)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func applyPoolOptions(cfg *pgxpool.Config, databaseURL string, o PoolOptions) {
	if o.MaxConns > 0 && !strings.Contains(databaseURL, "pool_max_conns") {
		cfg.MaxConns = o.MaxConns
	}
	if o.MinConns > 0 && !strings.Contains(databaseURL, "pool_min_conns") {
		cfg.MinConns = min(o.MinConns, cfg.MaxConns)
	}
	if o.MaxConnLifetime > 0 && !strings.Contains(databaseURL, "pool_max_conn_lifetime") {
		cfg.MaxConnLifetime = o.MaxConnLifetime
	}
	if o.MaxConnIdleTime > 0 && !strings.Contains(databaseURL, "pool_max_conn_idle_time") {
		cfg.MaxConnIdleTime = o.MaxConnIdleTime
	}
}

// ConnectPool creates a pgx connection pool bounded by maxConns (0 = keep
// the URL's pool_max_conns / the pgx default) and verifies connectivity.
// An explicit pool_max_conns in the URL always wins over maxConns.
func ConnectPool(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	return ConnectPoolOpts(ctx, databaseURL, PoolOptions{MaxConns: maxConns})
}

// DirectURL returns a connection string that bypasses a Neon connection
// pooler: the "-pooler" suffix of the first host label is removed
// (ep-xxx-pooler.region.aws.neon.tech -> ep-xxx.region.aws.neon.tech).
// Other URLs are returned unchanged. Session-level advisory locks (the
// migration lock) are only reliable on a direct connection: behind a
// transaction-mode pooler pg_advisory_lock and pg_advisory_unlock may run on
// different server connections.
func DirectURL(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil || u.Host == "" {
		return databaseURL
	}
	host := u.Hostname()
	label, rest, _ := strings.Cut(host, ".")
	if !strings.HasSuffix(label, "-pooler") {
		return databaseURL
	}
	newHost := strings.TrimSuffix(label, "-pooler")
	if rest != "" {
		newHost += "." + rest
	}
	if p := u.Port(); p != "" {
		newHost += ":" + p
	}
	u.Host = newHost
	return u.String()
}

// MigrateURL runs the migrations over a dedicated, short-lived DIRECT
// connection (see DirectURL) and closes it afterwards.
func MigrateURL(ctx context.Context, databaseURL string) error {
	pool, err := ConnectPool(ctx, databaseURL, 2)
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer pool.Close()
	return Migrate(ctx, pool)
}

// migrationLockKey is the pg_advisory_lock key that serializes migrations
// ("juz40mig" as an int64-ish constant — any fixed value works).
const migrationLockKey int64 = 0x6a757a34306d6967

// Migrate applies all pending SQL migrations embedded in the binary.
// Migrations run inside transactions and are recorded in schema_migrations.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	// Serialize migrations across instances: during a zero-downtime deploy
	// (Render) the old and the new instance may start at the same moment,
	// and two concurrent runs of the same migration would crash one of them
	// on the schema_migrations primary key. A session-level advisory lock on
	// a dedicated connection makes the second instance wait and then see
	// every migration as already applied.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	// Everything below runs on the SAME connection that holds the lock:
	// with a pool of one connection (pool_max_conns=1 in the URL wins over
	// the size MigrateURL asks for) a second connection never came and the
	// start hung forever on the first migration.
	// Create migration tracking table.
	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Read embedded migration files.
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var versions []int64
	files := make(map[int64]string)

	for _, entry := range entries {
		name := entry.Name()

		if entry.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}

		versionText := strings.SplitN(name, "_", 2)[0]

		version, err := strconv.ParseInt(versionText, 10, 64)
		if err != nil {
			return fmt.Errorf(
				"parse migration version from %q: %w",
				name,
				err,
			)
		}

		// Two files with the same number would silently skip one of them
		// (only one version row is ever recorded) — refuse to start.
		if prev, dup := files[version]; dup {
			return fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, name)
		}
		versions = append(versions, version)
		files[version] = name
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i] < versions[j]
	})

	// Apply migrations in order.
	for _, version := range versions {
		var applied bool

		err := conn.QueryRow(
			ctx,
			`SELECT EXISTS(
				SELECT 1
				FROM schema_migrations
				WHERE version = $1::BIGINT
			)`,
			version,
		).Scan(&applied)

		if err != nil {
			return fmt.Errorf(
				"check migration %d: %w",
				version,
				err,
			)
		}

		if applied {
			continue
		}

		// Read migration SQL.
		body, err := migrations.FS.ReadFile(files[version])
		if err != nil {
			return fmt.Errorf(
				"read migration %d: %w",
				version,
				err,
			)
		}

		// Start transaction.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf(
				"begin migration %d: %w",
				version,
				err,
			)
		}

		// Always rollback if something fails before commit.
		success := false
		defer func() {
			if !success {
				_ = tx.Rollback(ctx)
			}
		}()

		// Apply SQL migration.
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf(
				"apply migration %d: %w",
				version,
				err,
			)
		}

		// Save migration version.
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO schema_migrations (version)
			 VALUES ($1::BIGINT)`,
			version,
		); err != nil {
			return fmt.Errorf(
				"record migration %d: %w",
				version,
				err,
			)
		}

		// Commit.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf(
				"commit migration %d: %w",
				version,
				err,
			)
		}

		success = true
	}

	return nil
}

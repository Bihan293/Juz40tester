// Package database manages the PostgreSQL connection pool and migrations.
package database

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/migrations"
)

// Connect creates a pgx connection pool and verifies connectivity.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
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

	// Create migration tracking table.
	if _, err := pool.Exec(ctx, `
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

		versions = append(versions, version)
		files[version] = name
	}

	sort.Slice(versions, func(i, j int) bool {
		return versions[i] < versions[j]
	})

	// Apply migrations in order.
	for _, version := range versions {
		var applied bool

		err := pool.QueryRow(
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
		tx, err := pool.Begin(ctx)
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

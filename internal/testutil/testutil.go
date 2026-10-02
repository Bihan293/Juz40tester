// Package testutil holds helpers shared by the integration tests only. It is
// never imported by production code.
package testutil

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateSubject inserts (or reuses) a subject with the given name and returns
// its id. Production subjects are managed by migrations; this exists only so
// integration tests can create isolated subjects.
func CreateSubject(ctx context.Context, pool *pgxpool.Pool, name string) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO subjects (name, position)
		VALUES ($1, (SELECT COALESCE(MAX(position), 0) + 1 FROM subjects))
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, name).Scan(&id)
	return id, err
}

package database

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// TestMigrateConcurrent runs Migrate from several "instances" at once (the
// zero-downtime deploy case): every run must succeed and every migration
// must be recorded exactly once. Needs TEST_DATABASE_URL.
func TestMigrateConcurrent(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const instances = 4
	var wg sync.WaitGroup
	errs := make(chan error, instances)
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool, err := Connect(ctx, url)
			if err != nil {
				errs <- err
				return
			}
			defer pool.Close()
			errs <- Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migrate: %v", err)
		}
	}

	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var dups int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT version FROM schema_migrations GROUP BY version HAVING COUNT(*) > 1
		) d`).Scan(&dups); err != nil {
		t.Fatal(err)
	}
	if dups != 0 {
		t.Fatalf("%d migration(s) recorded more than once", dups)
	}
}

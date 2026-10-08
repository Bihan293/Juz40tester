package cluster

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/database"
)

func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE cluster_instances`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// TestRegistryCountsLiveSenders: web instances do not count, stale ones
// expire, and a stopped instance removes its row.
func TestRegistryCountsLiveSenders(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()
	w1 := NewRegistry(pool, "w1", "worker", true)
	w2 := NewRegistry(pool, "w2", "worker", true)
	web := NewRegistry(pool, "web", "web", false)
	if n, err := w1.Beat(ctx); err != nil || n != 1 {
		t.Fatalf("w1 alone: %d %v", n, err)
	}
	if n, _ := web.Beat(ctx); n != 1 {
		t.Fatalf("web sees %d senders, want 1", n)
	}
	if n, _ := w2.Beat(ctx); n != 2 {
		t.Fatalf("w2 sees %d senders, want 2", n)
	}
	// w1 stops heart-beating.
	if _, err := pool.Exec(ctx, `UPDATE cluster_instances SET heartbeat_at = now() - interval '5 minutes' WHERE id = 'w1'`); err != nil {
		t.Fatal(err)
	}
	if n, _ := w2.Beat(ctx); n != 1 {
		t.Fatalf("stale instance still counted: %d", n)
	}
	// Run: reports the count, deletes its row on stop.
	rctx, cancel := context.WithCancel(ctx)
	got := make(chan int, 4)
	done := make(chan struct{})
	go func() { w1.Run(rctx, func(n int) { got <- n }); close(done) }()
	select {
	case n := <-got:
		if n != 2 {
			t.Fatalf("Run reported %d, want 2", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not report")
	}
	cancel()
	<-done
	var exists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cluster_instances WHERE id = 'w1')`).Scan(&exists)
	if exists {
		t.Fatal("stopped instance kept its row")
	}
}

// TestPostgresBus: an event published with pg_notify reaches the listener.
func TestPostgresBus(t *testing.T) {
	pool := pgPool(t)
	bus := NewPostgresBus(pool, os.Getenv("TEST_DATABASE_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := make(chan Event, 2)
	b := NewEvents(bus, "b")
	go b.Run(ctx, func(e Event) { got <- e })
	a := NewEvents(bus, "a")
	// The listener needs a moment to LISTEN; publish until delivered.
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		a.Publish(Event{Type: EventTrFinished, TestID: 42, Ready: true})
		select {
		case e := <-got:
			if e.TestID != 42 || !e.Ready {
				t.Fatalf("event %+v", e)
			}
			return
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("event not delivered")
		}
	}
}

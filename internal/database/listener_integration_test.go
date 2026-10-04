package database

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestListenReceivesNotify: a NOTIFY on a listened channel reaches onNotify.
// Needs TEST_DATABASE_URL.
func TestListenReceivesNotify(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got := make(chan string, 16)
	done := make(chan struct{})
	go func() { defer close(done); Listen(ctx, url, []string{"test_ch"}, func(ch string) { got <- ch }) }()
	// The first call is the catch-up after LISTEN is active.
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("listener did not start")
	}
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `SELECT pg_notify('test_ch', '')`); err != nil {
		t.Fatal(err)
	}
	select {
	case ch := <-got:
		if ch != "test_ch" {
			t.Fatalf("channel = %q", ch)
		}
	case <-ctx.Done():
		t.Fatal("notification not received")
	}
	cancel()
	<-done
}

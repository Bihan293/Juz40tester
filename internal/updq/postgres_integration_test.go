package updq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/database"
)

// Integration tests of the PostgreSQL queue. Need TEST_DATABASE_URL.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freshPG(t *testing.T, pool *pgxpool.Pool, maxBacklog int) *Postgres {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE tg_update_queue`); err != nil {
		t.Fatal(err)
	}
	return NewPostgres(pool, maxBacklog)
}

func TestPostgresQueueConformance(t *testing.T) {
	pool := pgPool(t)
	runConformance(t, func(t *testing.T, max int) Queue { return freshPG(t, pool, max) })
}

// TestPostgresReaper: a claim whose worker stopped heart-beating goes back
// to pending; one that used all attempts becomes a dead letter.
func TestPostgresReaper(t *testing.T) {
	pool := pgPool(t)
	q := freshPG(t, pool, 0)
	ctx := context.Background()
	mustEnqueue(t, q, 1, 1)
	mustEnqueue(t, q, 2, 2)
	items := claimAll(t, q, "dead-worker")
	if len(items) != 2 {
		t.Fatalf("claimed %d", len(items))
	}
	// Heartbeat keeps update 1 alive; update 2's worker «died» 10 min ago.
	if _, err := pool.Exec(ctx, `UPDATE tg_update_queue SET heartbeat_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err := q.Heartbeat(ctx, "dead-worker", items[:1]); err != nil {
		t.Fatal(err)
	}
	req, dead, err := q.Reap(ctx, time.Minute, 3)
	if err != nil || req != 1 || dead != 0 {
		t.Fatalf("reap: requeued=%d dead=%d err=%v", req, dead, err)
	}
	again := claimAll(t, q, "w2")
	if len(again) != 1 || again[0].UpdateID != 2 || again[0].Attempts != 2 {
		t.Fatalf("re-claimed %+v", again)
	}
	// Last attempt also dies → dead letter.
	if _, err := pool.Exec(ctx, `UPDATE tg_update_queue SET heartbeat_at = now() - interval '10 minutes' WHERE update_id = 2`); err != nil {
		t.Fatal(err)
	}
	req, dead, err = q.Reap(ctx, time.Minute, 2)
	if err != nil || req != 0 || dead != 1 {
		t.Fatalf("reap 2: requeued=%d dead=%d err=%v", req, dead, err)
	}
	dl, err := q.DeadLetters(ctx, 10)
	if err != nil || len(dl) != 1 || dl[0].UpdateID != 2 || dl[0].LastError == "" {
		t.Fatalf("dead letters %+v err=%v", dl, err)
	}
	// Manual replay.
	if ok, err := q.Requeue(ctx, 2); err != nil || !ok {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	if got := claimAll(t, q, "w3"); len(got) != 1 || got[0].UpdateID != 2 || got[0].Attempts != 1 {
		t.Fatalf("after requeue claimed %+v", got)
	}
}

// TestPostgresPurge: old handled rows and dead letters are deleted, fresh
// ones (the idempotency window) are kept.
func TestPostgresPurge(t *testing.T) {
	pool := pgPool(t)
	q := freshPG(t, pool, 0)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		mustEnqueue(t, q, i, i)
	}
	for _, it := range claimAll(t, q, "w") {
		_ = q.Complete(ctx, it)
	}
	if _, err := pool.Exec(ctx, `UPDATE tg_update_queue SET finished_at = now() - interval '3 days' WHERE update_id IN (1, 2)`); err != nil {
		t.Fatal(err)
	}
	n, err := q.Purge(ctx, 48*time.Hour, 14*24*time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tg_update_queue`).Scan(&left)
	if left != 1 {
		t.Fatalf("left %d rows, want 1", left)
	}
}

// TestPostgresEnqueueNotifies: a new update is announced on tg_updates.
func TestPostgresEnqueueNotifies(t *testing.T) {
	pool := pgPool(t)
	q := freshPG(t, pool, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := make(chan string, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		database.ListenPayload(ctx, os.Getenv("TEST_DATABASE_URL"), []string{ChannelUpdates}, func(ch, _ string) { got <- ch })
	}()
	<-got // catch-up call once LISTEN is active
	mustEnqueue(t, q, 77, 1)
	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("no NOTIFY for a new update")
	}
	cancel()
	<-done
}

// TestPostgresEndToEnd: webhook ingress → tg_update_queue → two consumers
// (two «worker instances») woken by LISTEN/NOTIFY; every update handled
// exactly once, per-user order kept, re-deliveries ignored.
func TestPostgresEndToEnd(t *testing.T) {
	pool := pgPool(t)
	q := freshPG(t, pool, 0)
	url := os.Getenv("TEST_DATABASE_URL")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	seen := map[int64]int{}
	order := map[int64][]int64{}
	handle := func(_ context.Context, upd *bot.Update) error {
		mu.Lock()
		seen[upd.UpdateID]++
		order[upd.UserKey()] = append(order[upd.UserKey()], upd.UpdateID)
		mu.Unlock()
		return nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		c := NewConsumer(q, ConsumerConfig{Worker: "inst" + strconv.Itoa(i), Workers: 4, Poll: time.Hour}, handle)
		wg.Add(2)
		go func() { defer wg.Done(); c.Run(ctx) }()
		go func() {
			defer wg.Done()
			database.ListenPayload(ctx, url, []string{ChannelUpdates}, func(string, string) { c.Wake() })
		}()
	}
	time.Sleep(300 * time.Millisecond) // listeners up
	in := NewIngress("", q)
	for id := int64(1); id <= 60; id++ {
		user := id%4 + 1
		body := `{"update_id":` + strconv.FormatInt(id, 10) + `,"callback_query":{"id":"x","from":{"id":` + strconv.FormatInt(user, 10) + `},"data":"d"}}`
		for rep := 0; rep < 2; rep++ { // every update delivered twice
			rec := httptest.NewRecorder()
			in.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("ingress: %d", rec.Code)
			}
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n == 60 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("handled %d of 60 updates (NOTIFY wake-up broken?)", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("update %d handled %d times", id, n)
		}
	}
	for u, o := range order {
		for i := 1; i < len(o); i++ {
			if o[i] < o[i-1] {
				t.Fatalf("user %d out of order: %v", u, o)
			}
		}
	}
}

// TestPostgresNulInPayload: a \u0000 (rejected by jsonb) never makes the
// update un-storable (it would be answered 503 forever).
func TestPostgresNulInPayload(t *testing.T) {
	pool := pgPool(t)
	q := freshPG(t, pool, 0)
	body := []byte(`{"update_id":5,"message":{"message_id":1,"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"a\u0000b"}}`)
	ins, err := q.Enqueue(context.Background(), 5, 1, body)
	if err != nil || !ins {
		t.Fatalf("enqueue with NUL: inserted=%v err=%v", ins, err)
	}
}

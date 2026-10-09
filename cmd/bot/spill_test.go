package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/updq"
)

type memSpill struct {
	mu   sync.Mutex
	ids  []int64
	fail bool
}

func (m *memSpill) Enqueue(_ context.Context, id, _ int64, _ []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return false, fmt.Errorf("db down")
	}
	m.ids = append(m.ids, id)
	return true, nil
}

func (m *memSpill) n() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.ids)
}

// A backlog that cannot be processed before the shutdown deadline is
// saved (spilled) instead of being dropped after Telegram got its 200.
func TestShutdownSpillsUnprocessedUpdates(t *testing.T) {
	old := spillReserve
	spillReserve = 300 * time.Millisecond
	defer func() { spillReserve = old }()

	release := make(chan struct{})
	var handled atomic.Int32
	d := newUpdateDispatcher("", 2, 100, time.Hour, func(ctx context.Context, u *bot.Update) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		handled.Add(1)
	})
	sp := &memSpill{}
	d.spill = sp
	for i := 1; i <= 20; i++ {
		postRaw(d, fmt.Sprintf(`{"update_id":%d,"message":{"message_id":1,"from":{"id":%d},"chat":{"id":%d,"type":"private"},"text":"x"}}`, 500+i, i, i), "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(450 * time.Millisecond); close(release) }() // in-flight ones finish after the spill started
	_ = d.Shutdown(ctx)
	if got := sp.n(); got != 18 {
		t.Fatalf("spilled %d update(s), want 18 (20 accepted - 2 in flight)", got)
	}
	if d.dropped.Load() != 0 {
		t.Fatalf("%d update(s) dropped", d.dropped.Load())
	}
	if handled.Load() != 2 {
		t.Fatalf("handled %d, want the 2 in-flight ones", handled.Load())
	}
}

// A backlog that fits into the grace period is processed locally as
// before — nothing is spilled.
func TestShutdownProcessesSmallBacklogLocally(t *testing.T) {
	var handled atomic.Int32
	d := newUpdateDispatcher("", 2, 100, time.Hour, func(context.Context, *bot.Update) { handled.Add(1) })
	sp := &memSpill{}
	d.spill = sp
	for i := 1; i <= 10; i++ {
		postRaw(d, fmt.Sprintf(`{"update_id":%d}`, 900+i), "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if handled.Load() != 10 || sp.n() != 0 {
		t.Fatalf("handled %d, spilled %d — want 10 / 0", handled.Load(), sp.n())
	}
}

// When the spill itself fails, the update is still processed if time
// allows (old behaviour) and counted as dropped otherwise — never lost
// silently.
func TestShutdownSpillFailureFallsBack(t *testing.T) {
	old := spillReserve
	spillReserve = time.Hour // spill at once
	defer func() { spillReserve = old }()
	var handled atomic.Int32
	d := newUpdateDispatcher("", 1, 100, time.Hour, func(context.Context, *bot.Update) { handled.Add(1) })
	d.spill = &memSpill{fail: true}
	for i := 1; i <= 5; i++ {
		postRaw(d, fmt.Sprintf(`{"update_id":%d}`, 700+i), "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.Shutdown(ctx)
	if handled.Load()+int32(d.dropped.Load()) != 5 {
		t.Fatalf("handled %d + dropped %d != 5", handled.Load(), d.dropped.Load())
	}
}

// End to end on PostgreSQL: one instance spills, the next one replays
// every update exactly once and leaves no rows behind.
func TestSpillReplayPostgres(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tg_update_queue`); err != nil {
		t.Fatal(err)
	}
	old := spillReserve
	spillReserve = time.Hour
	defer func() { spillReserve = old }()

	// Instance 1: its handler is stuck, everything queued gets spilled.
	stuck := make(chan struct{})
	d1 := newUpdateDispatcher("", 1, 100, time.Hour, func(ctx context.Context, _ *bot.Update) {
		select {
		case <-stuck:
		case <-ctx.Done():
		}
	})
	d1.spill = updq.NewPostgres(pool, 0)
	base := time.Now().UnixNano() % 1_000_000_000
	const n = 30
	for i := int64(1); i <= n; i++ {
		body := fmt.Sprintf(`{"update_id":%d,"callback_query":{"id":"c%d","from":{"id":%d},"data":"ans:1:%d:0"}}`, base+i, i, 4242, i)
		if code := postRaw(d1, body, ""); code != 200 {
			t.Fatalf("post %d: HTTP %d", i, code)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_ = d1.Shutdown(sctx)
	cancel()
	close(stuck)
	if d1.spilled.Load() != n-1 {
		t.Fatalf("spilled %d, want %d", d1.spilled.Load(), n-1)
	}

	// Instance 2 replays them, in update_id order, once.
	var mu sync.Mutex
	var got []string
	d2 := newUpdateDispatcher("", 1, 100, time.Hour, func(_ context.Context, u *bot.Update) {
		mu.Lock()
		got = append(got, u.CallbackQuery.Data)
		mu.Unlock()
	})
	replayed, err := replaySpilledOnce(ctx, pool, d2)
	if err != nil || replayed != n-1 {
		t.Fatalf("replayed %d (%v), want %d", replayed, err, n-1)
	}
	if again, _ := replaySpilledOnce(ctx, pool, d2); again != 0 {
		t.Fatalf("replayed %d update(s) twice", again)
	}
	if err := d2.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(got) != n-1 || got[0] != "ans:1:2:0" || got[len(got)-1] != fmt.Sprintf("ans:1:%d:0", n) {
		t.Fatalf("replayed payloads: %v", got)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tg_update_queue`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d row(s) left (%v)", left, err)
	}
}

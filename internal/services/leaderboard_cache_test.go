package services

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newTestBoardCache(ttl time.Duration) (*leaderboardCache, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newLeaderboardCache(ttl)
	c.now = clk.now
	return c, clk
}

// P0-4: repeated requests within the TTL do not re-run the SQL; each
// subject has its own entry; expiry triggers a fresh load.
func TestLeaderboardCacheHitMissExpiry(t *testing.T) {
	c, clk := newTestBoardCache(time.Minute)
	var calls int32
	load := func(id int64) func(context.Context) ([]models.LeaderboardEntry, error) {
		return func(context.Context) ([]models.LeaderboardEntry, error) {
			n := atomic.AddInt32(&calls, 1)
			return []models.LeaderboardEntry{{UserID: id, Green: int(n)}}, nil
		}
	}
	ctx := context.Background()
	k1 := leaderboardKey{boardGreen, 1, 10}
	k2 := leaderboardKey{boardGreen, 2, 10}

	r, _ := c.get(ctx, k1, load(1))
	r[0].Green = 999 // callers' mutations must not reach the cache
	r, _ = c.get(ctx, k1, load(1))
	if calls != 1 || r[0].Green != 1 {
		t.Fatalf("hit: calls=%d green=%d", calls, r[0].Green)
	}
	if r, _ = c.get(ctx, k2, load(2)); calls != 2 || r[0].UserID != 2 {
		t.Fatalf("separate subject entry: calls=%d row=%+v", calls, r[0])
	}
	if _, _ = c.get(ctx, leaderboardKey{boardUnlocked, 1, 10}, load(1)); calls != 3 {
		t.Fatalf("separate board kind: calls=%d", calls)
	}
	clk.add(59 * time.Second)
	if c.get(ctx, k1, load(1)); calls != 3 {
		t.Fatalf("still fresh: calls=%d", calls)
	}
	clk.add(2 * time.Second)
	if r, _ = c.get(ctx, k1, load(1)); calls != 4 || r[0].Green != 4 {
		t.Fatalf("expired: calls=%d green=%d", calls, r[0].Green)
	}
}

func TestLeaderboardCacheErrorsNotCached(t *testing.T) {
	c, _ := newTestBoardCache(time.Minute)
	var calls int32
	boom := errors.New("db down")
	k := leaderboardKey{boardUnlocked, 1, 10}
	_, err := c.get(context.Background(), k, func(context.Context) ([]models.LeaderboardEntry, error) {
		atomic.AddInt32(&calls, 1)
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	_, err = c.get(context.Background(), k, func(context.Context) ([]models.LeaderboardEntry, error) {
		atomic.AddInt32(&calls, 1)
		return []models.LeaderboardEntry{{UserID: 1}}, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("after error: err=%v calls=%d", err, calls)
	}
}

// Concurrent misses for the same subject share one SQL query.
func TestLeaderboardCacheSingleFlight(t *testing.T) {
	c, _ := newTestBoardCache(time.Minute)
	var calls int32
	release := make(chan struct{})
	load := func(context.Context) ([]models.LeaderboardEntry, error) {
		atomic.AddInt32(&calls, 1)
		<-release
		return []models.LeaderboardEntry{{UserID: 7}}, nil
	}
	k := leaderboardKey{boardGreen, 1, 10}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.get(context.Background(), k, load)
			if err != nil || len(r) != 1 || r[0].UserID != 7 {
				t.Errorf("r=%v err=%v", r, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

// mapCache is an in-memory SharedCache (stands for Redis in unit tests).
type mapCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *mapCache) Get(_ context.Context, k string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok, nil
}

func (c *mapCache) Set(_ context.Context, k string, v []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = v
	return nil
}

// Cluster mode: two instances share the second-level cache, so the heavy
// leaderboard query runs once for both.
func TestLeaderboardSharedCacheAcrossInstances(t *testing.T) {
	shared := &mapCache{m: map[string][]byte{}}
	a, _ := newTestBoardCache(time.Minute)
	b, _ := newTestBoardCache(time.Minute)
	a.shared, b.shared = shared, shared
	var calls int32
	load := func(context.Context) ([]models.LeaderboardEntry, error) {
		atomic.AddInt32(&calls, 1)
		return []models.LeaderboardEntry{{UserID: 1, FirstName: "Аня", Green: 5}}, nil
	}
	key := leaderboardKey{boardGreen, 3, 10}
	if _, err := a.get(context.Background(), key, load); err != nil {
		t.Fatal(err)
	}
	rows, err := b.get(context.Background(), key, load)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("query ran %d times, want 1", calls)
	}
	if len(rows) != 1 || rows[0].FirstName != "Аня" || rows[0].Green != 5 {
		t.Fatalf("rows from the shared cache: %+v", rows)
	}
}

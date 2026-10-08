package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// LeaderboardCacheTTL is how long a computed subject leaderboard is reused
// (P0-4). Every «🏆 Топ» tap used to re-run the heavy aggregation over
// test_questions × user_question_progress for ALL users of the subject; a
// leaderboard that is up to two minutes old is perfectly fine for users.
const LeaderboardCacheTTL = 2 * time.Minute

type leaderboardKind uint8

const (
	boardUnlocked leaderboardKind = iota + 1
	boardGreen
)

type leaderboardKey struct {
	kind      leaderboardKind
	subjectID int64
	limit     int
}

// leaderboardEntry is either a cached result (done closed, expires set) or
// an in-flight computation that concurrent callers wait on (single-flight).
type leaderboardEntry struct {
	done    chan struct{}
	rows    []models.LeaderboardEntry
	err     error
	expires time.Time
}

// leaderboardCache is a small in-memory TTL cache with one entry per
// (leaderboard kind, subject, limit). Concurrent misses for the same key
// share ONE SQL query; errors are never cached.
type leaderboardCache struct {
	ttl time.Duration
	now func() time.Time

	mu      sync.Mutex
	entries map[leaderboardKey]*leaderboardEntry

	// shared (optional, CACHE_BACKEND=redis) is a second level shared by
	// every instance: with N workers the heavy aggregation runs once per
	// TTL for the whole cluster instead of once per instance.
	shared SharedCache
}

// SharedCache is the cross-instance cache used as a second level
// (cluster.Cache satisfies it).
type SharedCache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}

func (k leaderboardKey) String() string {
	return fmt.Sprintf("lb:%d:%d:%d", k.kind, k.subjectID, k.limit)
}

// loadShared wraps load with the shared second level (errors of the shared
// cache are ignored: it is an optimisation, the database is the truth).
func (c *leaderboardCache) loadShared(ctx context.Context, key leaderboardKey,
	load func(context.Context) ([]models.LeaderboardEntry, error)) ([]models.LeaderboardEntry, error) {
	if c.shared == nil {
		return load(ctx)
	}
	if b, ok, err := c.shared.Get(ctx, key.String()); err == nil && ok {
		var rows []models.LeaderboardEntry
		if json.Unmarshal(b, &rows) == nil {
			return rows, nil
		}
	}
	rows, err := load(ctx)
	if err != nil {
		return nil, err
	}
	if b, merr := json.Marshal(rows); merr == nil {
		_ = c.shared.Set(ctx, key.String(), b, c.ttl)
	}
	return rows, nil
}

func newLeaderboardCache(ttl time.Duration) *leaderboardCache {
	return &leaderboardCache{ttl: ttl, now: time.Now, entries: map[leaderboardKey]*leaderboardEntry{}}
}

func (c *leaderboardCache) get(ctx context.Context, key leaderboardKey,
	load func(context.Context) ([]models.LeaderboardEntry, error)) ([]models.LeaderboardEntry, error) {
	for {
		c.mu.Lock()
		e, ok := c.entries[key]
		if ok {
			select {
			case <-e.done:
				if c.now().Before(e.expires) {
					c.mu.Unlock()
					return cloneBoard(e.rows), nil
				}
				// Expired: drop it and compute a fresh one below.
				delete(c.entries, key)
				ok = false
			default:
			}
		}
		if ok {
			// Another goroutine is computing this key — wait for it.
			c.mu.Unlock()
			select {
			case <-e.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if e.err == nil {
				return cloneBoard(e.rows), nil
			}
			// The leader failed (maybe only its own context was cancelled):
			// retry, possibly becoming the leader ourselves.
			continue
		}

		e = &leaderboardEntry{done: make(chan struct{})}
		c.entries[key] = e
		c.mu.Unlock()

		// The query must not be cut short by one impatient waiter only,
		// but the leader's own deadline still applies.
		rows, err := c.loadShared(ctx, key, load)

		c.mu.Lock()
		e.rows, e.err = rows, err
		if err != nil {
			if c.entries[key] == e {
				delete(c.entries, key)
			}
		} else {
			e.expires = c.now().Add(c.ttl)
		}
		close(e.done)
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return cloneBoard(rows), nil
	}
}

// cloneBoard returns a copy so callers cannot mutate the cached slice.
func cloneBoard(rows []models.LeaderboardEntry) []models.LeaderboardEntry {
	if rows == nil {
		return nil
	}
	out := make([]models.LeaderboardEntry, len(rows))
	copy(out, rows)
	return out
}

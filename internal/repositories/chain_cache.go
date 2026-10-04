package repositories

import (
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// chainCache (R-10a) is an in-memory, per-subject cache of the chain
// STRUCTURE (ListChainTests: the shared chain tests, never user progress).
// It is process-wide so that every repository writing chain tests
// (GenerationRepository) invalidates what SubjectRepository serves. A zero
// TTL (the default, e.g. in tests) disables it; main enables it via
// SetChainCacheTTL. Other instances see changes after at most one TTL.
type chainCacheT struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[int64]chainCacheEntry
}

type chainCacheEntry struct {
	tests   []models.Test
	expires time.Time
}

var chainCache = &chainCacheT{now: time.Now, entries: map[int64]chainCacheEntry{}}

// SetChainCacheTTL enables (ttl > 0) or disables the chain-structure cache.
func SetChainCacheTTL(ttl time.Duration) {
	chainCache.mu.Lock()
	defer chainCache.mu.Unlock()
	chainCache.ttl = ttl
	chainCache.entries = map[int64]chainCacheEntry{}
}

// InvalidateChainCache drops the cached chain of the subject (call after a
// chain test is created/deleted/regenerated or a generation job finishes).
func InvalidateChainCache(subjectID int64) {
	chainCache.mu.Lock()
	delete(chainCache.entries, subjectID)
	chainCache.mu.Unlock()
}

func (c *chainCacheT) get(subjectID int64) ([]models.Test, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 {
		return nil, false
	}
	e, ok := c.entries[subjectID]
	if !ok || !c.now().Before(e.expires) {
		delete(c.entries, subjectID)
		return nil, false
	}
	return append([]models.Test(nil), e.tests...), true
}

func (c *chainCacheT) put(subjectID int64, tests []models.Test) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 {
		return
	}
	c.entries[subjectID] = chainCacheEntry{
		tests:   append([]models.Test(nil), tests...),
		expires: c.now().Add(c.ttl),
	}
}

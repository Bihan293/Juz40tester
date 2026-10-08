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
	// versions counts the invalidations per subject. A reader notes the
	// version BEFORE it queries the database and stores its result only
	// when no invalidation happened meanwhile: otherwise a read that started
	// before a new chain test was committed could put the OLD chain back
	// right after InvalidateChainCache and serve it for a whole TTL.
	versions map[int64]uint64
}

type chainCacheEntry struct {
	tests   []models.Test
	expires time.Time
}

var chainCache = &chainCacheT{now: time.Now, entries: map[int64]chainCacheEntry{}, versions: map[int64]uint64{}}

// SetChainCacheTTL enables (ttl > 0) or disables the chain-structure cache.
func SetChainCacheTTL(ttl time.Duration) {
	chainCache.mu.Lock()
	defer chainCache.mu.Unlock()
	chainCache.ttl = ttl
	chainCache.entries = map[int64]chainCacheEntry{}
	for id := range chainCache.versions {
		chainCache.versions[id]++ // in-flight reads must not repopulate
	}
}

// InvalidateChainCache drops the cached chain of the subject (call after a
// chain test is created/deleted/regenerated or a generation job finishes).
func InvalidateChainCache(subjectID int64) {
	chainCache.mu.Lock()
	delete(chainCache.entries, subjectID)
	chainCache.versions[subjectID]++
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

// version returns the invalidation counter of the subject (see versions).
func (c *chainCacheT) version(subjectID int64) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.versions[subjectID]
}

// putIfVersion stores tests only when the subject was not invalidated since
// version ver was read (the data may be older than the invalidation).
func (c *chainCacheT) putIfVersion(subjectID int64, ver uint64, tests []models.Test) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.versions[subjectID] != ver {
		return
	}
	c.putLocked(subjectID, tests)
}

func (c *chainCacheT) put(subjectID int64, tests []models.Test) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(subjectID, tests)
}

func (c *chainCacheT) putLocked(subjectID int64, tests []models.Test) {
	if c.ttl <= 0 {
		return
	}
	c.entries[subjectID] = chainCacheEntry{
		tests:   append([]models.Test(nil), tests...),
		expires: c.now().Add(c.ttl),
	}
}

package repositories

import (
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
)

func TestChainCacheTTLAndInvalidate(t *testing.T) {
	SetChainCacheTTL(time.Minute)
	defer SetChainCacheTTL(0)
	now := time.Unix(1000, 0)
	chainCache.now = func() time.Time { return now }
	defer func() { chainCache.now = time.Now }()

	chainCache.put(7, []models.Test{{ID: 1, SubjectID: 7, TestNumber: 1}})
	if got, ok := chainCache.get(7); !ok || len(got) != 1 {
		t.Fatal("cached chain not served within TTL")
	}
	now = now.Add(61 * time.Second)
	if _, ok := chainCache.get(7); ok {
		t.Fatal("expired chain served")
	}
	chainCache.put(7, []models.Test{{ID: 1}})
	InvalidateChainCache(7)
	if _, ok := chainCache.get(7); ok {
		t.Fatal("invalidated chain served")
	}
	SetChainCacheTTL(0)
	chainCache.put(7, []models.Test{{ID: 1}})
	if _, ok := chainCache.get(7); ok {
		t.Fatal("disabled cache served")
	}
}

// A read that started before a chain test was created (and the cache
// invalidated) must not put the OLD chain back afterwards.
func TestChainCacheStaleReadNotStoredAfterInvalidate(t *testing.T) {
	SetChainCacheTTL(time.Minute)
	defer SetChainCacheTTL(0)

	ver := chainCache.version(9) // reader: before its DB query
	InvalidateChainCache(9)      // writer: new chain test committed
	chainCache.putIfVersion(9, ver, []models.Test{{ID: 1, SubjectID: 9, TestNumber: 1}})
	if _, ok := chainCache.get(9); ok {
		t.Fatal("stale chain (read before the invalidation) was cached")
	}

	ver = chainCache.version(9) // a read after the invalidation is fresh
	chainCache.putIfVersion(9, ver, []models.Test{{ID: 1}, {ID: 2}})
	if got, ok := chainCache.get(9); !ok || len(got) != 2 {
		t.Fatal("fresh chain not cached")
	}
}

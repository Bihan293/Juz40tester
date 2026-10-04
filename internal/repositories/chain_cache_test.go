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

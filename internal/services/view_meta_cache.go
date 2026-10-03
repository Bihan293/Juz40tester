package services

import (
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// viewMetaTTL / viewMetaMax bound the per-attempt TestViewMeta cache. An
// attempt normally lasts minutes; entries of abandoned attempts simply
// expire. The cap protects memory under a burst of distinct attempts.
const (
	viewMetaTTL = 2 * time.Hour
	viewMetaMax = 20000
)

// viewMetaCache keeps the TestViewMeta of running attempts (R-2).
//
// Why it is safe: an attempt is bound to one test; the question count of a
// test (test_questions) and the subject name do not change while users are
// taking it. The only field that can change is Translated (the Kazakh
// translation may complete after the attempt started); callers re-read the
// meta for Kazakh users while it is incomplete (meta.complete()), so a
// completed translation is picked up exactly as before. A translation is
// never "un-completed" for a running attempt: ReplaceQuestionContent refuses
// questions that are still unanswered in an active attempt.
//
// Per-process only: with several instances each keeps its own copy — that
// is fine, the data is immutable and every instance can load it itself.
type viewMetaCache struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[int64]viewMetaEntry
}

type viewMetaEntry struct {
	meta    *cachedMeta
	expires time.Time
}

// cachedMeta is a TestViewMeta copy plus the test it belongs to.
type cachedMeta struct {
	repositories.TestViewMeta
	TestID int64
}

// metaComplete reports whether every question of the test has a Kazakh
// translation (the only state of TestViewMeta that can still change).
func metaComplete(m *repositories.TestViewMeta) bool { return m.Total > 0 && m.Translated >= m.Total }

func newViewMetaCache(ttl time.Duration, max int) *viewMetaCache {
	return &viewMetaCache{ttl: ttl, max: max, now: time.Now, entries: map[int64]viewMetaEntry{}}
}

// get returns the cached meta (as a fresh copy) of the attempt.
func (c *viewMetaCache) get(attemptID int64) (*repositories.TestViewMeta, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[attemptID]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		delete(c.entries, attemptID)
		return nil, false
	}
	m := e.meta.TestViewMeta
	return &m, true
}

func (c *viewMetaCache) put(attemptID, testID int64, m *repositories.TestViewMeta) {
	if c == nil || m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= c.max {
		for id, e := range c.entries { // drop expired first
			if !now.Before(e.expires) {
				delete(c.entries, id)
			}
		}
		for id := range c.entries { // still full: evict arbitrary entries
			if len(c.entries) < c.max {
				break
			}
			delete(c.entries, id)
		}
	}
	c.entries[attemptID] = viewMetaEntry{meta: &cachedMeta{TestViewMeta: *m, TestID: testID}, expires: now.Add(c.ttl)}
}

func (c *viewMetaCache) drop(attemptID int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, attemptID)
	c.mu.Unlock()
}

func (c *viewMetaCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

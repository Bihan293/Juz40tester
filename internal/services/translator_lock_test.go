package services

import (
	"sync"
	"testing"
)

// #40: per-test locks are removed after use (also on panic) and the lock
// still serialises concurrent work on the same test.
func TestTranslatorInFlightCleanup(t *testing.T) {
	tr := NewTranslatorService(nil, nil)

	for id := int64(1); id <= 1000; id++ {
		release := tr.lockTest(id)
		release()
	}
	if n := tr.inFlightLen(); n != 0 {
		t.Fatalf("inFlight grew to %d after 1000 finished translations", n)
	}

	// Panic inside the critical section must not leak the entry.
	func() {
		defer func() { _ = recover() }()
		release := tr.lockTest(7)
		defer release()
		panic("boom")
	}()
	if n := tr.inFlightLen(); n != 0 {
		t.Fatalf("entry leaked after panic: %d", n)
	}

	// Concurrent holders of the same and different ids: mutual exclusion
	// per id, and the map is empty when everyone is done.
	var wg sync.WaitGroup
	inside := map[int64]int{}
	var mu sync.Mutex
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := int64(g % 4)
			release := tr.lockTest(id)
			defer release()
			mu.Lock()
			inside[id]++
			if inside[id] > 1 {
				mu.Unlock()
				t.Errorf("two goroutines inside test %d at once", id)
				return
			}
			mu.Unlock()
			mu.Lock()
			inside[id]--
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	if n := tr.inFlightLen(); n != 0 {
		t.Fatalf("inFlight not empty after concurrent use: %d", n)
	}
}

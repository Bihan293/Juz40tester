package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateLimitOneActionPerInterval (R-9): taps faster than the interval are
// rejected, the next one after the interval passes; users are independent.
func TestRateLimitOneActionPerInterval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := New(300 * time.Millisecond).WithClock(func() time.Time { return now })
	if !l.Allow(1) {
		t.Fatal("first action must pass")
	}
	now = now.Add(100 * time.Millisecond)
	if l.Allow(1) {
		t.Fatal("action after 100ms must be throttled")
	}
	if !l.Allow(2) {
		t.Fatal("another user must not be affected")
	}
	now = now.Add(150 * time.Millisecond) // 250ms after the accepted one
	if l.Allow(1) {
		t.Fatal("action after 250ms must be throttled")
	}
	now = now.Add(50 * time.Millisecond) // 300ms after the accepted one
	if !l.Allow(1) {
		t.Fatal("action after the interval must pass (rejected taps must not extend the window)")
	}
}

// TestRateLimitBurstConcurrent: a burst of 100 concurrent taps of one user
// lets exactly ONE through.
func TestRateLimitBurstConcurrent(t *testing.T) {
	l := New(time.Hour)
	var ok int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Allow(42) {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d actions passed, want 1", ok)
	}
}

// TestRateLimitDisabledAndBounded: interval 0 disables the limiter; stale
// keys are swept so memory stays bounded.
func TestRateLimitDisabledAndBounded(t *testing.T) {
	if l := New(0); !l.Allow(1) || !l.Allow(1) {
		t.Fatal("disabled limiter must allow everything")
	}
	var nilL *Limiter
	if !nilL.Allow(1) {
		t.Fatal("nil limiter must allow")
	}
	now := time.Unix(0, 0)
	l := New(time.Second).WithClock(func() time.Time { return now })
	l.maxKeys = 10
	for i := int64(0); i < 10; i++ {
		l.Allow(i)
	}
	now = now.Add(2 * time.Second)
	l.Allow(100)
	if l.Len() > 2 {
		t.Fatalf("stale keys not swept: %d", l.Len())
	}
}

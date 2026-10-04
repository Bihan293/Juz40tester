package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// R-4: any number of users waiting for the same test share ONE waiter —
// one publish reaches all of them, and the fallback DB check runs once per
// interval per TEST, not per user.
func TestTranslationHubFanOutSharedPoll(t *testing.T) {
	var polls int32
	h := newTranslationHub(func(ctx context.Context, testID int64) (TranslationOutcome, bool, error) {
		atomic.AddInt32(&polls, 1)
		return TranslationOutcome{}, false, nil // still running
	}, 20*time.Millisecond)

	const users = 1000
	chans := make([]<-chan TranslationOutcome, users)
	for i := range chans {
		chans[i] = h.subscribe(42)
	}
	if n := h.waiting(42); n != users {
		t.Fatalf("waiting = %d, want %d", n, users)
	}
	time.Sleep(110 * time.Millisecond) // ~5 poll intervals
	if p := atomic.LoadInt32(&polls); p == 0 || p > 8 {
		t.Fatalf("polls = %d for %d waiters: want one shared poll per interval", p, users)
	}
	h.publish(42, TranslationOutcome{Ready: true})
	for i, ch := range chans {
		select {
		case out := <-ch:
			if !out.Ready {
				t.Fatalf("waiter %d got %+v", i, out)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d never notified", i)
		}
	}
	if n := h.waiting(42); n != 0 {
		t.Fatalf("subscribers left after publish: %d", n)
	}
	// The poller stops once the test is resolved.
	time.Sleep(60 * time.Millisecond)
	before := atomic.LoadInt32(&polls)
	time.Sleep(60 * time.Millisecond)
	if after := atomic.LoadInt32(&polls); after != before {
		t.Fatalf("poller kept polling after publish: %d -> %d", before, after)
	}
}

// A job finished by ANOTHER instance is picked up by the shared poll.
func TestTranslationHubPollResolves(t *testing.T) {
	var n int32
	h := newTranslationHub(func(ctx context.Context, testID int64) (TranslationOutcome, bool, error) {
		if atomic.AddInt32(&n, 1) >= 2 {
			return TranslationOutcome{Err: "boom"}, true, nil
		}
		return TranslationOutcome{}, false, nil
	}, 10*time.Millisecond)
	a, b := h.subscribe(7), h.subscribe(7)
	for _, ch := range []<-chan TranslationOutcome{a, b} {
		select {
		case out := <-ch:
			if out.Ready || out.Err != "boom" {
				t.Fatalf("got %+v", out)
			}
		case <-time.After(time.Second):
			t.Fatal("poll outcome not delivered")
		}
	}
}

// Nobody waits forever: the wait times out with TimedOut, and an
// unsubscribed waiter is dropped.
func TestTranslationHubTimeoutAndUnsubscribe(t *testing.T) {
	h := newTranslationHub(nil, 5*time.Millisecond)
	h.timeout = 40 * time.Millisecond
	gone := h.subscribe(9)
	h.unsubscribe(9, gone)
	if n := h.waiting(9); n != 0 {
		t.Fatalf("unsubscribe left %d waiters", n)
	}
	ch := h.subscribe(9)
	select {
	case out := <-ch:
		if !out.TimedOut || out.Ready {
			t.Fatalf("got %+v, want timeout", out)
		}
	case <-time.After(time.Second):
		t.Fatal("wait never timed out")
	}
}

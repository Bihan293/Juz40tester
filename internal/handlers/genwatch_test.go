package handlers

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// R-7: many users waiting on the same chain test share ONE poller; the
// worker's «job finished» kick delivers the result to all of them at once
// (no waiting for the 15–30 s poll), and the key is unregistered.
func TestGenWatchSharedPollerAndKick(t *testing.T) {
	f := &kbFake{}
	h := New(bot.NewClient("T").WithBaseURL(f.server(t).URL), nil, nil)
	h.gen.interval, h.gen.jitter = time.Hour, 0 // only the kick can wake it

	var checks, ready atomic.Int32
	check := func(context.Context) (*models.Test, bool, error) {
		checks.Add(1)
		if ready.Load() == 1 {
			return &models.Test{ID: 7, TestNumber: 3, Kind: models.TestKindChain}, false, nil
		}
		return nil, true, nil
	}
	key := chainWatchKey(1, 3)
	if !h.subscribeGeneration(key, 100, 1, check, "r") {
		t.Fatal("first subscriber must start the poller")
	}
	if h.subscribeGeneration(key, 200, 2, check, "r") || h.subscribeGeneration(key, 300, 3, check, "r") {
		t.Fatal("next subscribers must reuse the existing poller")
	}
	if !h.gen.isSubscribed(key, 200) {
		t.Fatal("chat 200 must be subscribed")
	}

	ready.Store(1)
	h.NotifyJobFinished(&models.GenerationJob{Kind: models.TestKindChain, SubjectID: 1, TestNumber: 3})
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n := f.calls
		f.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kick did not deliver the result to 3 subscribers (calls=%d)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.Close(time.Second)
	if c := checks.Load(); c != 1 {
		t.Fatalf("checks = %d, want 1 (one shared poll after the kick)", c)
	}
	if h.gen.isSubscribed(key, 100) {
		t.Fatal("key must be unregistered after the result")
	}
}

// R-7: shutdown stops the pollers without leaking goroutines.
func TestGenWatchCloseStopsPollers(t *testing.T) {
	h := New(bot.NewClient("T").WithBaseURL("http://127.0.0.1:1"), nil, nil)
	h.gen.interval, h.gen.jitter = time.Hour, 0
	check := func(context.Context) (*models.Test, bool, error) { return nil, true, nil }
	h.subscribeGeneration(jobWatchKey(5), 1, 1, check, "r")
	done := make(chan struct{})
	go func() { h.Close(2 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not stop the poller")
	}
	if h.subscribeGeneration(jobWatchKey(6), 1, 1, check, "r") {
		t.Fatal("no new pollers after Close")
	}
}

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

func post(d *updateDispatcher, body, secret string) int {
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec.Code
}

// TestWebhookMalformedJSONReturns200 (audit #29): a broken update is logged
// and acknowledged with 200 so Telegram does not re-deliver it forever; it
// never reaches the handler.
func TestWebhookMalformedJSONReturns200(t *testing.T) {
	var handled int32
	d := newUpdateDispatcher("", 4, 100, time.Minute, func(context.Context, *bot.Update) { atomic.AddInt32(&handled, 1) })
	if code := post(d, `{"update_id": 1, "message": {`, ""); code != http.StatusOK {
		t.Fatalf("malformed update: got HTTP %d, want 200", code)
	}
	_ = d.Shutdown(context.Background())
	if handled != 0 {
		t.Fatal("malformed update must not be handled")
	}
}

// TestWebhookSecretEnforced: a wrong / missing secret is rejected with 403.
func TestWebhookSecretEnforced(t *testing.T) {
	d := newUpdateDispatcher("s3cret", 4, 100, time.Minute, func(context.Context, *bot.Update) {})
	if code := post(d, `{"update_id":1}`, ""); code != http.StatusForbidden {
		t.Fatalf("missing secret: HTTP %d", code)
	}
	if code := post(d, `{"update_id":1}`, "wrong"); code != http.StatusForbidden {
		t.Fatalf("wrong secret: HTTP %d", code)
	}
	if code := post(d, `{"update_id":1}`, "s3cret"); code != http.StatusOK {
		t.Fatalf("right secret: HTTP %d", code)
	}
	_ = d.Shutdown(context.Background())
}

// TestWebhookShutdownWaitsForUpdates (audit #19): an acknowledged update
// that is still being processed is NOT lost — Shutdown waits for it; new
// updates after shutdown started are refused with 503 (Telegram re-delivers).
func TestWebhookShutdownWaitsForUpdates(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var finished int32
	d := newUpdateDispatcher("", 4, 100, time.Minute, func(ctx context.Context, u *bot.Update) {
		close(started)
		<-release
		atomic.StoreInt32(&finished, 1)
	})
	if code := post(d, `{"update_id":1}`, ""); code != http.StatusOK {
		t.Fatalf("HTTP %d", code)
	}
	<-started
	done := make(chan error, 1)
	go func() { done <- d.Shutdown(context.Background()) }()

	select {
	case <-done:
		t.Fatal("Shutdown returned while an update was still processing")
	case <-time.After(150 * time.Millisecond):
	}
	if code := post(d, `{"update_id":2}`, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("update during shutdown: HTTP %d, want 503", code)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if atomic.LoadInt32(&finished) != 1 {
		t.Fatal("in-flight update was not completed before shutdown returned")
	}
}

// TestWebhookShutdownTimeoutCancelsHandlers: when the graceful wait times
// out, the handlers' context is cancelled (no hang on SIGTERM).
func TestWebhookShutdownTimeoutCancelsHandlers(t *testing.T) {
	cancelled := make(chan struct{})
	d := newUpdateDispatcher("", 4, 100, time.Hour, func(ctx context.Context, u *bot.Update) {
		<-ctx.Done()
		close(cancelled)
	})
	post(d, `{"update_id":1}`, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler context was not cancelled on shutdown timeout")
	}
}

// TestWebhookConcurrencyBounded (audit #20): never more than maxConcurrent
// updates are processed at once, however many arrive; all are processed.
func TestWebhookConcurrencyBounded(t *testing.T) {
	const limit, total = 3, 40
	var cur, peak, done int32
	var mu sync.Mutex
	d := newUpdateDispatcher("", limit, 100, time.Minute, func(ctx context.Context, u *bot.Update) {
		n := atomic.AddInt32(&cur, 1)
		mu.Lock()
		if n > peak {
			peak = n
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		atomic.AddInt32(&done, 1)
	})
	for i := 0; i < total; i++ {
		if code := post(d, `{"update_id":1}`, ""); code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if peak > limit {
		t.Fatalf("peak concurrency %d exceeds limit %d", peak, limit)
	}
	if done != total {
		t.Fatalf("processed %d of %d updates", done, total)
	}
}

// TestSetWebhookRetries (audit #22): temporary failures are retried with a
// pause instead of crashing the process.
func TestSetWebhookRetries(t *testing.T) {
	var calls int32
	err := setWebhookWithRetry(context.Background(), func(context.Context) error {
		if atomic.AddInt32(&calls, 1) < 3 {
			return errors.New("dial tcp: api.telegram.org: connection refused")
		}
		return nil
	}, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d, want nil / 3", err, calls)
	}
}

// TestSetWebhookStopsOnShutdown: the retry loop ends when ctx is cancelled.
func TestSetWebhookStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	err := setWebhookWithRetry(ctx, func(context.Context) error { return errors.New("down") }, 5*time.Millisecond, 10*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestWebhookBurstBoundedGoroutines (R-3): a burst of updates far larger
// than the worker pool never creates a goroutine per update — the number
// of goroutines stays ~constant (workers only), all updates are processed.
func TestWebhookBurstBoundedGoroutines(t *testing.T) {
	const workers, queue, burst = 4, 1000, 900
	release := make(chan struct{})
	var done int32
	d := newUpdateDispatcher("", workers, queue, time.Minute, func(ctx context.Context, u *bot.Update) {
		<-release
		atomic.AddInt32(&done, 1)
	})
	before := runtime.NumGoroutine()
	for i := 0; i < burst; i++ {
		if code := post(d, `{"update_id":1}`, ""); code != http.StatusOK {
			t.Fatalf("update %d: HTTP %d", i, code)
		}
	}
	// Before R-3 every update got its own goroutine BEFORE the semaphore:
	// 900 updates = +900 goroutines. Now the queue holds them as data.
	if grown := runtime.NumGoroutine() - before; grown > 10 {
		t.Fatalf("goroutines grew by %d during a burst of %d updates", grown, burst)
	}
	if n := d.QueueLen(); n < burst-workers {
		t.Fatalf("queue holds %d updates, want >= %d", n, burst-workers)
	}
	close(release)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done != burst {
		t.Fatalf("processed %d of %d acknowledged updates", done, burst)
	}
}

// TestWebhookQueueOverflowReturns503 (R-3): once workers are busy and the
// bounded queue is full, the webhook answers 503 FAST (Telegram re-delivers
// later) instead of blocking or spawning goroutines; capacity frees up
// again as soon as the backlog is processed.
func TestWebhookQueueOverflowReturns503(t *testing.T) {
	const workers, queue = 2, 5
	release := make(chan struct{})
	started := make(chan struct{}, workers)
	var done int32
	d := newUpdateDispatcher("", workers, queue, time.Minute, func(ctx context.Context, u *bot.Update) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		atomic.AddInt32(&done, 1)
	})
	// Occupy both workers, then fill the queue.
	for i := 0; i < workers; i++ {
		if code := post(d, `{"update_id":1}`, ""); code != http.StatusOK {
			t.Fatalf("HTTP %d", code)
		}
	}
	for i := 0; i < workers; i++ {
		<-started
	}
	for i := 0; i < queue; i++ {
		if code := post(d, `{"update_id":2}`, ""); code != http.StatusOK {
			t.Fatalf("queued update %d: HTTP %d", i, code)
		}
	}
	// Overflow: 503, quickly.
	start := time.Now()
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(`{"update_id":3}`))
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("overflow update: HTTP %d, want 503", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("503 must carry Retry-After")
		}
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("overflow responses took %s — must not block", el)
	}
	if d.rejected.Load() != 20 {
		t.Fatalf("rejected = %d, want 20", d.rejected.Load())
	}
	// Drain: everything acknowledged with 200 is processed, then new
	// updates are accepted again.
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&done) < workers+queue && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if code := post(d, `{"update_id":4}`, ""); code != http.StatusOK {
		t.Fatalf("after drain: HTTP %d, want 200", code)
	}
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done != workers+queue+1 {
		t.Fatalf("processed %d, want %d (no acknowledged update may be lost)", done, workers+queue+1)
	}
}

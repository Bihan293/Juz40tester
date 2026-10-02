package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
	d := newUpdateDispatcher("", 4, time.Minute, func(context.Context, *bot.Update) { atomic.AddInt32(&handled, 1) })
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
	d := newUpdateDispatcher("s3cret", 4, time.Minute, func(context.Context, *bot.Update) {})
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
	d := newUpdateDispatcher("", 4, time.Minute, func(ctx context.Context, u *bot.Update) {
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
	d := newUpdateDispatcher("", 4, time.Hour, func(ctx context.Context, u *bot.Update) {
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
	d := newUpdateDispatcher("", limit, time.Minute, func(ctx context.Context, u *bot.Update) {
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

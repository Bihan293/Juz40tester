package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// updateDispatcher receives Telegram webhook requests and processes the
// updates asynchronously, with two guarantees the old inline `go func()`
// lacked:
//
//   - bounded concurrency (audit #20): at most cap(sem) updates are processed
//     at once, so a burst of updates cannot spawn thousands of goroutines all
//     fighting for the (bounded) DB pool;
//   - graceful shutdown (audit #19): every in-flight update is tracked by a
//     WaitGroup; Shutdown stops accepting new updates and waits for the
//     running ones (with a timeout) before main returns, so an answer that
//     was already acknowledged to Telegram is not lost on deploy.
type updateDispatcher struct {
	secret  string
	handle  func(ctx context.Context, upd *bot.Update)
	timeout time.Duration

	// base is the parent context of every update; cancelled only when the
	// graceful wait times out (last resort to unblock stuck handlers).
	base       context.Context
	cancelBase context.CancelFunc

	sem chan struct{}
	wg  sync.WaitGroup

	mu      sync.Mutex
	closing bool
}

func newUpdateDispatcher(secret string, maxConcurrent int, timeout time.Duration, handle func(context.Context, *bot.Update)) *updateDispatcher {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	base, cancel := context.WithCancel(context.Background())
	return &updateDispatcher{
		secret:     secret,
		handle:     handle,
		timeout:    timeout,
		base:       base,
		cancelBase: cancel,
		sem:        make(chan struct{}, maxConcurrent),
	}
}

// ServeHTTP implements POST /telegram/webhook.
func (d *updateDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.secret != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(d.secret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		// A broken connection: Telegram will retry anyway, and that retry is
		// legitimate (the body never arrived).
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var upd bot.Update
	if err := json.Unmarshal(body, &upd); err != nil {
		// Malformed update (audit #29): log it and acknowledge with 200. A
		// non-2xx makes Telegram re-deliver the very same broken payload
		// over and over — it can never succeed.
		log.Printf("webhook: malformed update dropped (%d bytes): %v", len(body), err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if !d.dispatch(&upd) {
		// Shutting down: ask Telegram to re-deliver to the next instance
		// instead of silently dropping the update.
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	// Respond to Telegram immediately; processing continues asynchronously.
	w.WriteHeader(http.StatusOK)
}

// dispatch starts processing of one update. Returns false when the
// dispatcher is shutting down (the update was not accepted).
func (d *updateDispatcher) dispatch(upd *bot.Update) bool {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return false
	}
	d.wg.Add(1) // under mu: never races with Shutdown's Wait
	d.mu.Unlock()

	go func() {
		defer d.wg.Done()
		// Semaphore: wait for a free processing slot (or for a forced stop).
		select {
		case d.sem <- struct{}{}:
		case <-d.base.Done():
			return
		}
		defer func() { <-d.sem }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("webhook: update %d handler panicked: %v", upd.UpdateID, r)
			}
		}()
		// Detached from the request context so processing survives the
		// response; the timeout guards against a hung provider/DB call.
		uctx, cancel := context.WithTimeout(d.base, d.timeout)
		defer cancel()
		d.handle(uctx, upd)
	}()
	return true
}

// Shutdown stops accepting updates and waits for the in-flight ones until
// ctx expires. On timeout the remaining handlers are cancelled and given a
// brief moment to unwind. Returns ctx.Err() when the graceful wait timed out.
func (d *updateDispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		d.cancelBase()
		return nil
	case <-ctx.Done():
		d.cancelBase()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}
}

// setWebhookWithRetry registers the webhook, retrying temporary failures
// (Telegram / network unavailable) with exponential backoff instead of
// crashing the process into a restart loop (audit #22). It returns only on
// success or when ctx is cancelled (shutdown).
func setWebhookWithRetry(ctx context.Context, set func(context.Context) error, initial, maxDelay time.Duration) error {
	delay := initial
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := set(cctx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("webhook: setWebhook attempt %d failed: %v — retrying in %s", attempt, err, delay)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay *= 2
		if delay > maxDelay {
			delay = maxDelay
		}
	}
}

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// updateDispatcher receives Telegram webhook requests and processes the
// updates asynchronously:
//
//   - bounded concurrency (audit #20): exactly `workers` long-lived worker
//     goroutines process updates, so a burst can never spawn thousands of
//     goroutines fighting for the (bounded) DB pool;
//   - bounded queue (R-3): accepted-but-not-yet-processed updates wait in a
//     channel of `queueSize` slots. Before this fix every update got its own
//     goroutine BEFORE acquiring the semaphore, so under a burst goroutines
//     piled up without any limit. When the queue is full the webhook answers
//     HTTP 503 immediately — Telegram keeps the update and re-delivers it
//     later (any non-2xx is retried by Telegram), so nothing is lost and the
//     request never blocks;
//   - graceful shutdown (audit #19): Shutdown stops accepting updates (503),
//     lets the workers drain everything already acknowledged with 200 and
//     waits for them (with a timeout) before main returns.
type updateDispatcher struct {
	secret  string
	handle  func(ctx context.Context, upd *bot.Update)
	timeout time.Duration

	// base is the parent context of every update; cancelled only when the
	// graceful wait times out (last resort to unblock stuck handlers).
	base       context.Context
	cancelBase context.CancelFunc

	queue chan *bot.Update
	wg    sync.WaitGroup // workers

	mu      sync.Mutex // guards closing + sends on queue (never send on a closed channel)
	closing bool

	rejected atomic.Int64 // updates refused with 503 because the queue was full
	lastWarn atomic.Int64 // unix seconds of the last overflow log line
}

// defaultUpdateQueueSize is the bounded webhook queue (R-3).
const defaultUpdateQueueSize = 1000

func newUpdateDispatcher(secret string, workers, queueSize int, timeout time.Duration, handle func(context.Context, *bot.Update)) *updateDispatcher {
	if workers <= 0 {
		workers = 1
	}
	if queueSize <= 0 {
		queueSize = 1
	}
	base, cancel := context.WithCancel(context.Background())
	d := &updateDispatcher{
		secret:     secret,
		handle:     handle,
		timeout:    timeout,
		base:       base,
		cancelBase: cancel,
		queue:      make(chan *bot.Update, queueSize),
	}
	d.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go d.worker()
	}
	return d
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
	switch d.enqueue(&upd) {
	case enqueued:
		// Respond to Telegram immediately; processing continues asynchronously.
		w.WriteHeader(http.StatusOK)
	case queueFull:
		// Overloaded: ask Telegram to re-deliver later instead of piling up
		// goroutines or blocking the request (R-3).
		w.Header().Set("Retry-After", "1")
		http.Error(w, "busy", http.StatusServiceUnavailable)
	default: // shuttingDown
		// Ask Telegram to re-deliver to the next instance instead of
		// silently dropping the update.
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
	}
}

type enqueueResult int

const (
	enqueued enqueueResult = iota
	queueFull
	shuttingDown
)

// enqueue puts one update into the bounded queue without blocking.
func (d *updateDispatcher) enqueue(upd *bot.Update) enqueueResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return shuttingDown
	}
	select {
	case d.queue <- upd:
		return enqueued
	default:
		n := d.rejected.Add(1)
		now := time.Now().Unix()
		if last := d.lastWarn.Load(); now-last >= 10 && d.lastWarn.CompareAndSwap(last, now) {
			log.Printf("webhook: update queue full (%d) — answering 503, Telegram will re-deliver (rejected so far: %d)", cap(d.queue), n)
		}
		return queueFull
	}
}

// worker processes queued updates until the queue is closed and drained.
func (d *updateDispatcher) worker() {
	defer d.wg.Done()
	for upd := range d.queue {
		if d.base.Err() != nil {
			continue // forced stop: drop what is left, do not start new work
		}
		d.process(upd)
	}
}

func (d *updateDispatcher) process(upd *bot.Update) {
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
}

// QueueLen reports the number of updates waiting for a worker.
func (d *updateDispatcher) QueueLen() int { return len(d.queue) }

// Shutdown stops accepting updates and waits for the queued and in-flight
// ones until ctx expires. On timeout the remaining handlers are cancelled
// (queued updates are dropped) and given a brief moment to unwind. Returns
// ctx.Err() when the graceful wait timed out.
func (d *updateDispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if !d.closing {
		d.closing = true
		close(d.queue) // workers drain the rest and exit
	}
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

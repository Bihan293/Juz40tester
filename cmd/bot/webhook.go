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
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/updq"
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
	// preCheckout (optional) answers a pre_checkout_query synchronously in
	// the webhook request instead of queueing it: Telegram waits at most
	// 10 s, a full or busy queue would make the payment fail.
	preCheckout func(ctx context.Context, q *bot.PreCheckoutQuery)

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

	// seen remembers recently accepted update_ids: Telegram re-delivers an
	// update when our 200 got lost (timeout, deploy) — the duplicate is
	// acknowledged without processing it twice (idempotency).
	seen *recentIDs

	// spill (optional) persists updates that were acknowledged with 200
	// but cannot be processed before the shutdown deadline. Before, they
	// were silently dropped: Telegram already had its 200, so the user's
	// answer / tap was lost for good (a deploy or a restart during a busy
	// minute lost up to the whole backlog). Spilled updates are stored in
	// tg_update_queue and replayed by the next instance (replaySpilled).
	spill    updateSpiller
	spilling atomic.Bool
	spilled  atomic.Int64 // updates handed to spill
	dropped  atomic.Int64 // updates lost (no spill, or the spill failed and time ran out)
}

// updateSpiller stores an update for a later instance (updq.Postgres).
type updateSpiller interface {
	Enqueue(ctx context.Context, updateID, userKey int64, payload []byte) (bool, error)
}

// spillReserve is the part of the shutdown grace period kept for spilling
// the not-yet-processed updates to the database.
var spillReserve = 5 * time.Second

// recentIDs is a bounded set of the last N update ids (ring buffer + map).
type recentIDs struct {
	mu   sync.Mutex
	set  map[int64]struct{}
	ring []int64
	pos  int
}

func newRecentIDs(n int) *recentIDs {
	return &recentIDs{set: make(map[int64]struct{}, n), ring: make([]int64, n)}
}

// add records id and reports whether it was NEW (false = duplicate).
func (r *recentIDs) add(id int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.set[id]; dup {
		return false
	}
	if old := r.ring[r.pos]; old != 0 {
		delete(r.set, old)
	}
	r.ring[r.pos] = id
	r.set[id] = struct{}{}
	r.pos = (r.pos + 1) % len(r.ring)
	return true
}

// forget removes id (an update that was NOT accepted must stay deliverable).
func (r *recentIDs) forget(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.set, id)
}

// recentUpdateIDs is how many update ids the dedupe window remembers.
const recentUpdateIDs = 10000

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
		seen:       newRecentIDs(recentUpdateIDs),
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
	if upd.UpdateID != 0 && !d.seen.add(int64(upd.UpdateID)) {
		metrics.Inc(metrics.DuplicateUpdates)
		w.WriteHeader(http.StatusOK) // already accepted once — do not process twice
		return
	}
	if upd.PreCheckoutQuery != nil && d.preCheckout != nil {
		updq.AnswerPreCheckoutNow(r.Context(), &upd, d.preCheckout)
		w.WriteHeader(http.StatusOK)
		return
	}
	res := d.enqueue(&upd)
	if res != enqueued && upd.UpdateID != 0 {
		d.seen.forget(int64(upd.UpdateID)) // 503: Telegram re-delivers it, it must be processed then
	}
	switch res {
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
		if d.spilling.Load() && d.spillOne(upd) {
			continue // persisted: the next instance processes it
		}
		if d.base.Err() != nil {
			d.dropped.Add(1)
			continue // forced stop: drop what is left, do not start new work
		}
		d.process(upd)
	}
}

// spillOne persists one accepted-but-unprocessed update; false when it
// could not be stored (the caller then processes it, as before).
func (d *updateDispatcher) spillOne(upd *bot.Update) bool {
	if d.spill == nil || upd.UpdateID == 0 {
		return false
	}
	body, err := json.Marshal(upd)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.spill.Enqueue(ctx, upd.UpdateID, upd.UserKey(), body); err != nil {
		log.Printf("webhook: spill of update %d failed: %v", upd.UpdateID, err)
		return false
	}
	d.spilled.Add(1)
	return true
}

// enqueueReplayed puts an update spilled by an earlier instance into the
// queue (it was acknowledged long ago — it bypasses the webhook path).
func (d *updateDispatcher) enqueueReplayed(upd *bot.Update) enqueueResult {
	if upd.UpdateID != 0 && !d.seen.add(upd.UpdateID) {
		return enqueued // Telegram re-delivered it meanwhile: already queued
	}
	res := d.enqueue(upd)
	if res != enqueued && upd.UpdateID != 0 {
		d.seen.forget(upd.UpdateID)
	}
	return res
}

// freeSlots is how many updates the queue can take right now.
func (d *updateDispatcher) freeSlots() int { return cap(d.queue) - len(d.queue) }

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
	defer func() {
		if n := d.spilled.Load(); n > 0 {
			log.Printf("shutdown: %d acknowledged update(s) saved to tg_update_queue — the next instance processes them", n)
		}
		if n := d.dropped.Load(); n > 0 {
			log.Printf("shutdown: %d acknowledged update(s) were DROPPED unprocessed", n)
		}
	}()
	if d.spill != nil {
		// Process locally while there is time; spillReserve before the
		// deadline every update not started yet is persisted instead.
		var spillAt <-chan time.Time
		if dl, ok := ctx.Deadline(); ok {
			t := time.NewTimer(max(time.Until(dl)-spillReserve, 0))
			defer t.Stop()
			spillAt = t.C
		}
		select {
		case <-done:
			d.cancelBase()
			return nil
		case <-spillAt:
			d.spilling.Store(true)
		case <-ctx.Done():
			d.spilling.Store(true)
		}
	}
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

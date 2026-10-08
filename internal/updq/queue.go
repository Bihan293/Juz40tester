// Package updq is the durable Telegram update queue of the cluster mode
// (ROLE=web|worker): the web process puts every accepted webhook update
// into the queue and answers 200 at once, worker processes take the
// updates from it and handle them.
//
// Guarantees of every implementation (PostgreSQL — the default — and Redis):
//
//   - idempotency by update_id in the shared store: an update re-delivered
//     by Telegram (or received by two web instances) is queued once;
//   - per-user ordering: two updates of the same user (UserKey) are never
//     handled at the same time, and they are handled in update_id order;
//   - at-least-once delivery with a lease: a claimed update is kept alive
//     by heartbeats; when its worker dies the reaper returns it to the queue;
//   - retries with backoff and a dead-letter state after MaxAttempts;
//   - backpressure: Enqueue refuses new updates (ErrQueueFull → HTTP 503,
//     Telegram re-delivers later) when too many are waiting.
package updq

import (
	"context"
	"errors"
	"time"
)

// ErrQueueFull is returned by Enqueue when the queue holds its maximum.
var ErrQueueFull = errors.New("update queue is full")

// Item is one claimed update.
type Item struct {
	UpdateID int64
	UserKey  int64
	Payload  []byte // the update JSON as received from Telegram
	// Attempts counts the claims of this update including the current one.
	Attempts int
	// Token identifies the claim (fencing): Complete / Fail / Release /
	// Heartbeat of a claim that was reaped and re-claimed by another worker
	// are no-ops.
	Token string
}

// Stats is a snapshot of the queue (metrics, /health).
type Stats struct {
	Pending    int64 // waiting (incl. retry backoff)
	Processing int64
	Dead       int64
}

// Queue is the durable update queue.
type Queue interface {
	// Enqueue stores the update. inserted=false means it is a duplicate of
	// an update already accepted (idempotent no-op). ErrQueueFull when the
	// backlog reached its maximum (the update is NOT recorded).
	Enqueue(ctx context.Context, updateID, userKey int64, payload []byte) (inserted bool, err error)
	// Claim takes up to n updates that may be handled now (oldest first,
	// at most one per user, none of a user whose earlier update is still
	// open). worker names the claimer (logs, locks).
	Claim(ctx context.Context, worker string, n int) ([]Item, error)
	// Complete marks the update handled.
	Complete(ctx context.Context, it Item) error
	// Fail records a failed attempt: the update is retried after retryIn,
	// or moved to the dead letters when it has used maxAttempts. Returns
	// dead=true in the latter case.
	Fail(ctx context.Context, it Item, cause error, retryIn time.Duration, maxAttempts int) (dead bool, err error)
	// Release hands a claimed update back without counting the attempt
	// (graceful shutdown).
	Release(ctx context.Context, it Item) error
	// Heartbeat extends the lease of claimed updates.
	Heartbeat(ctx context.Context, worker string, items []Item) error
	// Reap returns updates whose lease expired (no heartbeat for staleAfter)
	// to the queue; those that already used maxAttempts become dead.
	Reap(ctx context.Context, staleAfter time.Duration, maxAttempts int) (requeued, dead int, err error)
	// Purge deletes handled updates older than doneAge and dead letters
	// older than deadAge (no-op where the store expires them itself).
	Purge(ctx context.Context, doneAge, deadAge time.Duration) (int64, error)
	// Stats returns the current queue sizes.
	Stats(ctx context.Context) (Stats, error)
}

// backoff is the retry delay of attempt n (1-based): 2s, 4s, 8s … ≤ 1 min.
func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		return time.Minute
	}
	return time.Duration(1<<attempt) * time.Second
}

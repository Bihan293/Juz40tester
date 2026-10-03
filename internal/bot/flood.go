package bot

import (
	"context"
	"log"
	"sync"
	"time"
)

// defaultMaxDeferred bounds the number of requests waiting in the
// deferred-retry queue across all chats. When it is full a rate-limited
// request fails (RateLimitError, not deferred) instead of growing memory.
const defaultMaxDeferred = 1000

// deferredSendTimeout bounds one deferred HTTP request.
const deferredSendTimeout = 20 * time.Second

// floodControl implements the R-3 handling of Telegram 429:
//
//   - blockedUntil remembers, per chat, the retry_after window of the last
//     429 so further requests to that chat do not hit the API in vain;
//   - deferred requests are kept in per-chat FIFO queues and re-sent by ONE
//     drainer goroutine per chat that has queued work. These goroutines are
//     NOT the webhook handler slots — waiting out retry_after there never
//     blocks the processing of other users' updates. Their number is
//     bounded by the number of chats with queued work, and the total queue
//     by max.
//
// State is per process (in memory): after a restart the queue is gone —
// acceptable for UI messages, which are best-effort anyway.
type floodControl struct {
	max int

	mu           sync.Mutex
	blockedUntil map[int64]time.Time
	queues       map[int64][]deferredReq
	pending      int

	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool

	now func() time.Time
}

type deferredReq struct {
	method   string
	body     []byte
	attempts int
}

func newFloodControl(max int) *floodControl {
	base, cancel := context.WithCancel(context.Background())
	return &floodControl{
		max:          max,
		blockedUntil: map[int64]time.Time{},
		queues:       map[int64][]deferredReq{},
		base:         base,
		cancel:       cancel,
		now:          time.Now,
	}
}

// blocked returns the remaining ban of the chat and whether requests of the
// chat are already queued (a new one must then queue behind them to keep
// the message order).
func (f *floodControl) blocked(chatID int64) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	wait := time.Duration(0)
	if until, ok := f.blockedUntil[chatID]; ok {
		if d := until.Sub(f.now()); d > 0 {
			wait = d
		} else {
			delete(f.blockedUntil, chatID)
		}
	}
	return wait, len(f.queues[chatID]) > 0
}

// block records a 429 ban for the chat.
func (f *floodControl) block(chatID int64, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	until := f.now().Add(d)
	if cur, ok := f.blockedUntil[chatID]; !ok || until.After(cur) {
		f.blockedUntil[chatID] = until
	}
	// Opportunistic cleanup so the map does not grow forever.
	if len(f.blockedUntil) > 4*f.max {
		now := f.now()
		for id, u := range f.blockedUntil {
			if !u.After(now) {
				delete(f.blockedUntil, id)
			}
		}
	}
}

// enqueue queues a request for a deferred re-send. Returns false when the
// queue is full or closed (the caller then reports the 429 as a failure).
func (f *floodControl) enqueue(c *Client, chatID int64, method string, body []byte, wait time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.pending >= f.max {
		return false
	}
	if wait > 0 {
		until := f.now().Add(wait)
		if cur, ok := f.blockedUntil[chatID]; !ok || until.After(cur) {
			f.blockedUntil[chatID] = until
		}
	}
	q := f.queues[chatID]
	f.queues[chatID] = append(q, deferredReq{method: method, body: body})
	f.pending++
	if len(q) == 0 { // no drainer for this chat yet
		f.wg.Add(1)
		go f.drain(c, chatID)
	}
	return true
}

// Pending returns the number of queued deferred requests.
func (f *floodControl) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

// drain re-sends the queued requests of one chat in order, waiting out the
// chat's ban before each one. It exits when the queue of the chat is empty.
func (f *floodControl) drain(c *Client, chatID int64) {
	defer f.wg.Done()
	for {
		f.mu.Lock()
		q := f.queues[chatID]
		if len(q) == 0 {
			delete(f.queues, chatID)
			f.mu.Unlock()
			return
		}
		req := q[0]
		wait := time.Duration(0)
		if until, ok := f.blockedUntil[chatID]; ok {
			wait = until.Sub(f.now())
		}
		f.mu.Unlock()

		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-f.base.Done():
				t.Stop()
				f.dropChat(chatID)
				return
			case <-t.C:
			}
		}
		ctx, cancel := context.WithTimeout(f.base, deferredSendTimeout)
		retryAfter, err := c.callOnce(ctx, req.method, req.body, nil)
		cancel()

		f.mu.Lock()
		if retryAfter > 0 && retryAfter <= maxRetryAfter && req.attempts < maxRateLimitRetries && !f.closed {
			// Still limited: keep it at the head, extend the ban.
			f.queues[chatID][0].attempts++
			f.blockedUntil[chatID] = f.now().Add(retryAfter)
			f.mu.Unlock()
			continue
		}
		f.queues[chatID] = f.queues[chatID][1:]
		f.pending--
		f.mu.Unlock()
		if retryAfter > 0 {
			log.Printf("telegram %s: deferred request dropped — still rate limited (retry after %s)", req.method, retryAfter)
		} else if err != nil {
			log.Printf("telegram deferred %v", err)
		}
	}
}

func (f *floodControl) dropChat(chatID int64) {
	f.mu.Lock()
	f.pending -= len(f.queues[chatID])
	delete(f.queues, chatID)
	f.mu.Unlock()
}

// close stops accepting deferred requests and waits for the queued ones
// until ctx expires; then the remaining ones are dropped.
func (f *floodControl) close(ctx context.Context) error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
		f.cancel()
		return nil
	case <-ctx.Done():
		f.cancel()
		<-done
		return ctx.Err()
	}
}

// Close flushes the deferred-retry queue (waiting at most until ctx ends)
// and stops its goroutines. Call it on shutdown, after the update handlers
// have finished.
func (c *Client) Close(ctx context.Context) error {
	if c.flood == nil {
		return nil
	}
	return c.flood.close(ctx)
}

// PendingDeferred returns how many rate-limited requests wait for re-send.
func (c *Client) PendingDeferred() int {
	if c.flood == nil {
		return 0
	}
	return c.flood.Pending()
}

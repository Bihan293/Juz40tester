package updq

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/metrics"
)

// Metric names of the update queue.
const (
	MetricProcessed = "tg_updates_processed_total" // label result=ok|retry|dead|dropped|released
	MetricReaped    = "tg_updates_reaped_total"    // label result=requeued|dead
	MetricEnqueued  = "tg_updates_enqueued_total"  // label result=new|duplicate|full|error|ignored
	GaugePending    = "tg_update_queue_pending"
	GaugeProcessing = "tg_update_queue_processing"
	GaugeDead       = "tg_update_queue_dead"
)

// ConsumerConfig configures a Consumer.
type ConsumerConfig struct {
	Worker      string        // instance id (claim owner)
	Workers     int           // updates handled at once
	Timeout     time.Duration // per-update handler timeout
	Lease       time.Duration // claim lease (heartbeat every Lease/3)
	MaxAttempts int           // then dead letter
	Poll        time.Duration // fallback poll when no wake-up arrives (default 3 s)
	DoneTTL     time.Duration // purge of handled updates
	DeadTTL     time.Duration // purge of dead letters
}

// HandleFunc handles one update; a non-nil error retries it.
type HandleFunc func(ctx context.Context, upd *bot.Update) error

// Consumer takes updates from a Queue and handles them with a fixed pool
// of workers. Idle workers sleep until Wake (LISTEN/NOTIFY or Redis
// pub/sub) or the fallback poll; a worker that got an update wakes the
// next one, so a burst fans out over the whole pool.
type Consumer struct {
	q      Queue
	cfg    ConsumerConfig
	handle HandleFunc

	wake chan struct{}

	// base is the parent context of the handlers — NOT the Run context, so
	// the updates in flight finish during a graceful shutdown; cancelled
	// only when the grace period is over (their updates are released).
	base       context.Context
	cancelBase context.CancelFunc

	mu       sync.Mutex
	inflight map[claimKey]Item

	wg       sync.WaitGroup
	errLast  atomic.Int64
	sleepFor func(context.Context, time.Duration)
}

// NewConsumer creates a consumer (call Run to start it).
func NewConsumer(q Queue, cfg ConsumerConfig, handle HandleFunc) *Consumer {
	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Minute
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 45 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 3 * time.Second
	}
	base, cancel := context.WithCancel(context.Background())
	return &Consumer{
		q: q, cfg: cfg, handle: handle,
		wake: make(chan struct{}, cfg.Workers),
		base: base, cancelBase: cancel,
		inflight: map[claimKey]Item{},
		sleepFor: func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
	}
}

// Wake wakes one idle worker (non-blocking). Safe from any goroutine.
func (c *Consumer) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// InFlight returns the number of updates being handled right now.
func (c *Consumer) InFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inflight)
}

// Run starts the workers, the heartbeat, the reaper and the purge, and
// blocks until ctx is cancelled and the workers have stopped. Updates in
// flight keep running after ctx ends; Shutdown bounds that wait.
func (c *Consumer) Run(ctx context.Context) {
	c.wg.Add(c.cfg.Workers) // before the heartbeat loop waits on it
	var aux sync.WaitGroup
	aux.Add(3)
	go func() { defer aux.Done(); c.heartbeatLoop(ctx) }()
	go func() { defer aux.Done(); c.reapLoop(ctx) }()
	go func() { defer aux.Done(); c.pollLoop(ctx) }()
	for i := 0; i < c.cfg.Workers; i++ {
		go c.worker(ctx)
	}
	c.wg.Wait()
	aux.Wait()
}

// Shutdown waits (until ctx expires) for the updates in flight after the
// Run context was cancelled; then cancels the remaining handlers — their
// updates are released back to the queue without counting the attempt.
func (c *Consumer) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		c.cancelBase()
		return nil
	case <-ctx.Done():
		c.cancelBase()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		return ctx.Err()
	}
}

func (c *Consumer) worker(ctx context.Context) {
	defer c.wg.Done()
	for ctx.Err() == nil {
		items, err := c.q.Claim(ctx, c.cfg.Worker, 1)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logErr("claim", err)
			c.sleepFor(ctx, time.Second)
			continue
		}
		if len(items) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		c.Wake() // fan out: maybe more updates are waiting
		for _, it := range items {
			c.process(it)
		}
	}
}

// claimKey identifies one CLAIM of an update. The same update may be held
// twice by this process for a moment (its lease expired, the reaper
// returned it and another worker of this instance claimed it again while
// the first handler was still running): keyed by update_id alone, the
// first handler's cleanup deleted the second claim from inflight, its
// heartbeats stopped and the update was reaped and handled once more.
type claimKey struct {
	id       int64
	attempts int
	token    string
}

func keyOf(it Item) claimKey { return claimKey{it.UpdateID, it.Attempts, it.Token} }

// process handles one claimed update and records the outcome.
func (c *Consumer) process(it Item) {
	c.mu.Lock()
	c.inflight[keyOf(it)] = it
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.inflight, keyOf(it))
		c.mu.Unlock()
	}()

	var upd bot.Update
	if err := json.Unmarshal(it.Payload, &upd); err != nil {
		// Validated by the web role — cannot normally happen; never retry it.
		log.Printf("update queue: update %d has a malformed payload, dropped: %v", it.UpdateID, err)
		metrics.Inc(MetricProcessed, "result", "dropped")
		c.finish(func(ctx context.Context) error { return c.q.Complete(ctx, it) })
		return
	}
	herr := c.run(&upd)
	if c.base.Err() != nil {
		// Forced stop at the end of the shutdown grace period: the handler
		// was cut short — hand the update back, the attempt does not count.
		metrics.Inc(MetricProcessed, "result", "released")
		c.finish(func(ctx context.Context) error { return c.q.Release(ctx, it) })
		return
	}
	if herr == nil {
		metrics.Inc(MetricProcessed, "result", "ok")
		c.finish(func(ctx context.Context) error { return c.q.Complete(ctx, it) })
		return
	}
	var dead bool
	c.finish(func(ctx context.Context) error {
		var err error
		dead, err = c.q.Fail(ctx, it, herr, backoff(it.Attempts), c.cfg.MaxAttempts)
		return err
	})
	if dead {
		metrics.Inc(MetricProcessed, "result", "dead")
		log.Printf("update queue: update %d (user %d) moved to dead letters after %d attempt(s): %v",
			it.UpdateID, it.UserKey, it.Attempts, herr)
	} else {
		metrics.Inc(MetricProcessed, "result", "retry")
		log.Printf("update queue: update %d (user %d) failed (attempt %d/%d), retry in %s: %v",
			it.UpdateID, it.UserKey, it.Attempts, c.cfg.MaxAttempts, backoff(it.Attempts), herr)
	}
}

func (c *Consumer) run(upd *bot.Update) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("handler panicked")
			log.Printf("update queue: update %d handler panicked: %v", upd.UpdateID, r)
		}
	}()
	ctx, cancel := context.WithTimeout(c.base, c.cfg.Timeout)
	defer cancel()
	return c.handle(ctx, upd)
}

// finish runs a queue bookkeeping call with its own short context (the
// Run context may already be cancelled during shutdown).
func (c *Consumer) finish(f func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f(ctx); err != nil {
		c.logErr("ack", err)
	}
}

func (c *Consumer) snapshot() []Item {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Item, 0, len(c.inflight))
	for _, it := range c.inflight {
		out = append(out, it)
	}
	return out
}

// heartbeatLoop extends the lease of the updates in flight every Lease/3.
// It keeps running during the shutdown grace period (the updates are still
// being handled) and stops once the workers are gone.
func (c *Consumer) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(max(c.cfg.Lease/3, time.Second))
	defer t.Stop()
	stopped := make(chan struct{})
	go func() { c.wg.Wait(); close(stopped) }()
	for {
		select {
		case <-stopped:
			return
		case <-t.C:
		}
		items := c.snapshot()
		if len(items) == 0 {
			continue
		}
		hctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.q.Heartbeat(hctx, c.cfg.Worker, items); err != nil {
			c.logErr("heartbeat", err)
		}
		cancel()
	}
}

// reapLoop returns updates of dead workers to the queue (every Lease/2),
// refreshes the queue gauges and purges old rows (hourly).
func (c *Consumer) reapLoop(ctx context.Context) {
	t := time.NewTicker(max(c.cfg.Lease/2, 5*time.Second))
	defer t.Stop()
	lastPurge := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, dead, err := c.q.Reap(rctx, c.cfg.Lease, c.cfg.MaxAttempts)
		if err != nil {
			c.logErr("reap", err)
		} else if req+dead > 0 {
			metrics.Add(MetricReaped, float64(req), "result", "requeued")
			metrics.Add(MetricReaped, float64(dead), "result", "dead")
			log.Printf("update queue: reaper returned %d update(s) of stopped workers to the queue, %d moved to dead letters", req, dead)
			c.Wake()
		}
		if st, err := c.q.Stats(rctx); err == nil {
			metrics.SetGauge(GaugePending, float64(st.Pending))
			metrics.SetGauge(GaugeProcessing, float64(st.Processing))
			metrics.SetGauge(GaugeDead, float64(st.Dead))
		}
		if time.Since(lastPurge) >= time.Hour {
			lastPurge = time.Now()
			if n, err := c.q.Purge(rctx, c.cfg.DoneTTL, c.cfg.DeadTTL); err != nil {
				c.logErr("purge", err)
			} else if n > 0 {
				log.Printf("update queue: purged %d old row(s)", n)
			}
		}
		cancel()
	}
}

// pollLoop is the fallback when a wake-up is lost (listener reconnecting).
func (c *Consumer) pollLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Wake()
		}
	}
}

// logErr logs queue errors at most every 10 s (a DB outage must not flood
// the log from every worker).
func (c *Consumer) logErr(op string, err error) {
	now := time.Now().Unix()
	if last := c.errLast.Load(); now-last >= 10 && c.errLast.CompareAndSwap(last, now) {
		log.Printf("update queue: %s: %v", op, err)
	}
}

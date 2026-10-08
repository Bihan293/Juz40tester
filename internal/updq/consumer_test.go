package updq

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/metrics"
)

func miniQueue(t *testing.T, max int) *Redis {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, RedisOptions{Prefix: "c", MaxBacklog: max, Lease: 3 * time.Second})
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

// TestConsumerOrderAndParallelism: 8 workers handle 3 users × 10 updates;
// users run in parallel, one user's updates never overlap and keep order.
func TestConsumerOrderAndParallelism(t *testing.T) {
	q := miniQueue(t, 0)
	var mu sync.Mutex
	busy := map[int64]bool{}
	order := map[int64][]int64{}
	var overlap, parallel atomic.Int32
	var running atomic.Int32
	handle := func(ctx context.Context, upd *bot.Update) error {
		uk := upd.UserKey()
		mu.Lock()
		if busy[uk] {
			overlap.Add(1)
		}
		busy[uk] = true
		order[uk] = append(order[uk], upd.UpdateID)
		mu.Unlock()
		if running.Add(1) > 1 {
			parallel.Add(1)
		}
		time.Sleep(3 * time.Millisecond)
		running.Add(-1)
		mu.Lock()
		busy[uk] = false
		mu.Unlock()
		return nil
	}
	c := NewConsumer(q, ConsumerConfig{Worker: "t", Workers: 8, Poll: 20 * time.Millisecond}, handle)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	id := int64(0)
	for i := 0; i < 10; i++ {
		for u := int64(1); u <= 3; u++ {
			id++
			body := `{"update_id":` + itoa(id) + `,"message":{"message_id":1,"from":{"id":` + itoa(u) + `},"chat":{"id":` + itoa(u) + `,"type":"private"},"text":"x"}}`
			if _, err := q.Enqueue(context.Background(), id, u, []byte(body)); err != nil {
				t.Fatal(err)
			}
			c.Wake()
		}
	}
	waitFor(t, 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, o := range order {
			n += len(o)
		}
		return n == 30
	})
	cancel()
	<-done
	if overlap.Load() != 0 {
		t.Fatalf("%d overlapping updates of one user", overlap.Load())
	}
	if parallel.Load() == 0 {
		t.Fatal("different users were never handled in parallel")
	}
	for u, o := range order {
		for i := 1; i < len(o); i++ {
			if o[i] < o[i-1] {
				t.Fatalf("user %d out of order: %v", u, o)
			}
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestConsumerRetryAndDeadLetter: a failing update is retried and then
// dead-lettered; the next update of the same user still runs.
func TestConsumerRetryAndDeadLetter(t *testing.T) {
	q := miniQueue(t, 0)
	var calls atomic.Int32
	var okSeen atomic.Bool
	handle := func(ctx context.Context, upd *bot.Update) error {
		if upd.UpdateID == 1 {
			calls.Add(1)
			return errors.New("boom")
		}
		okSeen.Store(true)
		return nil
	}
	c := NewConsumer(q, ConsumerConfig{Worker: "t", Workers: 2, MaxAttempts: 2, Poll: 10 * time.Millisecond}, handle)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	mustEnqueue(t, q, 1, 5)
	mustEnqueue(t, q, 2, 5)
	// attempt 1 fails → backoff 2 s → attempt 2 fails → dead → update 2 runs.
	waitFor(t, 10*time.Second, func() bool { return okSeen.Load() })
	if calls.Load() != 2 {
		t.Fatalf("failing update ran %d times, want 2", calls.Load())
	}
	st, _ := q.Stats(context.Background())
	if st.Dead != 1 {
		t.Fatalf("stats %+v", st)
	}
}

// TestConsumerPanicIsRetried: a panicking handler does not kill the worker.
func TestConsumerPanicIsRetried(t *testing.T) {
	q := miniQueue(t, 0)
	var n atomic.Int32
	handle := func(ctx context.Context, upd *bot.Update) error {
		if n.Add(1) == 1 {
			panic("first try explodes")
		}
		return nil
	}
	c := NewConsumer(q, ConsumerConfig{Worker: "t", Workers: 1, MaxAttempts: 3, Poll: 10 * time.Millisecond}, handle)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	mustEnqueue(t, q, 9, 9)
	waitFor(t, 10*time.Second, func() bool {
		st, _ := q.Stats(context.Background())
		return n.Load() >= 2 && st.Pending == 0 && st.Processing == 0
	})
}

// TestConsumerShutdownReleases: an update still running when the grace
// period ends is released (attempt not counted) for another worker.
func TestConsumerShutdownReleases(t *testing.T) {
	q := miniQueue(t, 0)
	started := make(chan struct{})
	handle := func(ctx context.Context, upd *bot.Update) error {
		close(started)
		<-ctx.Done() // a long handler that only ends when cancelled
		return nil
	}
	c := NewConsumer(q, ConsumerConfig{Worker: "t", Workers: 1, Poll: 10 * time.Millisecond}, handle)
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	mustEnqueue(t, q, 3, 3)
	c.Wake()
	<-started
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer scancel()
	if err := c.Shutdown(sctx); err == nil {
		t.Fatal("expected the grace period to run out")
	}
	waitFor(t, 5*time.Second, func() bool {
		st, _ := q.Stats(context.Background())
		return st.Processing == 0 && st.Pending == 1
	})
	items := claimAll(t, q, "other")
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("released update claimed as %+v (attempt must not count)", items)
	}
}

// TestConsumerHeartbeatKeepsLease: a handler longer than the lease is not
// reaped while its worker heart-beats.
func TestConsumerHeartbeatKeepsLease(t *testing.T) {
	q := miniQueue(t, 0) // lease 3 s
	var runs atomic.Int32
	handle := func(ctx context.Context, upd *bot.Update) error {
		runs.Add(1)
		time.Sleep(4500 * time.Millisecond)
		return nil
	}
	c := NewConsumer(q, ConsumerConfig{Worker: "t", Workers: 2, Lease: 3 * time.Second, Poll: 10 * time.Millisecond}, handle)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	mustEnqueue(t, q, 4, 4)
	waitFor(t, 10*time.Second, func() bool {
		st, _ := q.Stats(context.Background())
		return runs.Load() >= 1 && st.Pending == 0 && st.Processing == 0
	})
	if runs.Load() != 1 {
		t.Fatalf("update ran %d times — the lease was lost despite heartbeats", runs.Load())
	}
}

// --- Ingress -----------------------------------------------------------------

func post(t *testing.T, h http.Handler, body, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIngress(t *testing.T) {
	q := miniQueue(t, 2)
	in := NewIngress("s3cret", q)
	var woke atomic.Int32
	in.OnEnqueued = func() { woke.Add(1) }
	msg := func(id int64, user int64) string {
		return `{"update_id":` + itoa(id) + `,"message":{"message_id":1,"from":{"id":` + itoa(user) + `},"chat":{"id":1,"type":"private"},"text":"hi"}}`
	}
	if rec := post(t, in, msg(1, 1), "wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong secret: %d", rec.Code)
	}
	if rec := post(t, in, `{not json`, "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("malformed: %d (must be 200, never retried)", rec.Code)
	}
	if rec := post(t, in, `{"update_id":5,"edited_message":{}}`, "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("ignored update: %d", rec.Code)
	}
	if rec := post(t, in, msg(1, 1), "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("new: %d", rec.Code)
	}
	if rec := post(t, in, msg(1, 1), "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	if woke.Load() != 1 {
		t.Fatalf("OnEnqueued called %d times, want 1 (not for the duplicate)", woke.Load())
	}
	if rec := post(t, in, msg(2, 2), "s3cret"); rec.Code != http.StatusOK {
		t.Fatalf("second: %d", rec.Code)
	}
	rec := post(t, in, msg(3, 3), "s3cret")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("full queue: %d (want 503 + Retry-After)", rec.Code)
	}
	st, _ := q.Stats(context.Background())
	if st.Pending != 2 {
		t.Fatalf("queued %d, want 2", st.Pending)
	}
	in.Close()
	if rec := post(t, in, msg(4, 4), "s3cret"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closing: %d", rec.Code)
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 2*time.Second || backoff(3) != 8*time.Second || backoff(10) != time.Minute || backoff(0) != 2*time.Second {
		t.Fatal("unexpected backoff schedule")
	}
}

// A second claim of the same update (after a reap) held by this process
// must stay in flight — and keep being heart-beaten — when the first,
// stale claim finishes.
func TestConsumerInflightPerClaim(t *testing.T) {
	q := miniQueue(t, 0)
	c := NewConsumer(q, ConsumerConfig{Worker: "w"}, func(context.Context, *bot.Update) error { return nil })
	second := Item{UpdateID: 9, UserKey: 1, Attempts: 2, Token: "w:b", Payload: []byte(`{"update_id":9}`)}
	c.mu.Lock()
	c.inflight[keyOf(second)] = second
	c.mu.Unlock()
	c.process(Item{UpdateID: 9, UserKey: 1, Attempts: 1, Token: "w:a", Payload: []byte(`{"update_id":9}`)})
	if got := c.snapshot(); len(got) != 1 || got[0].Token != "w:b" {
		t.Fatalf("in flight after the stale claim finished = %+v, want the second claim", got)
	}
}

// pre_checkout_query never waits in the queue: the ingress answers it in
// the request, even when the queue is full or the user has updates
// waiting (per-user order would hold it behind them).
func TestIngressPreCheckoutFastPath(t *testing.T) {
	q := miniQueue(t, 1)
	in := NewIngress("", q)
	var answered atomic.Int32
	var hadDeadline atomic.Bool
	in.PreCheckout = func(ctx context.Context, pq *bot.PreCheckoutQuery) {
		_, ok := ctx.Deadline()
		hadDeadline.Store(ok && ctx.Err() == nil)
		if pq.ID == "pq1" && pq.From.ID == 7 {
			answered.Add(1)
		}
	}
	// User 7 already has an update waiting and the queue is full.
	mustEnqueue(t, q, 1, 7)
	rec := post(t, in, `{"update_id":2,"pre_checkout_query":{"id":"pq1","from":{"id":7},"currency":"XTR","total_amount":10,"invoice_payload":"sub:plus"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-checkout: %d, want 200", rec.Code)
	}
	if answered.Load() != 1 || !hadDeadline.Load() {
		t.Fatalf("answered %d time(s), deadline %v — want 1 synchronous answer with its own deadline", answered.Load(), hadDeadline.Load())
	}
	if st, _ := q.Stats(context.Background()); st.Pending != 1 {
		t.Fatalf("queue %+v — the pre-checkout must not be queued", st)
	}
	// Without a fast path (nil) it is queued as before.
	in.PreCheckout = nil
	if rec := post(t, in, `{"update_id":3,"pre_checkout_query":{"id":"pq2","from":{"id":8}}}`, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("queued pre-checkout with a full queue: %d, want 503", rec.Code)
	}
}

// A successful_payment that ends in the dead letters is reported loudly
// (metric + log), never lost silently.
func TestConsumerDeadPaymentIsCounted(t *testing.T) {
	q := miniQueue(t, 0)
	before := metrics.Get(MetricPaymentDead)
	c := NewConsumer(q, ConsumerConfig{Worker: "t", Workers: 1, MaxAttempts: 1, Poll: 10 * time.Millisecond},
		func(context.Context, *bot.Update) error { return errors.New("db down") })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	body := []byte(`{"update_id":11,"message":{"message_id":1,"from":{"id":3},"chat":{"id":3,"type":"private"},"successful_payment":{"currency":"XTR","total_amount":10,"invoice_payload":"sub:plus","telegram_payment_charge_id":"ch1"}}}`)
	if _, err := q.Enqueue(context.Background(), 11, 3, body); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return metrics.Get(MetricPaymentDead) > before })
}

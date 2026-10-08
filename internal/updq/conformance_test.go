package updq

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// queueFactory builds an EMPTY queue with the given backlog limit.
type queueFactory func(t *testing.T, maxBacklog int) Queue

// runConformance runs the behaviour every Queue implementation must have.
func runConformance(t *testing.T, mk queueFactory) {
	t.Run("idempotent", func(t *testing.T) { testIdempotent(t, mk(t, 0)) })
	t.Run("per_user_order", func(t *testing.T) { testPerUserOrder(t, mk(t, 0)) })
	t.Run("concurrent_claims_serialise_users", func(t *testing.T) { testConcurrentClaims(t, mk(t, 0)) })
	t.Run("retry_then_dead", func(t *testing.T) { testRetryThenDead(t, mk(t, 0)) })
	t.Run("release_does_not_count", func(t *testing.T) { testRelease(t, mk(t, 0)) })
	t.Run("backpressure", func(t *testing.T) { testBackpressure(t, mk(t, 3)) })
	t.Run("fencing", func(t *testing.T) { testFencing(t, mk(t, 0)) })
}

func payload(id int64) []byte {
	return []byte(fmt.Sprintf(`{"update_id":%d,"message":{"message_id":1,"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"x"}}`, id))
}

func mustEnqueue(t *testing.T, q Queue, id, uk int64) {
	t.Helper()
	ins, err := q.Enqueue(context.Background(), id, uk, payload(id))
	if err != nil || !ins {
		t.Fatalf("enqueue %d: inserted=%v err=%v", id, ins, err)
	}
}

func claimAll(t *testing.T, q Queue, worker string) []Item {
	t.Helper()
	items, err := q.Claim(context.Background(), worker, 100)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return items
}

func testIdempotent(t *testing.T, q Queue) {
	ctx := context.Background()
	mustEnqueue(t, q, 101, 1)
	ins, err := q.Enqueue(ctx, 101, 1, payload(101))
	if err != nil || ins {
		t.Fatalf("duplicate enqueue: inserted=%v err=%v", ins, err)
	}
	items := claimAll(t, q, "w1")
	if len(items) != 1 || items[0].UpdateID != 101 || items[0].Attempts != 1 {
		t.Fatalf("claimed %+v", items)
	}
	if string(items[0].Payload) == "" {
		t.Fatal("payload lost")
	}
	if err := q.Complete(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	// A re-delivery AFTER handling is still a duplicate (idempotency window).
	ins, err = q.Enqueue(ctx, 101, 1, payload(101))
	if err != nil || ins {
		t.Fatalf("re-delivery after done: inserted=%v err=%v", ins, err)
	}
	if got := claimAll(t, q, "w1"); len(got) != 0 {
		t.Fatalf("a handled update was claimed again: %+v", got)
	}
}

func testPerUserOrder(t *testing.T, q Queue) {
	ctx := context.Background()
	// user 1: 201, 203; user 2: 202
	mustEnqueue(t, q, 201, 1)
	mustEnqueue(t, q, 202, 2)
	mustEnqueue(t, q, 203, 1)
	items := claimAll(t, q, "w1")
	ids := []int64{}
	for _, it := range items {
		ids = append(ids, it.UpdateID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if fmt.Sprint(ids) != "[201 202]" {
		t.Fatalf("first claim = %v, want [201 202] (one per user, oldest first)", ids)
	}
	// 203 must not be claimable while 201 is in flight.
	if got := claimAll(t, q, "w2"); len(got) != 0 {
		t.Fatalf("second update of a busy user claimed: %+v", got)
	}
	for _, it := range items {
		if err := q.Complete(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	got := claimAll(t, q, "w2")
	if len(got) != 1 || got[0].UpdateID != 203 {
		t.Fatalf("after completion claimed %+v, want 203", got)
	}
	_ = q.Complete(ctx, got[0])
}

// testConcurrentClaims: 8 workers race over 5 users × 20 updates; no two
// updates of a user may be in flight at once and each user's updates must
// be handled in order.
func testConcurrentClaims(t *testing.T, q Queue) {
	ctx := context.Background()
	const users, per = 5, 20
	id := int64(1000)
	for i := 0; i < per; i++ {
		for u := int64(1); u <= users; u++ {
			id++
			mustEnqueue(t, q, id, u)
		}
	}
	var mu sync.Mutex
	busy := map[int64]bool{}
	order := map[int64][]int64{}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	deadline := time.Now().Add(30 * time.Second)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			idle := 0
			for time.Now().Before(deadline) {
				items, err := q.Claim(ctx, fmt.Sprintf("w%d", w), 1)
				if err != nil {
					errs <- err
					return
				}
				if len(items) == 0 {
					idle++
					mu.Lock()
					total := 0
					for _, o := range order {
						total += len(o)
					}
					mu.Unlock()
					if total == users*per {
						return
					}
					time.Sleep(2 * time.Millisecond)
					continue
				}
				it := items[0]
				mu.Lock()
				if busy[it.UserKey] {
					mu.Unlock()
					errs <- fmt.Errorf("user %d has two updates in flight", it.UserKey)
					return
				}
				busy[it.UserKey] = true
				order[it.UserKey] = append(order[it.UserKey], it.UpdateID)
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				busy[it.UserKey] = false
				mu.Unlock()
				if err := q.Complete(ctx, it); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for u := int64(1); u <= users; u++ {
		o := order[u]
		if len(o) != per {
			t.Fatalf("user %d: handled %d updates, want %d", u, len(o), per)
		}
		for i := 1; i < len(o); i++ {
			if o[i] <= o[i-1] {
				t.Fatalf("user %d handled out of order: %v", u, o)
			}
		}
	}
}

func testRetryThenDead(t *testing.T, q Queue) {
	ctx := context.Background()
	mustEnqueue(t, q, 301, 7)
	mustEnqueue(t, q, 302, 7) // must wait behind 301 until it is dead
	boom := errors.New("boom")
	for attempt := 1; attempt <= 3; attempt++ {
		items := claimAll(t, q, "w1")
		if len(items) != 1 || items[0].UpdateID != 301 || items[0].Attempts != attempt {
			t.Fatalf("attempt %d: claimed %+v", attempt, items)
		}
		dead, err := q.Fail(ctx, items[0], boom, 0, 3)
		if err != nil {
			t.Fatal(err)
		}
		if dead != (attempt == 3) {
			t.Fatalf("attempt %d: dead=%v", attempt, dead)
		}
	}
	st, err := q.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Dead != 1 {
		t.Fatalf("stats %+v, want 1 dead", st)
	}
	items := claimAll(t, q, "w1")
	if len(items) != 1 || items[0].UpdateID != 302 {
		t.Fatalf("after dead letter claimed %+v, want 302", items)
	}
	_ = q.Complete(ctx, items[0])

	// Backoff: a failed update is not claimable before retryIn.
	mustEnqueue(t, q, 303, 8)
	items = claimAll(t, q, "w1")
	if _, err := q.Fail(ctx, items[0], boom, time.Hour, 3); err != nil {
		t.Fatal(err)
	}
	if got := claimAll(t, q, "w1"); len(got) != 0 {
		t.Fatalf("update claimed during its backoff: %+v", got)
	}
}

func testRelease(t *testing.T, q Queue) {
	ctx := context.Background()
	mustEnqueue(t, q, 401, 9)
	items := claimAll(t, q, "w1")
	if err := q.Release(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	items = claimAll(t, q, "w2")
	if len(items) != 1 || items[0].Attempts != 1 {
		t.Fatalf("after release claimed %+v, want attempt 1 again", items)
	}
	_ = q.Complete(ctx, items[0])
}

func testBackpressure(t *testing.T, q Queue) {
	ctx := context.Background()
	for i := int64(0); i < 3; i++ {
		mustEnqueue(t, q, 501+i, 10+i)
	}
	if _, err := q.Enqueue(ctx, 504, 20, payload(504)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("4th enqueue: err=%v, want ErrQueueFull", err)
	}
	// A re-delivery of an already queued update is a duplicate, not «full».
	if ins, err := q.Enqueue(ctx, 501, 10, payload(501)); err != nil || ins {
		t.Fatalf("duplicate while full: inserted=%v err=%v", ins, err)
	}
}

func testFencing(t *testing.T, q Queue) {
	ctx := context.Background()
	mustEnqueue(t, q, 601, 11)
	first := claimAll(t, q, "w1")[0]
	// The lease expires (simulated: release by the reaper path) and another
	// worker claims the update again.
	if err := q.Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := claimAll(t, q, "w2")[0]
	// The stale first claim must not complete or fail the new claim.
	_ = q.Complete(ctx, first)
	if _, err := q.Fail(ctx, first, errors.New("stale"), 0, 1); err != nil {
		t.Fatal(err)
	}
	st, _ := q.Stats(ctx)
	if st.Processing != 1 || st.Dead != 0 {
		t.Fatalf("stale claim changed the update: %+v", st)
	}
	if err := q.Complete(ctx, second); err != nil {
		t.Fatal(err)
	}
	st, _ = q.Stats(ctx)
	if st.Processing != 0 || st.Pending != 0 {
		t.Fatalf("after complete: %+v", st)
	}
}

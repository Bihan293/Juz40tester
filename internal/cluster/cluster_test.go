package cluster

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func testRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// realRedis returns a client of REDIS_TEST_URL (CI) or nil.
func realRedis(t *testing.T) *redis.Client {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		return nil
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// TestRedisTGLimiterGCRA: with a fixed clock the shared bucket hands out
// `burst` immediate slots, then one slot per interval — for ALL limiter
// instances together (two «processes» share one bucket).
func TestRedisTGLimiterGCRA(t *testing.T) {
	_, rdb := testRedis(t)
	now := time.Unix(1_800_000_000, 0)
	a := NewRedisTGLimiter(rdb, "t", 10, 2) // 100 ms interval, burst 2
	b := NewRedisTGLimiter(rdb, "t", 10, 2)
	a.now = func() time.Time { return now }
	b.now = a.now
	ctx := context.Background()
	want := []time.Duration{0, 0, 100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}
	lims := []*RedisTGLimiter{a, b, a, b, a}
	for i, l := range lims {
		d, err := l.reserve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d != want[i] {
			t.Fatalf("call %d: wait %s, want %s", i, d, want[i])
		}
	}
	// A 429 pauses every instance.
	b.Pause(5 * time.Second)
	d, err := a.reserve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d < 5*time.Second {
		t.Fatalf("after Pause the wait is %s, want ≥ 5s", d)
	}
}

// TestRedisTGLimiterRealTime: 30 calls at 50 rps (burst 5) from 3
// goroutines take ≈ (30-5)/50 = 0.5 s.
func TestRedisTGLimiterRealTime(t *testing.T) {
	clients := []*redis.Client{}
	_, mini := testRedis(t)
	clients = append(clients, mini)
	if r := realRedis(t); r != nil {
		_ = r.Del(context.Background(), key("rt", "tg:tat"), key("rt", "tg:pause")).Err()
		clients = append(clients, r)
	}
	for _, rdb := range clients {
		l := NewRedisTGLimiter(rdb, "rt", 50, 5)
		start := time.Now()
		var wg sync.WaitGroup
		for g := 0; g < 3; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					if err := l.Wait(context.Background()); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()
		el := time.Since(start)
		if el < 400*time.Millisecond || el > 3*time.Second {
			t.Fatalf("30 calls took %s, want ≈ 0.5 s", el)
		}
	}
}

// TestRedisTGLimiterUnavailable: Wait returns an error (the bot client then
// falls back to its local limiter) instead of blocking.
func TestRedisTGLimiterUnavailable(t *testing.T) {
	mr, rdb := testRedis(t)
	l := NewRedisTGLimiter(rdb, "t", 10, 1)
	mr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("expected an error with Redis down")
	}
}

func TestRedisActionLimiter(t *testing.T) {
	mr, rdb := testRedis(t)
	a := NewRedisActionLimiter(rdb, "t", time.Second)
	b := NewRedisActionLimiter(rdb, "t", time.Second) // another instance
	if !a.Allow(1) {
		t.Fatal("first action refused")
	}
	if b.Allow(1) {
		t.Fatal("second action within the interval allowed on another instance")
	}
	if !b.Allow(2) {
		t.Fatal("another user throttled")
	}
	mr.FastForward(1100 * time.Millisecond)
	if !a.Allow(1) {
		t.Fatal("action after the interval refused")
	}
	if !NewRedisActionLimiter(rdb, "t", 0).Allow(1) {
		t.Fatal("interval 0 must disable the throttle")
	}
	mr.Close()
	if !a.Allow(3) {
		t.Fatal("throttle must fail open when Redis is down")
	}
}

func testCache(t *testing.T, c Cache, expire func(time.Duration)) {
	ctx := context.Background()
	if _, ok, err := c.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("miss: ok=%v err=%v", ok, err)
	}
	if err := c.Set(ctx, "k", []byte("v1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := c.Get(ctx, "k"); !ok || string(v) != "v1" {
		t.Fatalf("hit: %q %v", v, ok)
	}
	expire(2 * time.Minute)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("entry did not expire")
	}
	_ = c.Set(ctx, "d", []byte("x"), time.Minute)
	_ = c.Delete(ctx, "d")
	if _, ok, _ := c.Get(ctx, "d"); ok {
		t.Fatal("deleted entry still there")
	}
}

func TestMemoryCache(t *testing.T) {
	c := NewMemoryCache(10)
	now := time.Now()
	c.now = func() time.Time { return now }
	testCache(t, c, func(d time.Duration) { now = now.Add(d) })
	// bounded
	for i := 0; i < 50; i++ {
		_ = c.Set(context.Background(), string(rune('a'+i)), []byte("x"), time.Hour)
	}
	if len(c.m) > 10 {
		t.Fatalf("cache grew to %d keys", len(c.m))
	}
}

func TestRedisCache(t *testing.T) {
	mr, rdb := testRedis(t)
	testCache(t, NewRedisCache(rdb, "t"), mr.FastForward)
}

func TestRedisBusAndEvents(t *testing.T) {
	_, rdb := testRedis(t)
	busA, busB := NewRedisBus(rdb, "t"), NewRedisBus(rdb, "t")
	a, b := NewEvents(busA, "inst-a"), NewEvents(busB, "inst-b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotA := make(chan Event, 4)
	gotB := make(chan Event, 4)
	go a.Run(ctx, func(e Event) { gotA <- e })
	go b.Run(ctx, func(e Event) { gotB <- e })
	time.Sleep(200 * time.Millisecond) // subscriptions active
	a.Publish(Event{Type: EventGenFinished, JobID: 7, Kind: "chain", SubjectID: 2, TestNumber: 3})
	select {
	case e := <-gotB:
		if e.JobID != 7 || e.TestNumber != 3 || e.Origin != "inst-a" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered to the other instance")
	}
	select {
	case e := <-gotA:
		t.Fatalf("own event delivered back: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
	var n atomic.Int32
	b.Deliver([]byte(`{"o":"inst-a","t":"tr","test":5,"ready":true}`), func(e Event) {
		if e.TestID == 5 && e.Ready {
			n.Add(1)
		}
	})
	b.Deliver([]byte(`{"o":"inst-b","t":"tr","test":5}`), func(Event) { n.Add(10) })
	b.Deliver([]byte(`garbage`), func(Event) { n.Add(100) })
	if n.Load() != 1 {
		t.Fatalf("Deliver dispatched %d", n.Load())
	}
}

func TestShareRPS(t *testing.T) {
	cases := [][3]int{{25, 1, 25}, {25, 2, 12}, {25, 3, 8}, {25, 40, 1}, {25, 0, 25}}
	for _, c := range cases {
		if got := ShareRPS(c[0], c[1]); got != c[2] {
			t.Errorf("ShareRPS(%d,%d) = %d, want %d", c[0], c[1], got, c[2])
		}
	}
}

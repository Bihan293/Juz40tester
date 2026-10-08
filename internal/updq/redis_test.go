package updq

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// redisClients returns the Redis servers to test against: always an
// in-process miniredis, plus a real server when REDIS_TEST_URL is set (CI).
func redisClients(t *testing.T) map[string]func(t *testing.T) redis.UniversalClient {
	out := map[string]func(t *testing.T) redis.UniversalClient{
		"miniredis": func(t *testing.T) redis.UniversalClient {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })
			return rdb
		},
	}
	if url := os.Getenv("REDIS_TEST_URL"); url != "" {
		out["real"] = func(t *testing.T) redis.UniversalClient {
			opt, err := redis.ParseURL(url)
			if err != nil {
				t.Fatal(err)
			}
			rdb := redis.NewClient(opt)
			if err := rdb.FlushDB(context.Background()).Err(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rdb.Close() })
			return rdb
		}
	}
	return out
}

func TestRedisQueueConformance(t *testing.T) {
	for name, mk := range redisClients(t) {
		t.Run(name, func(t *testing.T) {
			runConformance(t, func(t *testing.T, max int) Queue {
				return NewRedis(mk(t), RedisOptions{Prefix: "t", MaxBacklog: max})
			})
		})
	}
}

// TestRedisReaper: an expired lease is returned to the queue, the last
// attempt becomes a dead letter (with its payload).
func TestRedisReaper(t *testing.T) {
	for name, mk := range redisClients(t) {
		t.Run(name, func(t *testing.T) {
			q := NewRedis(mk(t), RedisOptions{Prefix: "t", Lease: 30 * time.Second})
			ctx := context.Background()
			mustEnqueue(t, q, 1, 1)
			mustEnqueue(t, q, 2, 2)
			items := claimAll(t, q, "w1")
			if len(items) != 2 {
				t.Fatalf("claimed %+v", items)
			}
			base := time.Now()
			// 20 s later: update 1 heart-beats, update 2 does not.
			q.now = func() time.Time { return base.Add(20 * time.Second) }
			if err := q.Heartbeat(ctx, "w1", items[:1]); err != nil {
				t.Fatal(err)
			}
			// 40 s after the claim: only update 2's lease has expired.
			q.now = func() time.Time { return base.Add(40 * time.Second) }
			req, dead, err := q.Reap(ctx, 0, 3)
			if err != nil || req != 1 || dead != 0 {
				t.Fatalf("reap: %d %d %v", req, dead, err)
			}
			again := claimAll(t, q, "w2")
			if len(again) != 1 || again[0].UpdateID != 2 || again[0].Attempts != 2 {
				t.Fatalf("re-claimed %+v", again)
			}
			q.now = func() time.Time { return base.Add(2 * time.Minute) }
			req, dead, err = q.Reap(ctx, 0, 2)
			if err != nil || dead < 1 {
				t.Fatalf("reap 2: %d %d %v", req, dead, err)
			}
			dl, err := q.DeadLetters(ctx, 10)
			if err != nil || len(dl) == 0 || dl[0].Payload == "" {
				t.Fatalf("dead letters %+v %v", dl, err)
			}
		})
	}
}

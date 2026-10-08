// Package cluster holds the pieces that let several bot processes share
// the work (ROLE=web|worker, docs/CLUSTER.md):
//
//   - a cluster-wide Telegram rate limiter (Redis token bucket, or an even
//     split of TG_MAX_RPS between the live instances registered in
//     PostgreSQL),
//   - a shared per-user action throttle (Redis),
//   - a shared cache (memory / Redis),
//   - an event bus (PostgreSQL NOTIFY / Redis pub/sub) that tells every
//     instance when a generation or translation finished,
//   - the registry of live instances (PostgreSQL).
//
// Redis is optional: with REDIS_URL empty nothing here touches Redis.
package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisClient connects to REDIS_URL (redis:// or rediss://) and pings it.
func NewRedisClient(ctx context.Context, url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL: %w", err)
	}
	if opt.DialTimeout == 0 {
		opt.DialTimeout = 5 * time.Second
	}
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = 3 * time.Second
	}
	if opt.WriteTimeout == 0 {
		opt.WriteTimeout = 3 * time.Second
	}
	rdb := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return rdb, nil
}

// key builds a key under the hash tag {prefix}.
func key(prefix, name string) string { return "{" + prefix + "}:" + name }

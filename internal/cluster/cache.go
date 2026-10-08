package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache is a small shared key/value cache with TTL (CACHE_BACKEND).
// Values are opaque bytes (callers marshal). A miss returns ok=false.
type Cache interface {
	Get(ctx context.Context, key string) (val []byte, ok bool, err error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// MemoryCache is the in-process Cache (the default): bounded, expiring.
type MemoryCache struct {
	mu      sync.Mutex
	m       map[string]memEntry
	maxKeys int
	now     func() time.Time
}

type memEntry struct {
	v   []byte
	exp time.Time
}

// NewMemoryCache creates an in-memory cache holding at most maxKeys keys
// (expired keys are swept when the limit is reached; then the insert of a
// new key evicts an arbitrary one).
func NewMemoryCache(maxKeys int) *MemoryCache {
	if maxKeys <= 0 {
		maxKeys = 10000
	}
	return &MemoryCache{m: map[string]memEntry{}, maxKeys: maxKeys, now: time.Now}
}

// Get implements Cache.
func (c *MemoryCache) Get(_ context.Context, k string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return nil, false, nil
	}
	if !e.exp.IsZero() && !c.now().Before(e.exp) {
		delete(c.m, k)
		return nil, false, nil
	}
	return append([]byte(nil), e.v...), true, nil
}

// Set implements Cache (ttl <= 0 = no expiry).
func (c *MemoryCache) Set(_ context.Context, k string, v []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.m[k]; !exists && len(c.m) >= c.maxKeys {
		now := c.now()
		for kk, e := range c.m {
			if !e.exp.IsZero() && !now.Before(e.exp) {
				delete(c.m, kk)
			}
		}
		if len(c.m) >= c.maxKeys {
			for kk := range c.m {
				delete(c.m, kk)
				break
			}
		}
	}
	e := memEntry{v: append([]byte(nil), v...)}
	if ttl > 0 {
		e.exp = c.now().Add(ttl)
	}
	c.m[k] = e
	return nil
}

// Delete implements Cache.
func (c *MemoryCache) Delete(_ context.Context, k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
	return nil
}

// RedisCache is the shared Cache on Redis (CACHE_BACKEND=redis).
type RedisCache struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewRedisCache creates the Redis cache.
func NewRedisCache(rdb redis.UniversalClient, prefix string) *RedisCache {
	return &RedisCache{rdb: rdb, prefix: prefix}
}

// Get implements Cache.
func (c *RedisCache) Get(ctx context.Context, k string) ([]byte, bool, error) {
	v, err := c.rdb.Get(ctx, key(c.prefix, "cache:"+k)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// Set implements Cache.
func (c *RedisCache) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	if ttl < 0 {
		ttl = 0
	}
	return c.rdb.Set(ctx, key(c.prefix, "cache:"+k), v, ttl).Err()
}

// Delete implements Cache.
func (c *RedisCache) Delete(ctx context.Context, k string) error {
	return c.rdb.Del(ctx, key(c.prefix, "cache:"+k)).Err()
}

var (
	_ Cache = (*MemoryCache)(nil)
	_ Cache = (*RedisCache)(nil)
)

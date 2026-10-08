package cluster

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisTGLimiter is the cluster-wide token bucket (GCRA) of outgoing
// message-changing Telegram calls (RATELIMIT_BACKEND=redis): TG_MAX_RPS
// holds for ALL instances together. Each Wait is one Lua call that books
// the next slot atomically and returns how long to wait. A 429 pauses the
// whole cluster (Pause). Times are microseconds of the caller's clock
// (instances are NTP-synced; a skew of a few ms only shifts single slots).
//
// It implements bot.SharedLimiter: when Redis is unreachable Wait returns
// the error and the Telegram client falls back to its local limiter.
type RedisTGLimiter struct {
	rdb      redis.UniversalClient
	tatKey   string
	pauseKey string
	interval int64 // µs
	tau      int64 // µs
	now      func() time.Time
	script   *redis.Script
	pauseS   *redis.Script
}

// NewRedisTGLimiter creates the shared limiter: rps calls per second with
// the given burst.
func NewRedisTGLimiter(rdb redis.UniversalClient, prefix string, rps, burst int) *RedisTGLimiter {
	if rps <= 0 {
		rps = 25
	}
	if burst <= 0 {
		burst = 1
	}
	iv := int64(time.Second/time.Microsecond) / int64(rps)
	return &RedisTGLimiter{
		rdb: rdb, tatKey: key(prefix, "tg:tat"), pauseKey: key(prefix, "tg:pause"),
		interval: iv, tau: int64(burst-1) * iv, now: time.Now,
		script: redis.NewScript(luaGCRA), pauseS: redis.NewScript(luaPause),
	}
}

// luaGCRA books the next slot: KEYS tat, pause; ARGV now, interval, tau (µs).
// Returns the wait in µs.
const luaGCRA = `
local now = tonumber(ARGV[1])
local iv = tonumber(ARGV[2])
local tau = tonumber(ARGV[3])
local tat = tonumber(redis.call('GET', KEYS[1]) or '0')
local paused = tonumber(redis.call('GET', KEYS[2]) or '0')
local base = math.max(tat, now, paused)
local allow = math.max(base - tau, now, paused)
local nt = base + iv
local ttl = math.floor((nt - now) / 1000) + 1000
redis.call('SET', KEYS[1], string.format('%.0f', nt), 'PX', ttl)
return allow - now
`

// luaPause extends the cluster-wide pause: KEYS pause; ARGV until, ttlMs.
const luaPause = `
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
if tonumber(ARGV[1]) > cur then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
end
return 1
`

// reserve books a slot and returns the wait.
func (l *RedisTGLimiter) reserve(ctx context.Context) (time.Duration, error) {
	now := l.now().UnixMicro()
	us, err := l.script.Run(ctx, l.rdb, []string{l.tatKey, l.pauseKey}, now, l.interval, l.tau).Int64()
	if err != nil {
		return 0, err
	}
	return time.Duration(us) * time.Microsecond, nil
}

// Wait blocks until the caller may send (bot.SharedLimiter).
func (l *RedisTGLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	d, err := l.reserve(rctx)
	cancel()
	if err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Pause stops the whole cluster for d (Telegram 429 retry_after).
func (l *RedisTGLimiter) Pause(d time.Duration) {
	if d <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	until := l.now().Add(d).UnixMicro()
	_ = l.pauseS.Run(ctx, l.rdb, []string{l.pauseKey}, strconv.FormatInt(until, 10), (d + time.Second).Milliseconds()).Err()
}

// RedisActionLimiter is the cluster-wide per-user action throttle
// (RATELIMIT_BACKEND=redis): at most one action per key per interval, via
// SET NX PX. Fails OPEN — a Redis outage never blocks users.
type RedisActionLimiter struct {
	rdb      redis.UniversalClient
	prefix   string
	interval time.Duration
}

// NewRedisActionLimiter creates the throttle; interval <= 0 disables it.
func NewRedisActionLimiter(rdb redis.UniversalClient, prefix string, interval time.Duration) *RedisActionLimiter {
	return &RedisActionLimiter{rdb: rdb, prefix: prefix, interval: interval}
}

// Allow reports whether key may act now and, if so, records the action.
func (l *RedisActionLimiter) Allow(k int64) bool {
	if l == nil || l.interval <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	ok, err := l.rdb.SetNX(ctx, key(l.prefix, "act:"+strconv.FormatInt(k, 10)), "1", l.interval).Result()
	if err != nil {
		return true
	}
	return ok
}

package updq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the Redis implementation of Queue (QUEUE_BACKEND=redis).
//
// Layout (every key shares the hash tag {prefix}, so the Lua scripts also
// run on Redis Cluster):
//
//	{p}:upd:seen:<update_id>  idempotency marker, TTL = doneTTL
//	{p}:upd:q:<user_key>      LIST of the user's update ids (FIFO)
//	{p}:upd:data              HASH update_id → payload
//	{p}:upd:att               HASH update_id → attempts
//	{p}:upd:ready             ZSET user_key → time (ms) the user's head may run
//	{p}:upd:busy              ZSET user_key → lease deadline (ms)
//	{p}:upd:owner             HASH user_key → claim token (fencing)
//	{p}:upd:depth             counter of queued updates (backpressure)
//	{p}:upd:dead              LIST of dead letters (JSON), capped
//
// A user is in exactly one of ready/busy while their list is non-empty, so
// only the HEAD of a user's list is ever handled — per-user order and «one
// update per user at a time» hold for any number of workers. Every state
// change is one atomic Lua script. New updates are announced on the pub/sub
// channel {p}:upd:wake.
type Redis struct {
	rdb       redis.UniversalClient
	p         string
	max       int64
	seenTTL   time.Duration
	lease     time.Duration
	deadMax   int64
	now       func() time.Time
	enqueue   *redis.Script
	claim     *redis.Script
	complete  *redis.Script
	fail      *redis.Script
	release   *redis.Script
	heartbeat *redis.Script
	reap      *redis.Script
}

// RedisOptions configures the Redis queue.
type RedisOptions struct {
	Prefix     string        // default "juz40"
	MaxBacklog int           // 0 = no backpressure
	SeenTTL    time.Duration // idempotency window, default 48 h
	Lease      time.Duration // claim lease, default 45 s
	DeadMax    int           // dead letters kept, default 10000
}

// NewRedis creates the Redis queue.
func NewRedis(rdb redis.UniversalClient, o RedisOptions) *Redis {
	if o.Prefix == "" {
		o.Prefix = "juz40"
	}
	if o.SeenTTL <= 0 {
		o.SeenTTL = 48 * time.Hour
	}
	if o.Lease <= 0 {
		o.Lease = 45 * time.Second
	}
	if o.DeadMax <= 0 {
		o.DeadMax = 10000
	}
	return &Redis{
		rdb: rdb, p: "{" + o.Prefix + "}:upd:", max: int64(o.MaxBacklog),
		seenTTL: o.SeenTTL, lease: o.Lease, deadMax: int64(o.DeadMax), now: time.Now,
		enqueue: redis.NewScript(luaEnqueue), claim: redis.NewScript(luaClaim),
		complete: redis.NewScript(luaComplete), fail: redis.NewScript(luaFail),
		release: redis.NewScript(luaRelease), heartbeat: redis.NewScript(luaHeartbeat),
		reap: redis.NewScript(luaReap),
	}
}

func (q *Redis) k(name string) string { return q.p + name }

// WakeChannel is the pub/sub channel announcing new updates.
func (q *Redis) WakeChannel() string { return q.k("wake") }

func (q *Redis) nowMs() int64 { return q.now().UnixMilli() }

const luaEnqueue = `
if redis.call('EXISTS', KEYS[1]) == 1 then return 0 end
local max = tonumber(ARGV[5])
if max > 0 and tonumber(redis.call('GET', KEYS[6]) or '0') >= max then return -1 end
redis.call('SET', KEYS[1], '1', 'EX', ARGV[4])
redis.call('HSET', KEYS[5], ARGV[1], ARGV[2])
local n = redis.call('RPUSH', KEYS[2], ARGV[1])
redis.call('INCR', KEYS[6])
if n == 1 and not redis.call('ZSCORE', KEYS[4], ARGV[6]) then
  redis.call('ZADD', KEYS[3], ARGV[3], ARGV[6])
end
redis.call('PUBLISH', KEYS[7], '')
return 1
`

// Enqueue implements Queue.
func (q *Redis) Enqueue(ctx context.Context, updateID, userKey int64, payload []byte) (bool, error) {
	id, uk := strconv.FormatInt(updateID, 10), strconv.FormatInt(userKey, 10)
	res, err := q.enqueue.Run(ctx, q.rdb,
		[]string{q.k("seen:" + id), q.k("q:" + uk), q.k("ready"), q.k("busy"), q.k("data"), q.k("depth"), q.WakeChannel()},
		id, string(payload), q.nowMs(), int64(q.seenTTL/time.Second), q.max, uk).Int64()
	if err != nil {
		return false, err
	}
	switch res {
	case -1:
		return false, ErrQueueFull
	case 0:
		return false, nil
	}
	return true, nil
}

const luaClaim = `
local uks = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
local out = {}
local deadline = string.format('%.0f', tonumber(ARGV[1]) + tonumber(ARGV[3]))
for _, uk in ipairs(uks) do
  redis.call('ZREM', KEYS[1], uk)
  local id = redis.call('LINDEX', ARGV[4] .. uk, 0)
  if id then
    local a = redis.call('HINCRBY', KEYS[3], id, 1)
    redis.call('ZADD', KEYS[2], deadline, uk)
    redis.call('HSET', KEYS[5], uk, ARGV[5])
    local p = redis.call('HGET', KEYS[4], id)
    table.insert(out, uk)
    table.insert(out, id)
    table.insert(out, tostring(a))
    table.insert(out, p or '')
  end
end
return out
`

// Claim implements Queue.
func (q *Redis) Claim(ctx context.Context, worker string, n int) ([]Item, error) {
	if n <= 0 {
		return nil, nil
	}
	token := worker + ":" + randToken()
	raw, err := q.claim.Run(ctx, q.rdb,
		[]string{q.k("ready"), q.k("busy"), q.k("att"), q.k("data"), q.k("owner")},
		q.nowMs(), n, q.lease.Milliseconds(), q.k("q:"), token).StringSlice()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	var out []Item
	for i := 0; i+3 < len(raw); i += 4 {
		uk, _ := strconv.ParseInt(raw[i], 10, 64)
		id, _ := strconv.ParseInt(raw[i+1], 10, 64)
		a, _ := strconv.Atoi(raw[i+2])
		out = append(out, Item{UpdateID: id, UserKey: uk, Attempts: a, Payload: []byte(raw[i+3]), Token: token})
	}
	return out, nil
}

const luaComplete = `
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[3] then return 0 end
local q = ARGV[5] .. ARGV[1]
if redis.call('LINDEX', q, 0) == ARGV[2] then
  redis.call('LPOP', q)
  redis.call('HDEL', KEYS[4], ARGV[2])
  redis.call('HDEL', KEYS[3], ARGV[2])
  redis.call('DECR', KEYS[6])
end
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[5], ARGV[1])
if redis.call('LLEN', q) > 0 then redis.call('ZADD', KEYS[1], ARGV[4], ARGV[1]) end
return 1
`

func (q *Redis) stateKeys() []string {
	return []string{q.k("ready"), q.k("busy"), q.k("att"), q.k("data"), q.k("owner"), q.k("depth"), q.k("dead")}
}

// Complete implements Queue.
func (q *Redis) Complete(ctx context.Context, it Item) error {
	return q.complete.Run(ctx, q.rdb, q.stateKeys(),
		strconv.FormatInt(it.UserKey, 10), strconv.FormatInt(it.UpdateID, 10), it.Token, q.nowMs(), q.k("q:")).Err()
}

const luaFail = `
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[3] then return 0 end
local q = ARGV[5] .. ARGV[1]
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[5], ARGV[1])
local a = tonumber(redis.call('HGET', KEYS[3], ARGV[2]) or '0')
if a >= tonumber(ARGV[7]) then
  if redis.call('LINDEX', q, 0) == ARGV[2] then
    redis.call('LPOP', q)
    redis.call('HDEL', KEYS[4], ARGV[2])
    redis.call('HDEL', KEYS[3], ARGV[2])
    redis.call('DECR', KEYS[6])
  end
  redis.call('RPUSH', KEYS[7], ARGV[8])
  redis.call('LTRIM', KEYS[7], -tonumber(ARGV[9]), -1)
  if redis.call('LLEN', q) > 0 then redis.call('ZADD', KEYS[1], ARGV[4], ARGV[1]) end
  return 2
end
redis.call('ZADD', KEYS[1], ARGV[6], ARGV[1])
return 1
`

// RedisDeadLetter is one entry of the Redis dead-letter list.
type RedisDeadLetter struct {
	UpdateID int64  `json:"update_id"`
	UserKey  int64  `json:"user_key"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	Payload  string `json:"payload"`
	At       string `json:"at"`
}

// Fail implements Queue.
func (q *Redis) Fail(ctx context.Context, it Item, cause error, retryIn time.Duration, maxAttempts int) (bool, error) {
	msg := "failed"
	if cause != nil {
		msg = cause.Error()
	}
	entry, _ := json.Marshal(RedisDeadLetter{UpdateID: it.UpdateID, UserKey: it.UserKey, Attempts: it.Attempts,
		Error: msg, Payload: string(it.Payload), At: q.now().UTC().Format(time.RFC3339)})
	now := q.nowMs()
	res, err := q.fail.Run(ctx, q.rdb, q.stateKeys(),
		strconv.FormatInt(it.UserKey, 10), strconv.FormatInt(it.UpdateID, 10), it.Token, now, q.k("q:"),
		now+retryIn.Milliseconds(), maxAttempts, string(entry), q.deadMax).Int64()
	if err != nil {
		return false, err
	}
	return res == 2, nil
}

const luaRelease = `
if redis.call('HGET', KEYS[5], ARGV[1]) ~= ARGV[3] then return 0 end
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[5], ARGV[1])
if tonumber(redis.call('HGET', KEYS[3], ARGV[2]) or '0') > 0 then redis.call('HINCRBY', KEYS[3], ARGV[2], -1) end
redis.call('ZADD', KEYS[1], ARGV[4], ARGV[1])
redis.call('PUBLISH', ARGV[5], '')
return 1
`

// Release implements Queue.
func (q *Redis) Release(ctx context.Context, it Item) error {
	return q.release.Run(ctx, q.rdb, q.stateKeys(),
		strconv.FormatInt(it.UserKey, 10), strconv.FormatInt(it.UpdateID, 10), it.Token, q.nowMs(), q.WakeChannel()).Err()
}

const luaHeartbeat = `
local n = 0
for i = 2, #ARGV, 2 do
  if redis.call('HGET', KEYS[2], ARGV[i]) == ARGV[i+1] then
    redis.call('ZADD', KEYS[1], 'XX', ARGV[1], ARGV[i])
    n = n + 1
  end
end
return n
`

// Heartbeat implements Queue.
func (q *Redis) Heartbeat(ctx context.Context, _ string, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	args := []any{q.nowMs() + q.lease.Milliseconds()}
	for _, it := range items {
		args = append(args, strconv.FormatInt(it.UserKey, 10), it.Token)
	}
	return q.heartbeat.Run(ctx, q.rdb, []string{q.k("busy"), q.k("owner")}, args...).Err()
}

const luaReap = `
local uks = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[5]))
local req, dead = 0, 0
for _, uk in ipairs(uks) do
  redis.call('ZREM', KEYS[2], uk)
  redis.call('HDEL', KEYS[5], uk)
  local q = ARGV[3] .. uk
  local id = redis.call('LINDEX', q, 0)
  if id then
    local a = tonumber(redis.call('HGET', KEYS[3], id) or '0')
    if a >= tonumber(ARGV[2]) then
      local p = redis.call('HGET', KEYS[4], id) or ''
      redis.call('LPOP', q)
      redis.call('HDEL', KEYS[4], id)
      redis.call('HDEL', KEYS[3], id)
      redis.call('DECR', KEYS[6])
      redis.call('RPUSH', KEYS[7], cjson.encode({update_id = tonumber(id), user_key = tonumber(uk), attempts = a,
        error = 'lease expired: the worker stopped heart-beating', payload = p, at = ARGV[6]}))
      redis.call('LTRIM', KEYS[7], -tonumber(ARGV[4]), -1)
      dead = dead + 1
      if redis.call('LLEN', q) > 0 then redis.call('ZADD', KEYS[1], ARGV[1], uk) end
    else
      redis.call('ZADD', KEYS[1], ARGV[1], uk)
      req = req + 1
    end
  end
end
if req + dead > 0 then redis.call('PUBLISH', ARGV[7], '') end
return {req, dead}
`

// Reap implements Queue. staleAfter is implied by the lease deadline that
// Claim/Heartbeat store; the argument is accepted for interface parity.
func (q *Redis) Reap(ctx context.Context, _ time.Duration, maxAttempts int) (int, int, error) {
	res, err := q.reap.Run(ctx, q.rdb, q.stateKeys(),
		q.nowMs(), maxAttempts, q.k("q:"), q.deadMax, reapBatch, q.now().UTC().Format(time.RFC3339), q.WakeChannel()).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	if len(res) != 2 {
		return 0, 0, fmt.Errorf("reap: unexpected reply %v", res)
	}
	return int(res[0]), int(res[1]), nil
}

// Purge implements Queue: a no-op — handled updates leave Redis at once,
// idempotency markers expire by TTL and the dead letters are capped.
func (q *Redis) Purge(context.Context, time.Duration, time.Duration) (int64, error) { return 0, nil }

// Stats implements Queue.
func (q *Redis) Stats(ctx context.Context) (Stats, error) {
	pipe := q.rdb.Pipeline()
	depth := pipe.Get(ctx, q.k("depth"))
	busy := pipe.ZCard(ctx, q.k("busy"))
	dead := pipe.LLen(ctx, q.k("dead"))
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return Stats{}, err
	}
	d, _ := depth.Int64()
	s := Stats{Processing: busy.Val(), Dead: dead.Val()}
	s.Pending = max(d-s.Processing, 0)
	return s, nil
}

// DeadLetters returns the newest dead letters.
func (q *Redis) DeadLetters(ctx context.Context, limit int) ([]RedisDeadLetter, error) {
	raw, err := q.rdb.LRange(ctx, q.k("dead"), int64(-limit), -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]RedisDeadLetter, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- {
		var d RedisDeadLetter
		if json.Unmarshal([]byte(raw[i]), &d) == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// Subscribe calls onWake for every wake-up published on the queue channel
// until ctx ends (and once after every (re)subscription — a message may
// have been missed while disconnected).
func (q *Redis) Subscribe(ctx context.Context, onWake func()) {
	for ctx.Err() == nil {
		sub := q.rdb.Subscribe(ctx, q.WakeChannel())
		if _, err := sub.Receive(ctx); err == nil {
			onWake()
			ch := sub.Channel()
		loop:
			for {
				select {
				case <-ctx.Done():
					break loop
				case _, ok := <-ch:
					if !ok {
						break loop
					}
					onWake()
				}
			}
		}
		_ = sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func randToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var _ Queue = (*Redis)(nil)

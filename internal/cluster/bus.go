package cluster

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Bihan293/Juz40tester/internal/database"
)

// ChannelEvents is the PostgreSQL NOTIFY channel of the event bus.
const ChannelEvents = "juz_events"

// Bus is a fire-and-forget broadcast to every instance (pub/sub). Delivery
// is best effort: every consumer of an event also has a polling fallback.
type Bus interface {
	Publish(ctx context.Context, payload []byte) error
	// Subscribe calls fn for every message until ctx ends (blocking).
	Subscribe(ctx context.Context, fn func(payload []byte))
}

// PostgresBus broadcasts with pg_notify (payload ≤ 8000 bytes) and listens
// on a dedicated DIRECT connection (LISTEN does not work behind a
// transaction-mode pooler).
type PostgresBus struct {
	pool      *pgxpool.Pool
	listenURL string
}

// NewPostgresBus creates the PostgreSQL bus.
func NewPostgresBus(pool *pgxpool.Pool, listenURL string) *PostgresBus {
	return &PostgresBus{pool: pool, listenURL: listenURL}
}

// Publish implements Bus.
func (b *PostgresBus) Publish(ctx context.Context, payload []byte) error {
	_, err := b.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, ChannelEvents, string(payload))
	return err
}

// Subscribe implements Bus.
func (b *PostgresBus) Subscribe(ctx context.Context, fn func([]byte)) {
	database.ListenPayload(ctx, b.listenURL, []string{ChannelEvents}, func(_, payload string) {
		if payload != "" {
			fn([]byte(payload))
		}
	})
}

// RedisBus broadcasts with Redis PUBLISH / SUBSCRIBE.
type RedisBus struct {
	rdb     redis.UniversalClient
	channel string
}

// NewRedisBus creates the Redis bus.
func NewRedisBus(rdb redis.UniversalClient, prefix string) *RedisBus {
	return &RedisBus{rdb: rdb, channel: key(prefix, "events")}
}

// Publish implements Bus.
func (b *RedisBus) Publish(ctx context.Context, payload []byte) error {
	return b.rdb.Publish(ctx, b.channel, payload).Err()
}

// Subscribe implements Bus (re-subscribes after a connection loss).
func (b *RedisBus) Subscribe(ctx context.Context, fn func([]byte)) {
	for ctx.Err() == nil {
		sub := b.rdb.Subscribe(ctx, b.channel)
		if _, err := sub.Receive(ctx); err == nil {
			ch := sub.Channel()
		loop:
			for {
				select {
				case <-ctx.Done():
					break loop
				case m, ok := <-ch:
					if !ok {
						break loop
					}
					fn([]byte(m.Payload))
				}
			}
		} else if ctx.Err() == nil {
			log.Printf("redis bus: subscribe: %v", err)
		}
		_ = sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

// Event is a cluster event: a generation job or a translation finished.
type Event struct {
	Origin string `json:"o"` // instance id of the sender (own events are skipped)
	Type   string `json:"t"` // "gen" | "tr"

	// gen
	JobID      int64  `json:"job,omitempty"`
	Kind       string `json:"kind,omitempty"`
	SubjectID  int64  `json:"subj,omitempty"`
	TestNumber int    `json:"num,omitempty"`

	// tr
	TestID   int64  `json:"test,omitempty"`
	Ready    bool   `json:"ready,omitempty"`
	TimedOut bool   `json:"to,omitempty"`
	Err      string `json:"err,omitempty"`
}

// Event types.
const (
	EventGenFinished = "gen"
	EventTrFinished  = "tr"
)

// Events publishes and dispatches cluster events over a Bus.
type Events struct {
	bus    Bus
	origin string
}

// NewEvents creates the event dispatcher of this instance.
func NewEvents(bus Bus, origin string) *Events { return &Events{bus: bus, origin: origin} }

// Publish broadcasts e (best effort, logged on failure).
func (e *Events) Publish(ev Event) {
	if e == nil || e.bus == nil {
		return
	}
	ev.Origin = e.origin
	if len(ev.Err) > 500 {
		ev.Err = ev.Err[:500]
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.bus.Publish(ctx, b); err != nil {
		log.Printf("cluster events: publish %s: %v", ev.Type, err)
	}
}

// Run delivers the events of OTHER instances to fn until ctx ends.
func (e *Events) Run(ctx context.Context, fn func(Event)) {
	if e == nil || e.bus == nil {
		return
	}
	e.bus.Subscribe(ctx, func(p []byte) {
		var ev Event
		if json.Unmarshal(p, &ev) != nil || ev.Origin == e.origin {
			return
		}
		fn(ev)
	})
}

// Deliver dispatches one raw event received outside Run (the PostgreSQL
// bus shares the LISTEN connection of the job queues); events of this
// instance are skipped.
func (e *Events) Deliver(p []byte, fn func(Event)) {
	if e == nil {
		return
	}
	var ev Event
	if json.Unmarshal(p, &ev) != nil || ev.Origin == e.origin {
		return
	}
	fn(ev)
}

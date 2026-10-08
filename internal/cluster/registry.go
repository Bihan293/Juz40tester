package cluster

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/metrics"
)

// Registry keeps this instance's row in cluster_instances alive and reports
// how many instances that send Telegram messages are alive — with
// RATELIMIT_BACKEND=postgres every sender uses TG_MAX_RPS / N, so the bot
// as a whole stays under Telegram's global limit without Redis. (The split
// is even, not adaptive: an idle instance keeps its share unused. Use
// RATELIMIT_BACKEND=redis for one exact shared bucket.)
type Registry struct {
	pool  *pgxpool.Pool
	id    string
	role  string
	sends bool
	every time.Duration
	ttl   time.Duration
}

// RegistryHeartbeat / RegistryTTL: heartbeat period and liveness window.
const (
	RegistryHeartbeat = 15 * time.Second
	RegistryTTL       = 45 * time.Second
)

// NewRegistry creates the registry of this instance. sends = the instance
// sends Telegram messages (worker / all; a pure web instance does not).
func NewRegistry(pool *pgxpool.Pool, id, role string, sends bool) *Registry {
	return &Registry{pool: pool, id: id, role: role, sends: sends, every: RegistryHeartbeat, ttl: RegistryTTL}
}

// Beat upserts this instance and returns the number of live senders
// (at least 1 when this instance sends).
func (r *Registry) Beat(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		WITH up AS (
			INSERT INTO cluster_instances (id, role, sends, heartbeat_at)
			VALUES ($1, $2, $3, now())
			ON CONFLICT (id) DO UPDATE SET role = EXCLUDED.role, sends = EXCLUDED.sends, heartbeat_at = now()
			RETURNING id
		)
		SELECT count(*) FROM cluster_instances
		WHERE sends AND heartbeat_at > now() - make_interval(secs => $4) AND id <> (SELECT id FROM up)`,
		r.id, r.role, r.sends, r.ttl.Seconds()).Scan(&n)
	if err != nil {
		return 0, err
	}
	if r.sends {
		n++
	}
	return max(n, 1), nil
}

// Run beats every 15 s and calls onSenders with the number of live senders
// whenever it changes (and once at start); expired rows are deleted now and
// then. On return (ctx cancelled) the row of this instance is removed so
// the others take over its share at once.
func (r *Registry) Run(ctx context.Context, onSenders func(n int)) {
	last := 0
	beat := func() {
		bctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		n, err := r.Beat(bctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("cluster registry: %v", err)
			}
			return
		}
		metrics.SetGauge("cluster_live_senders", float64(n))
		if n != last {
			if last != 0 {
				log.Printf("cluster registry: %d instance(s) send Telegram messages now (was %d)", n, last)
			}
			last = n
			if onSenders != nil {
				onSenders(n)
			}
		}
	}
	beat()
	t := time.NewTicker(r.every)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-ctx.Done():
			dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = r.pool.Exec(dctx, `DELETE FROM cluster_instances WHERE id = $1`, r.id)
			cancel()
			return
		case <-t.C:
			beat()
			if i++; i%40 == 0 { // ~every 10 minutes
				dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, _ = r.pool.Exec(dctx, `DELETE FROM cluster_instances WHERE heartbeat_at < now() - interval '1 hour'`)
				cancel()
			}
		}
	}
}

// ShareRPS splits the cluster-wide rate between n senders (at least 1 rps).
func ShareRPS(total, n int) int {
	if n <= 1 {
		return max(total, 1)
	}
	return max(total/n, 1)
}

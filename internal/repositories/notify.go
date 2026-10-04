package repositories

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgconn"
)

// LISTEN/NOTIFY channels announcing new work in the job queues (R-5a).
const (
	ChannelGenJobs = "gen_jobs"
	ChannelTrJobs  = "tr_jobs"
)

// execer is the subset of pgxpool.Pool / pgx.Tx used by notifyQueue.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// notifyQueue sends pg_notify on channel right after a job was committed.
// Best effort: workers still poll on a timer, so a lost notification only
// delays the job, it never loses it — an error is logged, not returned.
func notifyQueue(ctx context.Context, db execer, channel string) {
	if _, err := db.Exec(ctx, `SELECT pg_notify($1, '')`, channel); err != nil {
		log.Printf("pg_notify %s: %v", channel, err)
	}
}

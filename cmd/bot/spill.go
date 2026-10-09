package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// Spilled updates (QUEUE_BACKEND=memory).
//
// The in-process dispatcher acknowledges an update with 200 BEFORE handling
// it. When the process is stopped (deploy, restart, Render spin-down) with
// a backlog that cannot be processed within the grace period, the rest is
// written to tg_update_queue (updateDispatcher.spill) instead of being
// dropped, and announced with NOTIFY tg_updates. Every memory-mode instance
// replays such rows: right after its start and whenever a NOTIFY arrives
// (on a zero-downtime deploy the new instance is already running while the
// old one spills). A row is claimed and deleted in ONE statement, so two
// instances never replay the same update.

// replayBatch bounds one claim.
const replayBatch = 200

// claimSpilled atomically takes (and deletes) up to n pending rows.
func claimSpilled(ctx context.Context, pool *pgxpool.Pool, n int) ([]*bot.Update, error) {
	rows, err := pool.Query(ctx, `
		DELETE FROM tg_update_queue WHERE update_id IN (
			SELECT update_id FROM tg_update_queue
			WHERE status = 'pending'
			ORDER BY update_id
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		RETURNING update_id, payload::text`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*bot.Update
	for rows.Next() {
		var id int64
		var payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return out, err
		}
		upd := &bot.Update{}
		if err := json.Unmarshal([]byte(payload), upd); err != nil {
			log.Printf("replay: spilled update %d is malformed, dropped: %v", id, err)
			continue
		}
		upd.UpdateID = id
		out = append(out, upd)
	}
	return out, rows.Err()
}

// replaySpilledOnce moves spilled updates into the dispatcher while it has
// room. Returns how many were replayed.
func replaySpilledOnce(ctx context.Context, pool *pgxpool.Pool, d *updateDispatcher) (int, error) {
	total := 0
	for ctx.Err() == nil {
		n := min(d.freeSlots(), replayBatch)
		if n <= 0 {
			return total, nil // busy: the rest waits for the next wake-up
		}
		batch, err := claimSpilled(ctx, pool, n)
		if err != nil {
			return total, err
		}
		for _, upd := range batch {
			if d.enqueueReplayed(upd) != enqueued {
				// Full or shutting down: put it back for later / another instance.
				if !d.spillOne(upd) {
					d.dropped.Add(1)
					log.Printf("replay: update %d could not be re-queued — dropped", upd.UpdateID)
				}
				continue
			}
			total++
		}
		if len(batch) < n {
			return total, nil
		}
	}
	return total, ctx.Err()
}

// replaySpilled runs replaySpilledOnce at start, on every wake-up (NOTIFY
// tg_updates) and every replayPoll as a fallback, until ctx ends.
func replaySpilled(ctx context.Context, pool *pgxpool.Pool, d *updateDispatcher, wake <-chan struct{}) {
	t := time.NewTicker(replayPoll)
	defer t.Stop()
	for {
		if n, err := replaySpilledOnce(ctx, pool, d); err != nil && ctx.Err() == nil {
			log.Printf("replay: spilled updates: %v", err)
		} else if n > 0 {
			log.Printf("replay: %d update(s) saved by a stopped instance are being processed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-t.C:
		}
	}
}

// replayPoll is the fallback period of replaySpilled (a missed NOTIFY, or
// rows left because the queue was full).
const replayPoll = 2 * time.Minute

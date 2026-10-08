package updq

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChannelUpdates is the LISTEN/NOTIFY channel announcing new updates.
const ChannelUpdates = "tg_updates"

// backlogCacheTTL: how long Enqueue reuses the last backlog count (one
// count query per second per web instance, not one per update).
const backlogCacheTTL = time.Second

// purgeBatch bounds one DELETE of the purge (short locks, small WAL bursts).
const purgeBatch = 5000

// reapBatch bounds one reaper pass.
const reapBatch = 500

// Postgres is the PostgreSQL implementation of Queue (table tg_update_queue,
// migration 000025). Claims use FOR UPDATE SKIP LOCKED, new updates are
// announced with NOTIFY tg_updates (workers LISTEN on a direct connection
// and fall back to a short poll).
type Postgres struct {
	pool *pgxpool.Pool
	max  int64 // backlog limit, 0 = none

	mu        sync.Mutex
	backlog   int64
	backlogAt time.Time
	now       func() time.Time
}

// NewPostgres creates the queue; maxBacklog <= 0 disables backpressure.
func NewPostgres(pool *pgxpool.Pool, maxBacklog int) *Postgres {
	return &Postgres{pool: pool, max: int64(maxBacklog), now: time.Now}
}

// full reports whether the backlog (pending + processing) reached the
// limit, using a count cached for backlogCacheTTL.
func (q *Postgres) full(ctx context.Context) (bool, error) {
	if q.max <= 0 {
		return false, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.backlogAt.IsZero() || q.now().Sub(q.backlogAt) >= backlogCacheTTL {
		var n int64
		if err := q.pool.QueryRow(ctx,
			`SELECT count(*) FROM tg_update_queue WHERE status IN ('pending','processing')`).Scan(&n); err != nil {
			return false, err
		}
		q.backlog, q.backlogAt = n, q.now()
	}
	if q.backlog >= q.max {
		return true, nil
	}
	q.backlog++ // account for this update until the next refresh
	return false, nil
}

// Enqueue implements Queue: one round trip — INSERT … ON CONFLICT DO
// NOTHING plus NOTIFY in the same statement.
func (q *Postgres) Enqueue(ctx context.Context, updateID, userKey int64, payload []byte) (bool, error) {
	isFull, err := q.full(ctx)
	if err != nil {
		return false, err
	}
	if isFull {
		// A re-delivery of an update we already hold is still a duplicate,
		// not «full»: Telegram must get its 200.
		var exists bool
		if err := q.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tg_update_queue WHERE update_id = $1)`, updateID).Scan(&exists); err == nil && exists {
			return false, nil
		}
		return false, ErrQueueFull
	}
	inserted, err := q.insert(ctx, updateID, userKey, payload)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P05" {
		// jsonb rejects the \u0000 escape (a NUL in a user's text): drop it
		// instead of answering 503 forever for an update that can never fit.
		inserted, err = q.insert(ctx, updateID, userKey, bytes.ReplaceAll(payload, []byte(`\u0000`), nil))
	}
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// insert stores one update and announces it (single round trip).
func (q *Postgres) insert(ctx context.Context, updateID, userKey int64, payload []byte) (bool, error) {
	var inserted int
	err := q.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO tg_update_queue (update_id, user_key, payload)
			VALUES ($1, $2, $3::jsonb)
			ON CONFLICT (update_id) DO NOTHING
			RETURNING 1
		), n AS (
			SELECT pg_notify($4, '') FROM ins
		)
		SELECT (SELECT count(*) FROM ins) + 0 * (SELECT count(*) FROM n)`,
		updateID, userKey, string(payload), ChannelUpdates).Scan(&inserted)
	if err != nil {
		return false, err
	}
	return inserted > 0, nil
}

// Claim implements Queue. A pending row is claimable when its backoff is
// over and NO earlier row of the same user is still pending or processing
// — that single anti-join gives both «one update per user at a time» and
// «in update_id order», across any number of workers.
func (q *Postgres) Claim(ctx context.Context, worker string, n int) ([]Item, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := q.pool.Query(ctx, `
		WITH cand AS (
			SELECT u.update_id
			FROM tg_update_queue u
			WHERE u.status = 'pending'
			  AND u.available_at <= now()
			  AND NOT EXISTS (
			      SELECT 1 FROM tg_update_queue p
			      WHERE p.user_key = u.user_key
			        AND p.status IN ('pending','processing')
			        AND p.update_id < u.update_id)
			ORDER BY u.update_id
			LIMIT $2
			FOR UPDATE OF u SKIP LOCKED
		)
		UPDATE tg_update_queue t
		SET status = 'processing', locked_by = $1, heartbeat_at = now(),
		    attempts = t.attempts + 1
		FROM cand
		WHERE t.update_id = cand.update_id
		RETURNING t.update_id, t.user_key, t.payload::text, t.attempts`, worker, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var payload string
		if err := rows.Scan(&it.UpdateID, &it.UserKey, &payload, &it.Attempts); err != nil {
			return nil, err
		}
		it.Payload = []byte(payload)
		it.Token = worker
		out = append(out, it)
	}
	return out, rows.Err()
}

// Complete implements Queue (fenced by worker + attempt number).
func (q *Postgres) Complete(ctx context.Context, it Item) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE tg_update_queue
		SET status = 'done', finished_at = now(), locked_by = NULL, heartbeat_at = NULL
		WHERE update_id = $1 AND status = 'processing' AND locked_by = $2 AND attempts = $3`,
		it.UpdateID, it.Token, it.Attempts)
	// No NOTIFY: the completing worker claims again right away and picks
	// up the next update of this user itself.
	return err
}

// Fail implements Queue.
func (q *Postgres) Fail(ctx context.Context, it Item, cause error, retryIn time.Duration, maxAttempts int) (bool, error) {
	msg := "failed"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	var status string
	err := q.pool.QueryRow(ctx, `
		UPDATE tg_update_queue
		SET status = CASE WHEN attempts >= $4 THEN 'dead' ELSE 'pending' END,
		    available_at = now() + make_interval(secs => $5),
		    locked_by = NULL, heartbeat_at = NULL, last_error = $6,
		    finished_at = CASE WHEN attempts >= $4 THEN now() END
		WHERE update_id = $1 AND status = 'processing' AND locked_by = $2 AND attempts = $3
		RETURNING status`,
		it.UpdateID, it.Token, it.Attempts, maxAttempts, retryIn.Seconds(), msg).Scan(&status)
	if err != nil {
		if isNoRows(err) {
			return false, nil // lease lost: someone else owns the row now
		}
		return false, err
	}
	return status == "dead", nil
}

// Release implements Queue: back to pending, the attempt is not counted.
func (q *Postgres) Release(ctx context.Context, it Item) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE tg_update_queue
		SET status = 'pending', attempts = GREATEST(attempts - 1, 0),
		    available_at = now(), locked_by = NULL, heartbeat_at = NULL
		WHERE update_id = $1 AND status = 'processing' AND locked_by = $2 AND attempts = $3`,
		it.UpdateID, it.Token, it.Attempts)
	if err == nil {
		q.notify(ctx)
	}
	return err
}

// Heartbeat implements Queue.
func (q *Postgres) Heartbeat(ctx context.Context, worker string, items []Item) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.UpdateID
	}
	_, err := q.pool.Exec(ctx, `
		UPDATE tg_update_queue SET heartbeat_at = now()
		WHERE status = 'processing' AND locked_by = $1 AND update_id = ANY($2)`, worker, ids)
	return err
}

// Reap implements Queue.
func (q *Postgres) Reap(ctx context.Context, staleAfter time.Duration, maxAttempts int) (int, int, error) {
	rows, err := q.pool.Query(ctx, `
		WITH stale AS (
			SELECT update_id FROM tg_update_queue
			WHERE status = 'processing' AND heartbeat_at < now() - make_interval(secs => $1)
			ORDER BY update_id
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE tg_update_queue t
		SET status = CASE WHEN t.attempts >= $2 THEN 'dead' ELSE 'pending' END,
		    available_at = now(), locked_by = NULL, heartbeat_at = NULL,
		    last_error = CASE WHEN t.attempts >= $2 THEN 'lease expired: the worker stopped heart-beating' ELSE t.last_error END,
		    finished_at = CASE WHEN t.attempts >= $2 THEN now() END
		FROM stale
		WHERE t.update_id = stale.update_id
		RETURNING t.status`, staleAfter.Seconds(), maxAttempts, reapBatch)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var requeued, dead int
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return requeued, dead, err
		}
		if s == "dead" {
			dead++
		} else {
			requeued++
		}
	}
	if err := rows.Err(); err != nil {
		return requeued, dead, err
	}
	if requeued+dead > 0 {
		q.notify(ctx)
	}
	return requeued, dead, nil
}

// Purge implements Queue (batched deletes).
func (q *Postgres) Purge(ctx context.Context, doneAge, deadAge time.Duration) (int64, error) {
	var total int64
	for _, p := range []struct {
		status string
		age    time.Duration
	}{{"done", doneAge}, {"dead", deadAge}} {
		if p.age <= 0 {
			continue
		}
		for {
			tag, err := q.pool.Exec(ctx, `
				DELETE FROM tg_update_queue WHERE update_id IN (
					SELECT update_id FROM tg_update_queue
					WHERE status = $1 AND finished_at < now() - make_interval(secs => $2)
					LIMIT $3)`, p.status, p.age.Seconds(), purgeBatch)
			if err != nil {
				return total, err
			}
			total += tag.RowsAffected()
			if tag.RowsAffected() < purgeBatch || ctx.Err() != nil {
				break
			}
		}
	}
	return total, nil
}

// Stats implements Queue.
func (q *Postgres) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := q.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'processing'),
		       count(*) FILTER (WHERE status = 'dead')
		FROM tg_update_queue WHERE status IN ('pending','processing','dead')`).Scan(&s.Pending, &s.Processing, &s.Dead)
	return s, err
}

// DeadLetter is one dead update (inspection / manual replay).
type DeadLetter struct {
	UpdateID  int64
	UserKey   int64
	Attempts  int
	LastError string
	Payload   string
}

// DeadLetters lists the newest dead updates.
func (q *Postgres) DeadLetters(ctx context.Context, limit int) ([]DeadLetter, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT update_id, user_key, attempts, COALESCE(last_error, ''), payload::text
		FROM tg_update_queue WHERE status = 'dead'
		ORDER BY finished_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeadLetter
	for rows.Next() {
		var d DeadLetter
		if err := rows.Scan(&d.UpdateID, &d.UserKey, &d.Attempts, &d.LastError, &d.Payload); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Requeue moves a dead update back to pending (manual replay).
func (q *Postgres) Requeue(ctx context.Context, updateID int64) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE tg_update_queue SET status = 'pending', attempts = 0, available_at = now(),
		       finished_at = NULL, last_error = NULL
		WHERE update_id = $1 AND status = 'dead'`, updateID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		q.notify(ctx)
	}
	return tag.RowsAffected() > 0, nil
}

// notify wakes the listening workers (best effort: they also poll).
func (q *Postgres) notify(ctx context.Context) {
	_, _ = q.pool.Exec(ctx, `SELECT pg_notify($1, '')`, ChannelUpdates)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// String is used in logs.
func (q *Postgres) String() string { return "postgres(max=" + strconv.FormatInt(q.max, 10) + ")" }

var _ Queue = (*Postgres)(nil)

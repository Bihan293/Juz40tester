package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Broadcast statuses.
const (
	BroadcastDraft    = "draft"
	BroadcastQueued   = "queued"
	BroadcastSending  = "sending"
	BroadcastDone     = "done"
	BroadcastCanceled = "canceled"
)

// Broadcast recipient statuses.
const (
	RecipientPending  = "pending"
	RecipientSending  = "sending"
	RecipientSent     = "sent"
	RecipientFailed   = "failed"
	RecipientBlocked  = "blocked"
	RecipientCanceled = "canceled"
)

// BroadcastButton is one URL button under a broadcast message.
type BroadcastButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Broadcast is one mass mailing.
type Broadcast struct {
	ID          int64
	AdminTgID   int64
	Status      string
	Text        string
	Entities    json.RawMessage // Telegram message entities of Text (nil = plain)
	MediaType   string          // "", "photo", "video"
	MediaFileID string
	Buttons     [][]BroadcastButton // rows of URL buttons
	Total       int
	Sent        int
	Failed      int
	Blocked     int
	LastError   string
	CreatedAt   time.Time
	QueuedAt    *time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	CanceledAt  *time.Time
}

const broadcastCols = `id, admin_tg_id, status, text, entities, media_type, media_file_id, buttons,
	total, sent, failed, blocked, last_error, created_at, queued_at, started_at, finished_at, canceled_at`

func scanBroadcasts(rows pgx.Rows) ([]Broadcast, error) {
	defer rows.Close()
	var out []Broadcast
	for rows.Next() {
		var b Broadcast
		var ent, btn []byte
		if err := rows.Scan(&b.ID, &b.AdminTgID, &b.Status, &b.Text, &ent, &b.MediaType, &b.MediaFileID, &btn,
			&b.Total, &b.Sent, &b.Failed, &b.Blocked, &b.LastError, &b.CreatedAt, &b.QueuedAt, &b.StartedAt,
			&b.FinishedAt, &b.CanceledAt); err != nil {
			return nil, err
		}
		if len(ent) > 0 && string(ent) != "null" {
			b.Entities = json.RawMessage(ent)
		}
		if len(btn) > 0 {
			if err := json.Unmarshal(btn, &b.Buttons); err != nil {
				// An old flat list ([{"text","url"}]) — one button per row.
				var flat []BroadcastButton
				if json.Unmarshal(btn, &flat) == nil {
					for _, x := range flat {
						b.Buttons = append(b.Buttons, []BroadcastButton{x})
					}
				}
			}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *AdminRepository) oneBroadcast(ctx context.Context, q pgxQuerier, sql string, args ...any) (*Broadcast, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	bs, err := scanBroadcasts(rows)
	if err != nil || len(bs) == 0 {
		return nil, err
	}
	return &bs[0], nil
}

type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// CreateBroadcastDraft stores a composed (not yet confirmed) broadcast.
func (r *AdminRepository) CreateBroadcastDraft(ctx context.Context, b *Broadcast) (*Broadcast, error) {
	btn, err := json.Marshal(b.Buttons)
	if err != nil {
		return nil, err
	}
	if b.Buttons == nil {
		btn = []byte("[]")
	}
	var ent []byte
	if len(b.Entities) > 0 {
		ent = b.Entities
	}
	return r.oneBroadcast(ctx, r.pool, `
		INSERT INTO broadcasts (admin_tg_id, status, text, entities, media_type, media_file_id, buttons)
		VALUES ($1, 'draft', $2, $3, $4, $5, $6)
		RETURNING `+broadcastCols, b.AdminTgID, b.Text, ent, b.MediaType, b.MediaFileID, btn)
}

// BroadcastByID loads a broadcast (ErrNotFound when missing).
func (r *AdminRepository) BroadcastByID(ctx context.Context, id int64) (*Broadcast, error) {
	b, err := r.oneBroadcast(ctx, r.pool, `SELECT `+broadcastCols+` FROM broadcasts WHERE id = $1`, id)
	if err == nil && b == nil {
		return nil, ErrNotFound
	}
	return b, err
}

// ListBroadcasts returns the latest broadcasts (drafts excluded).
func (r *AdminRepository) ListBroadcasts(ctx context.Context, limit int) ([]Broadcast, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+broadcastCols+` FROM broadcasts
		WHERE status <> 'draft' ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanBroadcasts(rows)
}

// CountBroadcastAudience is the number of users a broadcast would reach
// now (everybody except users who blocked the bot).
func (r *AdminRepository) CountBroadcastAudience(ctx context.Context) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM users WHERE blocked_at IS NULL`).Scan(&n)
	return n, err
}

// ConfirmBroadcast turns a draft into a queued broadcast and materialises
// its recipients (every user who has not blocked the bot) in ONE
// transaction. Idempotent: a second confirmation (double tap, retried
// update) changes nothing and returns changed = false.
func (r *AdminRepository) ConfirmBroadcast(ctx context.Context, id int64) (changed bool, total int, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE broadcasts SET status = 'queued', queued_at = now() WHERE id = $1 AND status = 'draft'`, id)
	if err != nil {
		return false, 0, err
	}
	if tag.RowsAffected() == 0 {
		return false, 0, tx.Commit(ctx)
	}
	tag, err = tx.Exec(ctx, `
		INSERT INTO broadcast_recipients (broadcast_id, user_id, telegram_id)
		SELECT $1, id, telegram_id FROM users WHERE blocked_at IS NULL
		ON CONFLICT DO NOTHING`, id)
	if err != nil {
		return false, 0, err
	}
	total = int(tag.RowsAffected())
	if _, err := tx.Exec(ctx, `UPDATE broadcasts SET total = $2 WHERE id = $1`, id, total); err != nil {
		return false, 0, err
	}
	return true, total, tx.Commit(ctx)
}

// BroadcastCounts are the live per-status recipient counts.
type BroadcastCounts struct {
	Total, Pending, Sending, Sent, Failed, Blocked, Canceled int
}

// Done is the number of processed recipients.
func (c BroadcastCounts) Done() int { return c.Sent + c.Failed + c.Blocked + c.Canceled }

// CountRecipients returns the live progress of a broadcast.
func (r *AdminRepository) CountRecipients(ctx context.Context, id int64) (BroadcastCounts, error) {
	var c BroadcastCounts
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int,
		       COUNT(*) FILTER (WHERE status = 'pending')::int,
		       COUNT(*) FILTER (WHERE status = 'sending')::int,
		       COUNT(*) FILTER (WHERE status = 'sent')::int,
		       COUNT(*) FILTER (WHERE status = 'failed')::int,
		       COUNT(*) FILTER (WHERE status = 'blocked')::int,
		       COUNT(*) FILTER (WHERE status = 'canceled')::int
		FROM broadcast_recipients WHERE broadcast_id = $1`, id).
		Scan(&c.Total, &c.Pending, &c.Sending, &c.Sent, &c.Failed, &c.Blocked, &c.Canceled)
	return c, err
}

// CancelBroadcast stops a broadcast: not yet sent recipients are skipped.
// changed = false when it was already finished / cancelled.
func (r *AdminRepository) CancelBroadcast(ctx context.Context, id int64) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE broadcasts SET status = 'canceled', canceled_at = now(), finished_at = now(),
		       locked_by = NULL, locked_until = NULL
		WHERE id = $1 AND status IN ('draft','queued','sending')`, id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE broadcast_recipients SET status = 'canceled', updated_at = now()
		WHERE broadcast_id = $1 AND status = 'pending'`, id); err != nil {
		return false, err
	}
	if err := storeBroadcastCounts(ctx, tx, id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func storeBroadcastCounts(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE broadcasts b SET total = c.total, sent = c.sent, failed = c.failed, blocked = c.blocked
		FROM (SELECT COUNT(*)::int AS total,
		             COUNT(*) FILTER (WHERE status = 'sent')::int AS sent,
		             COUNT(*) FILTER (WHERE status = 'failed')::int AS failed,
		             COUNT(*) FILTER (WHERE status = 'blocked')::int AS blocked
		      FROM broadcast_recipients WHERE broadcast_id = $1) c
		WHERE b.id = $1`, id)
	return err
}

// --- Sender side (one worker owns a broadcast at a time: lease) -----------------

// ClaimBroadcast takes the oldest queued broadcast (or one whose sender
// died: its lease expired) for this worker. Only ONE broadcast is sent at
// a time in the whole cluster, so the Telegram rate budget is not
// multiplied by the number of workers. Rows left in 'sending' by a dead
// sender are closed as failed («delivery unknown») — never re-sent, so
// nobody gets a message twice. nil = nothing to send.
func (r *AdminRepository) ClaimBroadcast(ctx context.Context, worker string, lease time.Duration) (*Broadcast, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b, err := r.oneBroadcast(ctx, tx, `
		UPDATE broadcasts SET status = 'sending', locked_by = $1, locked_until = now() + make_interval(secs => $2),
		       started_at = COALESCE(started_at, now())
		WHERE id = (SELECT id FROM broadcasts
		            WHERE status IN ('queued','sending')
		            ORDER BY queued_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
		  AND (status = 'queued' OR locked_until IS NULL OR locked_until < now() OR locked_by = $1)
		RETURNING `+broadcastCols, worker, lease.Seconds())
	if err != nil || b == nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE broadcast_recipients SET status = 'failed', error = 'прервано перезапуском (доставка неизвестна)', updated_at = now()
		WHERE broadcast_id = $1 AND status = 'sending'`, b.ID); err != nil {
		return nil, err
	}
	return b, tx.Commit(ctx)
}

// RenewBroadcastLease extends the lease; false = the broadcast was
// cancelled or taken over (the sender must stop).
func (r *AdminRepository) RenewBroadcastLease(ctx context.Context, id int64, worker string, lease time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE broadcasts SET locked_until = now() + make_interval(secs => $3)
		WHERE id = $1 AND status = 'sending' AND locked_by = $2`, id, worker, lease.Seconds())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseBroadcast gives the broadcast back (graceful shutdown): another
// worker continues at once.
func (r *AdminRepository) ReleaseBroadcast(ctx context.Context, id int64, worker string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE broadcasts SET locked_by = NULL, locked_until = NULL
		WHERE id = $1 AND status = 'sending' AND locked_by = $2`, id, worker)
	return err
}

// BroadcastRecipient is one pending delivery.
type BroadcastRecipient struct {
	BroadcastID int64
	UserID      int64
	TelegramID  int64
	Attempts    int
}

// DueRecipients returns up to limit pending recipients whose next try is
// due (oldest user first).
func (r *AdminRepository) DueRecipients(ctx context.Context, id int64, limit int) ([]BroadcastRecipient, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT broadcast_id, user_id, telegram_id, attempts FROM broadcast_recipients
		WHERE broadcast_id = $1 AND status = 'pending' AND next_at <= now()
		ORDER BY next_at, user_id LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BroadcastRecipient
	for rows.Next() {
		var x BroadcastRecipient
		if err := rows.Scan(&x.BroadcastID, &x.UserID, &x.TelegramID, &x.Attempts); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// NextDueIn is how long until the next pending recipient is due (0 = now
// or none pending).
func (r *AdminRepository) NextDueIn(ctx context.Context, id int64) (time.Duration, bool, error) {
	var next *time.Time
	var now time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT MIN(next_at), now() FROM broadcast_recipients WHERE broadcast_id = $1 AND status = 'pending'`, id).Scan(&next, &now)
	if err != nil || next == nil {
		return 0, false, err
	}
	if d := next.Sub(now); d > 0 {
		return d, true, nil
	}
	return 0, true, nil
}

// MarkRecipientSending records that the HTTP call for this recipient is
// about to be made. false = the row is no longer pending (cancelled) —
// do not send.
func (r *AdminRepository) MarkRecipientSending(ctx context.Context, id, userID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE broadcast_recipients SET status = 'sending', attempts = attempts + 1, updated_at = now()
		WHERE broadcast_id = $1 AND user_id = $2 AND status = 'pending'`, id, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FinishRecipient stores the final result of one delivery.
func (r *AdminRepository) FinishRecipient(ctx context.Context, id, userID int64, status, errText string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE broadcast_recipients SET status = $3, error = left($4, 500), updated_at = now()
		WHERE broadcast_id = $1 AND user_id = $2 AND status = 'sending'`, id, userID, status, errText)
	return err
}

// RetryRecipient puts a delivery that Telegram certainly did NOT accept
// (429, 5xx, network error before an answer) back to pending, due after
// `after`.
func (r *AdminRepository) RetryRecipient(ctx context.Context, id, userID int64, after time.Duration, errText string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE broadcast_recipients SET status = 'pending', next_at = now() + make_interval(secs => $3),
		       error = left($4, 500), updated_at = now()
		WHERE broadcast_id = $1 AND user_id = $2 AND status = 'sending'`, id, userID, after.Seconds(), errText)
	return err
}

// FinishBroadcastIfDone closes the broadcast when no recipient is left
// (pending / sending) and stores the final counters. It returns the
// finished broadcast to exactly ONE caller (nil otherwise) — that caller
// reports the result to the admin.
func (r *AdminRepository) FinishBroadcastIfDone(ctx context.Context, id int64) (*Broadcast, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE broadcasts SET status = 'done', finished_at = now(), locked_by = NULL, locked_until = NULL
		WHERE id = $1 AND status = 'sending'
		  AND NOT EXISTS (SELECT 1 FROM broadcast_recipients
		                  WHERE broadcast_id = $1 AND status IN ('pending','sending'))`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, tx.Commit(ctx)
	}
	if err := storeBroadcastCounts(ctx, tx, id); err != nil {
		return nil, err
	}
	b, err := r.oneBroadcast(ctx, tx, `SELECT `+broadcastCols+` FROM broadcasts WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	return b, tx.Commit(ctx)
}

// BroadcastStatus returns the current status of a broadcast.
func (r *AdminRepository) BroadcastStatus(ctx context.Context, id int64) (string, error) {
	var s string
	err := r.pool.QueryRow(ctx, `SELECT status FROM broadcasts WHERE id = $1`, id).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return s, err
}

// DeleteStaleDrafts removes drafts never confirmed (older than keep).
func (r *AdminRepository) DeleteStaleDrafts(ctx context.Context, keep time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM broadcasts WHERE status = 'draft' AND created_at < now() - make_interval(secs => $1)`, keep.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

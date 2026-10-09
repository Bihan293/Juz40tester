package repositories

// «✨ Свой тест» (custom tests): orders, the «waiting for the description»
// drafts and the custom tests themselves. Payments reuse the payments table
// of the billing (kind 'custom_test'), so the billing reconciler performs
// the Stars refunds exactly like for the weak-topics orders.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// customLockNS is the advisory-lock namespace of custom-order payments
// (one user's payments/free confirmations are applied one at a time, so
// the «3 per subject» and «one in flight» rules hold under races).
const customLockNS int32 = 40_030

// CustomTestRepository stores custom-test orders, drafts and tests.
type CustomTestRepository struct {
	pool *pgxpool.Pool
}

// NewCustomTestRepository creates the repository.
func NewCustomTestRepository(pool *pgxpool.Pool) *CustomTestRepository {
	return &CustomTestRepository{pool: pool}
}

// Custom order statuses (OrderCreated/OrderPaid/OrderFulfilled/OrderRefunded
// of the weak-test orders apply too).
const OrderCanceled = "canceled"

// CustomOrder is one requested custom test.
type CustomOrder struct {
	ID             int64
	UserID         int64
	TelegramUserID int64
	SubjectID      int64
	Amount         int
	Description    string
	Title          string
	Status         string
	ChargeID       string
	Free           bool
	JobID          int64
	TestID         int64
	CreatedAt      time.Time
	PaidAt         *time.Time
}

const customOrderCols = `id, user_id, telegram_user_id, subject_id, amount, description, title, status,
	COALESCE(charge_id, ''), free, COALESCE(job_id, 0), COALESCE(test_id, 0), created_at, paid_at`

func qualifiedCustomOrderCols(p string) string {
	return fmt.Sprintf(`%[1]sid, %[1]suser_id, %[1]stelegram_user_id, %[1]ssubject_id, %[1]samount, %[1]sdescription, %[1]stitle, %[1]sstatus,
	COALESCE(%[1]scharge_id, ''), %[1]sfree, COALESCE(%[1]sjob_id, 0), COALESCE(%[1]stest_id, 0), %[1]screated_at, %[1]spaid_at`, p)
}

func scanCustomOrders(rows pgx.Rows) ([]CustomOrder, error) {
	defer rows.Close()
	var out []CustomOrder
	for rows.Next() {
		var o CustomOrder
		if err := rows.Scan(&o.ID, &o.UserID, &o.TelegramUserID, &o.SubjectID, &o.Amount, &o.Description, &o.Title, &o.Status,
			&o.ChargeID, &o.Free, &o.JobID, &o.TestID, &o.CreatedAt, &o.PaidAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r *CustomTestRepository) oneOrder(ctx context.Context, q querierRows, sql string, args ...any) (*CustomOrder, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	os, err := scanCustomOrders(rows)
	if err != nil || len(os) == 0 {
		return nil, err
	}
	return &os[0], nil
}

type querierRows interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// --- Drafts («send me the description») ----------------------------------------

// SetDraft remembers that the user's next text message is the description
// of a custom test of the subject.
func (r *CustomTestRepository) SetDraft(ctx context.Context, userID, subjectID int64) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO custom_test_drafts (user_id, subject_id) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET subject_id = EXCLUDED.subject_id, created_at = now()`, userID, subjectID)
	return err
}

// Draft returns the subject of the user's draft younger than ttl (ok =
// false when there is none).
func (r *CustomTestRepository) Draft(ctx context.Context, userID int64, ttl time.Duration) (int64, bool, error) {
	var sid int64
	err := r.pool.QueryRow(ctx, `
		SELECT subject_id FROM custom_test_drafts
		WHERE user_id = $1 AND created_at > now() - make_interval(secs => $2)`, userID, ttl.Seconds()).Scan(&sid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return sid, err == nil, err
}

// ClearDraft forgets the user's draft (no-op when there is none).
func (r *CustomTestRepository) ClearDraft(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM custom_test_drafts WHERE user_id = $1`, userID)
	return err
}

// --- Tests -------------------------------------------------------------------------

// CustomTests returns the user's (unfinished) custom tests of the subject,
// oldest first.
func (r *CustomTestRepository) CustomTests(ctx context.Context, userID, subjectID int64) ([]models.Test, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+testColumns+` FROM tests
		WHERE kind = 'custom' AND owner_user_id = $1 AND subject_id = $2
		ORDER BY id`, userID, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Test
	for rows.Next() {
		t, err := scanTest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// CustomTestCounts returns the number of the user's custom tests per subject.
func (r *CustomTestRepository) CustomTestCounts(ctx context.Context, userID int64) (map[int64]int, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT subject_id, COUNT(*)::int FROM tests
		WHERE kind = 'custom' AND owner_user_id = $1 GROUP BY subject_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var sid int64
		var n int
		if err := rows.Scan(&sid, &n); err != nil {
			return nil, err
		}
		out[sid] = n
	}
	return out, rows.Err()
}

// slotsUsedSQL: custom tests of the subject + paid orders of the subject
// still being generated ($1 user, $2 subject).
const slotsUsedSQL = `
	SELECT (SELECT COUNT(*) FROM tests WHERE kind = 'custom' AND owner_user_id = $1 AND subject_id = $2)
	     + (SELECT COUNT(*) FROM custom_test_orders WHERE user_id = $1 AND subject_id = $2 AND status = 'paid')`

// SlotsUsed counts the user's custom tests of the subject plus the ones
// being generated (the «3 per subject» cap).
func (r *CustomTestRepository) SlotsUsed(ctx context.Context, userID, subjectID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, slotsUsedSQL, userID, subjectID).Scan(&n)
	return n, err
}

// DeleteCustomTest deletes the user's custom test («🏁 Закончить тест»)
// with its questions (they belong to this test only). ErrNotFound when the
// test is not a custom test of this user.
func (r *CustomTestRepository) DeleteCustomTest(ctx context.Context, userID, testID int64) (subjectID int64, err error) {
	err = r.pool.QueryRow(ctx, `SELECT subject_id FROM tests WHERE id = $1 AND owner_user_id = $2 AND kind = 'custom'`,
		testID, userID).Scan(&subjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return subjectID, deleteOwnedTest(ctx, r.pool, userID, testID, subjectID, models.TestKindCustom)
}

// CustomTestByOrder returns the test generated for the order (nil = none).
func (r *CustomTestRepository) CustomTestByOrder(ctx context.Context, orderID int64) (*models.Test, error) {
	return scanTest(r.pool.QueryRow(ctx, `SELECT `+testColumns+` FROM tests WHERE custom_order_id = $1`, orderID))
}

// --- Orders --------------------------------------------------------------------------

// CreateOrder creates the order of an accepted description. The user's
// other unpaid orders are cancelled (only the newest invoice is payable).
func (r *CustomTestRepository) CreateOrder(ctx context.Context, userID, tgUserID, subjectID int64, amount int, description, title string) (*CustomOrder, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE custom_test_orders SET status = 'canceled', updated_at = now()
		WHERE user_id = $1 AND status = 'created'`, userID); err != nil {
		return nil, err
	}
	o, err := r.oneOrder(ctx, tx, `
		INSERT INTO custom_test_orders (user_id, telegram_user_id, subject_id, amount, description, title)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+customOrderCols, userID, tgUserID, subjectID, amount, description, title)
	if err != nil {
		return nil, err
	}
	return o, tx.Commit(ctx)
}

// OrderByID loads an order (ErrNotFound when missing).
func (r *CustomTestRepository) OrderByID(ctx context.Context, id int64) (*CustomOrder, error) {
	o, err := r.oneOrder(ctx, r.pool, `SELECT `+customOrderCols+` FROM custom_test_orders WHERE id = $1`, id)
	if err == nil && o == nil {
		return nil, ErrNotFound
	}
	return o, err
}

// InflightOrder returns the user's paid order still being generated (nil =
// none; at most one exists).
func (r *CustomTestRepository) InflightOrder(ctx context.Context, userID int64) (*CustomOrder, error) {
	return r.oneOrder(ctx, r.pool, `SELECT `+customOrderCols+` FROM custom_test_orders WHERE user_id = $1 AND status = 'paid'`, userID)
}

// payableSQL is the guarded transition created → paid ($1 order, $2 user,
// $3 max per subject). Not payable: not this user's, not 'created'
// (cancelled by a newer description, already paid), another order of the
// user is in flight, or the subject already has max tests/generations.
const payableSQL = `
	UPDATE custom_test_orders o SET status = 'paid', paid_at = now(), next_check_at = now(), updated_at = now() %s
	WHERE o.id = $1 AND o.user_id = $2 AND o.status = 'created'
	  AND NOT EXISTS (SELECT 1 FROM custom_test_orders o2 WHERE o2.user_id = $2 AND o2.status = 'paid')
	  AND (SELECT COUNT(*) FROM tests WHERE kind = 'custom' AND owner_user_id = $2 AND subject_id = o.subject_id) < $3`

func lockUserCustom(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, customLockNS, int32(userID%2147483647))
	return err
}

// PayOrder records the payment of a custom order in ONE transaction: the
// payment row (idempotent by charge id) and the transition created → paid.
// duplicate = this charge was already recorded. accepted = false: the
// order was not payable — the payment is marked for refund.
func (r *CustomTestRepository) PayOrder(ctx context.Context, p PaymentRecord, maxPerSubject int) (duplicate, accepted bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUserCustom(ctx, tx, p.UserID); err != nil {
		return false, false, err
	}
	inserted, err := insertPayment(ctx, tx, p)
	if err != nil {
		return false, false, err
	}
	if !inserted {
		return true, false, tx.Commit(ctx)
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(payableSQL, `, charge_id = $4`), p.OrderID, p.UserID, maxPerSubject, p.ChargeID)
	if err != nil {
		return false, false, err
	}
	accepted = tag.RowsAffected() == 1
	decision, status := "custom_paid", "paid"
	if !accepted {
		decision, status = "custom_not_payable", refundStatus(p.AdminBypass)
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET decision = $2, status = $3, refund_reason = CASE WHEN $3 = 'paid' THEN '' ELSE $2 END,
		refunded_at = CASE WHEN $3 = 'refunded' THEN now() END
		WHERE charge_id = $1`, p.ChargeID, decision, status); err != nil {
		return false, false, err
	}
	return false, accepted, tx.Commit(ctx)
}

// ConfirmFree moves a free order (price 0 / subscriptions off) to paid
// under the same rules as PayOrder, without a payment.
func (r *CustomTestRepository) ConfirmFree(ctx context.Context, orderID, userID int64, maxPerSubject int) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockUserCustom(ctx, tx, userID); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf(payableSQL, `, free = TRUE`), orderID, userID, maxPerSubject)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}

// SetOrderJob links the generation job of a paid order.
func (r *CustomTestRepository) SetOrderJob(ctx context.Context, orderID, jobID int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE custom_test_orders SET job_id = $2, updated_at = now() WHERE id = $1`, orderID, jobID)
	return err
}

// ClaimDueOrders takes up to limit paid orders whose next check is due
// (lease: other workers skip them meanwhile).
func (r *CustomTestRepository) ClaimDueOrders(ctx context.Context, limit int, lease time.Duration) ([]CustomOrder, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE custom_test_orders o SET next_check_at = now() + make_interval(secs => $2), updated_at = now()
		FROM (SELECT id FROM custom_test_orders
		      WHERE status = 'paid' AND next_check_at <= now()
		      ORDER BY next_check_at LIMIT $1 FOR UPDATE SKIP LOCKED) d
		WHERE o.id = d.id
		RETURNING `+qualifiedCustomOrderCols("o."), limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return scanCustomOrders(rows)
}

// ClaimOrder takes ONE paid order right away (payment handler path): nil
// when another worker holds it or it is no longer paid.
func (r *CustomTestRepository) ClaimOrder(ctx context.Context, id int64, lease time.Duration) (*CustomOrder, error) {
	return r.oneOrder(ctx, r.pool, `
		UPDATE custom_test_orders SET next_check_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = $1 AND status = 'paid' AND next_check_at <= now()
		RETURNING `+customOrderCols, id, lease.Seconds())
}

// KickOrder makes a paid order due now (its generation finished).
func (r *CustomTestRepository) KickOrder(ctx context.Context, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE custom_test_orders SET next_check_at = now()
		WHERE id = $1 AND status = 'paid' AND next_check_at > now()`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// NoteRetry schedules the next check of a paid order.
func (r *CustomTestRepository) NoteRetry(ctx context.Context, id int64, next time.Duration, msg string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE custom_test_orders SET last_error = left($3, 500), next_check_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = $1 AND status = 'paid'`, id, next.Seconds(), msg)
	return err
}

// FulfillOrder marks the order fulfilled with its test. changed = this
// call did the transition (only that caller notifies the user).
func (r *CustomTestRepository) FulfillOrder(ctx context.Context, id, testID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE custom_test_orders SET status = 'fulfilled', test_id = $2, fulfilled_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'paid'`, id, testID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RefundOrder gives up on a paid order: order → refunded, its payment →
// refund_pending (the billing reconciler performs the Stars refund; an
// admin-bypass payment is closed at once) and its generation job, if still
// queued, is cancelled. changed = this call did the transition — a second
// call (another worker, a retry) changes nothing, so a payment is refunded
// exactly once.
func (r *CustomTestRepository) RefundOrder(ctx context.Context, id int64, reason string) (changed bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var charge string
	var jobID int64
	err = tx.QueryRow(ctx, `
		UPDATE custom_test_orders SET status = 'refunded', refunded_at = now(), last_error = left($2, 500), updated_at = now()
		WHERE id = $1 AND status = 'paid'
		RETURNING COALESCE(charge_id, ''), COALESCE(job_id, 0)`, id, reason).Scan(&charge, &jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if charge != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE payments SET status = `+refundStatusSQL+`, refund_reason = left($2, 500), next_check_at = now(),
			       refunded_at = CASE WHEN admin_bypass THEN now() END
			WHERE charge_id = $1 AND status = 'paid'`, charge, "custom_order:"+reason); err != nil {
			return false, err
		}
	}
	if jobID > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE generation_jobs SET status = 'failed', last_error = 'custom order refunded', updated_at = now()
			WHERE id = $1 AND status = 'pending'`, jobID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// stopPaidCustomOrderSQL moves the custom order paid with charge $1 from
// 'paid' (still being generated) to 'refunded' — the custom twin of
// stopPaidOrderSQL, used inside a WITH of the refund paths.
const stopPaidCustomOrderSQL = `UPDATE custom_test_orders SET status = 'refunded', refunded_at = now(),
			       last_error = 'payment refunded', updated_at = now()
			WHERE charge_id = $1 AND status = 'paid'
			RETURNING 1`

// OrderPaidForJob reports whether the order a custom job generates for is
// still paid (the generator never pays for a test whose order was refunded).
func (r *CustomTestRepository) OrderPaidForJob(ctx context.Context, orderID int64) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM custom_test_orders WHERE id = $1 AND status = 'paid')`, orderID).Scan(&ok)
	return ok, err
}

// isCustomPayment reports a payment kind of this file (used by the shared
// refund code).
func isCustomPayment(kind string) bool { return kind == billing.KindCustomTest }

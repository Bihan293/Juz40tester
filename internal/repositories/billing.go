package repositories

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

// ErrQuotaExceeded: the user has no test completions left today (the
// daily limit of the plan minus completed tests minus tests already
// started today and still open).
var ErrQuotaExceeded = errors.New("daily test quota exceeded")

// quotaLockNS is the advisory-lock namespace that serialises the quota
// check of one user (two simultaneous «start test» taps can not both pass
// the last free slot).
const quotaLockNS int32 = 40_026

// BillingRepository stores subscriptions, payments, weak-test orders and
// the daily quota. Every state lives in PostgreSQL (no process memory), so
// it works the same with ROLE=all and with web + N workers.
type BillingRepository struct {
	pool  *pgxpool.Pool
	cat   *billing.Catalog
	cal   billing.Calendar
	grace time.Duration
	now   func() time.Time
}

// NewBillingRepository creates the repository. grace extends every paid
// period (a renewal charged a little after the expiry date must not drop
// the user to Free in between).
func NewBillingRepository(pool *pgxpool.Pool, cat *billing.Catalog, loc *time.Location, grace time.Duration) *BillingRepository {
	return &BillingRepository{pool: pool, cat: cat, cal: billing.Calendar{Loc: loc}, grace: grace, now: time.Now}
}

// WithClock replaces the clock (tests: day switch, expiry).
func (r *BillingRepository) WithClock(now func() time.Time) *BillingRepository {
	r.now = now
	return r
}

// Catalog returns the plan catalog.
func (r *BillingRepository) Catalog() *billing.Catalog { return r.cat }

// Calendar returns the quota calendar.
func (r *BillingRepository) Calendar() billing.Calendar { return r.cal }

// Now is the repository clock.
func (r *BillingRepository) Now() time.Time { return r.now() }

// Grace is the expiry grace period.
func (r *BillingRepository) Grace() time.Duration { return r.grace }

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// --- Daily quota -----------------------------------------------------------------

// Usage is the quota picture of one user today.
type Usage struct {
	Plan      billing.Plan
	Stored    *billing.SubState // nil = never subscribed
	ExpiresAt time.Time         // end of the paid plan (zero on Free)
	Used      int               // completions charged today
	OpenToday int               // non-personal attempts started today, still open
	Limit     int
	Left      int // Limit - Used (≥ 0)
	CanStart  int // tests that may still be started today (≥ 0)
	Day       string
	ResetAt   time.Time
}

// usageSQL reads, for one user, the stored subscription, today's charged
// completions and today's open (non-personal) attempts other than
// excludeTest. Index-only work: user_subscriptions PK, daily_usage PK,
// test_attempts(user_id).
const usageSQL = `
	SELECT s.plan, s.expires_at, COALESCE(s.sub_charge_id, ''), COALESCE(s.charge_id, ''), s.source,
	       COALESCE((SELECT used FROM daily_usage WHERE user_id = $1 AND day = $2::date), 0),
	       (SELECT COUNT(*)::int FROM test_attempts a JOIN tests t ON t.id = a.test_id
	         WHERE a.user_id = $1 AND a.status = 'in_progress' AND a.started_at >= $3
	           AND t.kind <> 'personal' AND a.test_id <> $4)
	FROM (SELECT 1) one
	LEFT JOIN user_subscriptions s ON s.user_id = $1 AND s.status = 'active'`

func (r *BillingRepository) usage(ctx context.Context, q querier, userID, excludeTest int64) (*Usage, error) {
	now := r.now()
	day, start, next := r.cal.Day(now)
	var plan, subCharge, charge, source *string
	var expires *time.Time
	u := &Usage{Day: day, ResetAt: next}
	if err := q.QueryRow(ctx, usageSQL, userID, day, start, excludeTest).
		Scan(&plan, &expires, &subCharge, &charge, &source, &u.Used, &u.OpenToday); err != nil {
		return nil, err
	}
	if plan != nil && expires != nil {
		u.Stored = &billing.SubState{Plan: *plan, ExpiresAt: *expires, SubChargeID: deref(subCharge), ChargeID: deref(charge), Source: deref(source)}
		u.Plan = r.cat.Effective(*plan, *expires, now, r.grace)
		if !u.Plan.IsFree() {
			u.ExpiresAt = *expires
		}
	} else {
		u.Plan = r.cat.Free()
	}
	u.Limit = u.Plan.DailyLimit
	u.Left = billing.Left(u.Limit, u.Used)
	u.CanStart = billing.Remaining(u.Limit, u.Used, u.OpenToday)
	return u, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Usage returns today's quota picture of the user (one query).
func (r *BillingRepository) Usage(ctx context.Context, userID int64) (*Usage, error) {
	return r.usage(ctx, r.pool, userID, 0)
}

// CheckStartTx is the start gate of AttemptRepository: it runs INSIDE the
// transaction that creates a new attempt, under a per-user advisory lock,
// so concurrent starts can not both take the last slot. Personal
// (weak-topics) tests are paid separately and never limited.
func (r *BillingRepository) CheckStartTx(ctx context.Context, tx pgx.Tx, userID, testID int64) error {
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM tests WHERE id = $1`, testID).Scan(&kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if kind == models.TestKindPersonal {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, quotaLockNS, int32(userID%2147483647)); err != nil {
		return err
	}
	u, err := r.usage(ctx, tx, userID, testID)
	if err != nil {
		return err
	}
	if u.CanStart <= 0 {
		return ErrQuotaExceeded
	}
	return nil
}

// ChargeCompletionTx records the completion of an attempt and charges one
// test of today's quota — in the transaction of the final answer. The
// attempt_id primary key makes it idempotent (a second call for the same
// attempt changes nothing); personal tests are recorded but not charged.
func (r *BillingRepository) ChargeCompletionTx(ctx context.Context, tx pgx.Tx, userID, attemptID, testID int64) (bool, error) {
	now := r.now()
	day, _, _ := r.cal.Day(now)
	plan := ""
	if u, err := r.usage(ctx, tx, userID, 0); err == nil {
		plan = u.Plan.Code
	}
	var charged bool
	err := tx.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO test_completions (attempt_id, user_id, test_id, day, charged, plan)
			SELECT $1, $2, $3, $4::date, t.kind <> 'personal', $5
			FROM tests t WHERE t.id = $3
			ON CONFLICT (attempt_id) DO NOTHING
			RETURNING charged
		), up AS (
			INSERT INTO daily_usage (user_id, day, used)
			SELECT $2, $4::date, 1 FROM ins WHERE charged
			ON CONFLICT (user_id, day) DO UPDATE SET used = daily_usage.used + 1, updated_at = now()
			RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM up)`, attemptID, userID, testID, day, plan).Scan(&charged)
	return charged, err
}

// --- Subscriptions -----------------------------------------------------------------

// PaymentRecord is a successful payment as received from Telegram.
type PaymentRecord struct {
	ChargeID         string
	ProviderChargeID string
	UserID           int64
	TelegramUserID   int64
	Kind             string // billing.KindSubscription | billing.KindWeakTest
	Plan             string
	OrderID          int64
	Amount           int
	Currency         string
	Payload          string
	IsRecurring      bool
	IsFirstRecurring bool
	SubExpiresAt     time.Time
}

func insertPayment(ctx context.Context, tx pgx.Tx, p PaymentRecord) (bool, error) {
	var exp *time.Time
	if !p.SubExpiresAt.IsZero() {
		exp = &p.SubExpiresAt
	}
	var plan *string
	if p.Plan != "" {
		plan = &p.Plan
	}
	var order *int64
	if p.OrderID > 0 {
		order = &p.OrderID
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO payments (charge_id, provider_charge_id, user_id, telegram_user_id, kind, plan, order_id,
		                      amount, currency, payload, is_recurring, is_first_recurring, subscription_expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (charge_id) DO NOTHING`,
		p.ChargeID, p.ProviderChargeID, p.UserID, p.TelegramUserID, p.Kind, plan, order,
		p.Amount, p.Currency, p.Payload, p.IsRecurring, p.IsFirstRecurring, exp)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ApplySubscriptionPayment records a subscription payment and applies it
// to the user's subscription in ONE transaction. duplicate = the charge id
// was already recorded (nothing changed). A refund decision marks the
// payment refund_pending (the reconciler performs the refund).
func (r *BillingRepository) ApplySubscriptionPayment(ctx context.Context, p PaymentRecord) (duplicate bool, dec billing.SubDecision, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, dec, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Two different payments of one user at the same moment are applied one
	// after the other (the subscription row may not exist yet to lock).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, quotaLockNS+1, int32(p.UserID%2147483647)); err != nil {
		return false, dec, err
	}
	inserted, err := insertPayment(ctx, tx, p)
	if err != nil {
		return false, dec, err
	}
	if !inserted {
		return true, dec, tx.Commit(ctx)
	}
	var cur *billing.SubState
	var plan, source string
	var subCharge, charge *string
	var expires time.Time
	err = tx.QueryRow(ctx, `
		SELECT plan, expires_at, sub_charge_id, charge_id, source
		FROM user_subscriptions WHERE user_id = $1 AND status = 'active'
		FOR UPDATE`, p.UserID).Scan(&plan, &expires, &subCharge, &charge, &source)
	switch {
	case err == nil:
		cur = &billing.SubState{Plan: plan, ExpiresAt: expires, SubChargeID: deref(subCharge), ChargeID: deref(charge), Source: source}
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return false, dec, err
	}
	dec = r.cat.ApplySubscriptionPayment(cur, billing.SubPayment{
		Plan: p.Plan, ChargeID: p.ChargeID, ExpiresAt: p.SubExpiresAt,
		IsRecurring: p.IsRecurring, IsFirstRecurring: p.IsFirstRecurring,
	}, r.now(), r.grace)
	if dec.New != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_subscriptions (user_id, plan, status, expires_at, charge_id, sub_charge_id, source, started_at, updated_at)
			VALUES ($1, $2, 'active', $3, $4, $5, $6, now(), now())
			ON CONFLICT (user_id) DO UPDATE SET
				plan = EXCLUDED.plan, status = 'active', expires_at = EXCLUDED.expires_at,
				charge_id = EXCLUDED.charge_id, sub_charge_id = EXCLUDED.sub_charge_id,
				source = EXCLUDED.source,
				started_at = CASE WHEN user_subscriptions.plan = EXCLUDED.plan AND user_subscriptions.status = 'active'
				                  THEN user_subscriptions.started_at ELSE now() END,
				updated_at = now()`,
			p.UserID, dec.New.Plan, dec.New.ExpiresAt, dec.New.ChargeID, nullIfEmpty(dec.New.SubChargeID), dec.New.Source); err != nil {
			return false, dec, err
		}
	}
	status := "paid"
	reason := ""
	if dec.Refund {
		status, reason = "refund_pending", "subscription:"+dec.Kind
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET decision = $2, status = $3, refund_reason = $4 WHERE charge_id = $1`,
		p.ChargeID, dec.Kind, status, reason); err != nil {
		return false, dec, err
	}
	return false, dec, tx.Commit(ctx)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GrantPlan sets the user's plan by hand (admin): plan until `until`,
// source 'admin'. Plan "free" revokes the current plan.
func (r *BillingRepository) GrantPlan(ctx context.Context, userID int64, plan string, until time.Time) error {
	if plan == billing.PlanFree {
		_, err := r.pool.Exec(ctx, `UPDATE user_subscriptions SET status = 'revoked', updated_at = now() WHERE user_id = $1`, userID)
		return err
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO user_subscriptions (user_id, plan, status, expires_at, source)
		VALUES ($1, $2, 'active', $3, 'admin')
		ON CONFLICT (user_id) DO UPDATE SET plan = EXCLUDED.plan, status = 'active',
			expires_at = EXCLUDED.expires_at, source = 'admin', started_at = now(), updated_at = now()`,
		userID, plan, until)
	return err
}

// SubscriptionCharge returns the Telegram subscription id of the user's
// current subscription ("" when none / not a Stars subscription).
func (r *BillingRepository) SubscriptionCharge(ctx context.Context, userID int64) (string, error) {
	var s *string
	err := r.pool.QueryRow(ctx, `SELECT sub_charge_id FROM user_subscriptions WHERE user_id = $1 AND source = 'stars'`, userID).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return deref(s), err
}

// --- Refunds --------------------------------------------------------------------

// Payment is a stored payment (refund processing, admin info).
type Payment struct {
	ChargeID       string
	UserID         int64
	TelegramUserID int64
	Kind           string
	Plan           string
	OrderID        int64
	Amount         int
	Status         string
	Decision       string
	RefundReason   string
	Attempts       int
	CreatedAt      time.Time
}

// ClaimDueRefunds takes up to limit payments waiting for a refund (lease:
// next_check_at moves by lease, so another worker skips them meanwhile).
func (r *BillingRepository) ClaimDueRefunds(ctx context.Context, limit int, lease time.Duration) ([]Payment, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE payments p SET next_check_at = now() + make_interval(secs => $2)
		FROM (SELECT charge_id FROM payments
		      WHERE status = 'refund_pending' AND next_check_at <= now()
		      ORDER BY next_check_at LIMIT $1 FOR UPDATE SKIP LOCKED) d
		WHERE p.charge_id = d.charge_id
		RETURNING p.charge_id, p.user_id, p.telegram_user_id, p.kind, COALESCE(p.plan, ''), COALESCE(p.order_id, 0),
		          p.amount, p.status, p.decision, p.refund_reason, p.refund_attempts, p.created_at`,
		limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return scanPayments(rows)
}

func scanPayments(rows pgx.Rows) ([]Payment, error) {
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ChargeID, &p.UserID, &p.TelegramUserID, &p.Kind, &p.Plan, &p.OrderID,
			&p.Amount, &p.Status, &p.Decision, &p.RefundReason, &p.Attempts, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkRefunded records a completed refund (idempotent). changed = this
// call did the transition.
func (r *BillingRepository) MarkRefunded(ctx context.Context, chargeID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE payments SET status = 'refunded', refunded_at = now(), last_error = ''
		WHERE charge_id = $1 AND status <> 'refunded'`, chargeID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// NoteRefundFailure keeps the refund pending and schedules the next try.
func (r *BillingRepository) NoteRefundFailure(ctx context.Context, chargeID string, next time.Duration, msg string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE payments SET refund_attempts = refund_attempts + 1, last_error = left($3, 500),
		       next_check_at = now() + make_interval(secs => $2)
		WHERE charge_id = $1 AND status = 'refund_pending'`, chargeID, next.Seconds(), msg)
	return err
}

// RequestRefund marks a paid payment for refund (admin / failed order).
func (r *BillingRepository) RequestRefund(ctx context.Context, chargeID, reason string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE payments SET status = 'refund_pending', refund_reason = $2, next_check_at = now()
		WHERE charge_id = $1 AND status = 'paid'`, chargeID, reason)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// OnExternalRefund handles Telegram's refunded_payment (a refund made
// outside the reconciler): the payment is marked refunded and, when it
// paid for the current subscription, the plan ends now.
func (r *BillingRepository) OnExternalRefund(ctx context.Context, chargeID string) (*Payment, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		UPDATE payments SET status = 'refunded', refunded_at = COALESCE(refunded_at, now())
		WHERE charge_id = $1
		RETURNING charge_id, user_id, telegram_user_id, kind, COALESCE(plan, ''), COALESCE(order_id, 0),
		          amount, status, decision, refund_reason, refund_attempts, created_at`, chargeID)
	if err != nil {
		return nil, err
	}
	ps, err := scanPayments(rows)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	p := ps[0]
	if p.Kind == billing.KindSubscription {
		if _, err := tx.Exec(ctx, `
			UPDATE user_subscriptions SET status = 'revoked', expires_at = LEAST(expires_at, now()), updated_at = now()
			WHERE user_id = $1 AND charge_id = $2`, p.UserID, chargeID); err != nil {
			return nil, err
		}
	}
	return &p, tx.Commit(ctx)
}

// RecentPayments returns the latest payments of a user (admin info).
func (r *BillingRepository) RecentPayments(ctx context.Context, userID int64, limit int) ([]Payment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT charge_id, user_id, telegram_user_id, kind, COALESCE(plan, ''), COALESCE(order_id, 0),
		       amount, status, decision, refund_reason, refund_attempts, created_at
		FROM payments WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return scanPayments(rows)
}

// --- Weak-topics test orders ------------------------------------------------------

// Weak-test order statuses.
const (
	OrderCreated   = "created"
	OrderPaid      = "paid"
	OrderFulfilled = "fulfilled"
	OrderRefunded  = "refunded"
)

// WeakOrder is a paid weak-topics test order.
type WeakOrder struct {
	ID             int64
	UserID         int64
	TelegramUserID int64
	SubjectID      int64
	Amount         int
	Status         string
	ChargeID       string
	ReplaceTestID  int64
	TestID         int64
	GenAttempts    int
	CreatedAt      time.Time
	PaidAt         *time.Time
}

const weakOrderCols = `id, user_id, telegram_user_id, subject_id, amount, status, COALESCE(charge_id, ''),
	COALESCE(replace_test_id, 0), COALESCE(test_id, 0), gen_attempts, created_at, paid_at`

func scanWeakOrders(rows pgx.Rows) ([]WeakOrder, error) {
	defer rows.Close()
	var out []WeakOrder
	for rows.Next() {
		var o WeakOrder
		if err := rows.Scan(&o.ID, &o.UserID, &o.TelegramUserID, &o.SubjectID, &o.Amount, &o.Status, &o.ChargeID,
			&o.ReplaceTestID, &o.TestID, &o.GenAttempts, &o.CreatedAt, &o.PaidAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r *BillingRepository) oneOrder(ctx context.Context, sql string, args ...any) (*WeakOrder, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	os, err := scanWeakOrders(rows)
	if err != nil || len(os) == 0 {
		return nil, err
	}
	return &os[0], nil
}

// openOrderTTL: an unpaid invoice is re-used for this long, then replaced.
const openOrderTTL = 24 * time.Hour

// OpenWeakOrder returns the user's open (unpaid) order of the subject,
// creating it when there is none (one open invoice per user and subject).
// An order older than a day is cancelled and replaced (the price may have
// changed).
func (r *BillingRepository) OpenWeakOrder(ctx context.Context, userID, tgUserID, subjectID int64, amount int) (*WeakOrder, error) {
	if _, err := r.pool.Exec(ctx, `
		UPDATE weak_test_orders SET status = 'canceled', updated_at = now()
		WHERE user_id = $1 AND subject_id = $2 AND status = 'created'
		  AND (created_at < now() - make_interval(secs => $3) OR amount <> $4)`,
		userID, subjectID, openOrderTTL.Seconds(), amount); err != nil {
		return nil, err
	}
	o, err := r.oneOrder(ctx, `
		INSERT INTO weak_test_orders (user_id, telegram_user_id, subject_id, amount)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, subject_id) WHERE status = 'created' DO NOTHING
		RETURNING `+weakOrderCols, userID, tgUserID, subjectID, amount)
	if err != nil || o != nil {
		return o, err
	}
	return r.oneOrder(ctx, `SELECT `+weakOrderCols+` FROM weak_test_orders
		WHERE user_id = $1 AND subject_id = $2 AND status = 'created'`, userID, subjectID)
}

// WeakOrderByID loads an order.
func (r *BillingRepository) WeakOrderByID(ctx context.Context, id int64) (*WeakOrder, error) {
	o, err := r.oneOrder(ctx, `SELECT `+weakOrderCols+` FROM weak_test_orders WHERE id = $1`, id)
	if err == nil && o == nil {
		return nil, ErrNotFound
	}
	return o, err
}

// PaidWeakOrder returns the user's paid, not yet fulfilled order of the
// subject (nil when none).
func (r *BillingRepository) PaidWeakOrder(ctx context.Context, userID, subjectID int64) (*WeakOrder, error) {
	return r.oneOrder(ctx, `SELECT `+weakOrderCols+` FROM weak_test_orders
		WHERE user_id = $1 AND subject_id = $2 AND status = 'paid'`, userID, subjectID)
}

// PayWeakOrder records the payment of a weak-test order in ONE
// transaction: the payment row (idempotent by charge id) and the order
// transition created → paid. duplicate = this charge was already
// recorded. accepted = false: the order was not payable (already paid
// with another charge, cancelled, not this user's) — the payment is
// marked refund_pending and will be refunded.
func (r *BillingRepository) PayWeakOrder(ctx context.Context, p PaymentRecord, replaceTestID int64) (duplicate, accepted bool, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inserted, err := insertPayment(ctx, tx, p)
	if err != nil {
		return false, false, err
	}
	if !inserted {
		return true, false, tx.Commit(ctx)
	}
	var replace *int64
	if replaceTestID > 0 {
		replace = &replaceTestID
	}
	tag, err := tx.Exec(ctx, `
		UPDATE weak_test_orders SET status = 'paid', charge_id = $3, paid_at = now(), replace_test_id = $4,
		       next_check_at = now(), updated_at = now()
		WHERE id = $1 AND user_id = $2 AND status IN ('created','canceled')
		  AND NOT EXISTS (SELECT 1 FROM weak_test_orders o2
		                  WHERE o2.user_id = $2 AND o2.subject_id = weak_test_orders.subject_id AND o2.status = 'paid')`,
		p.OrderID, p.UserID, p.ChargeID, replace)
	if err != nil {
		return false, false, err
	}
	accepted = tag.RowsAffected() == 1
	decision, status := "weak_paid", "paid"
	if !accepted {
		decision, status = "weak_not_payable", "refund_pending"
	}
	if _, err := tx.Exec(ctx, `UPDATE payments SET decision = $2, status = $3, refund_reason = CASE WHEN $3 = 'paid' THEN '' ELSE $2 END
		WHERE charge_id = $1`, p.ChargeID, decision, status); err != nil {
		return false, false, err
	}
	return false, accepted, tx.Commit(ctx)
}

// ClaimDueWeakOrders takes up to limit paid orders whose next check is due
// (lease semantics like ClaimDueRefunds).
func (r *BillingRepository) ClaimDueWeakOrders(ctx context.Context, limit int, lease time.Duration) ([]WeakOrder, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE weak_test_orders o SET next_check_at = now() + make_interval(secs => $2), updated_at = now()
		FROM (SELECT id FROM weak_test_orders
		      WHERE status = 'paid' AND next_check_at <= now()
		      ORDER BY next_check_at LIMIT $1 FOR UPDATE SKIP LOCKED) d
		WHERE o.id = d.id
		RETURNING `+qualifiedWeakOrderCols("o."), limit, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return scanWeakOrders(rows)
}

// ClaimWeakOrder takes ONE paid order right away (payment handler path):
// nil when another worker holds it or it is no longer paid.
func (r *BillingRepository) ClaimWeakOrder(ctx context.Context, id int64, lease time.Duration) (*WeakOrder, error) {
	return r.oneOrder(ctx, `
		UPDATE weak_test_orders SET next_check_at = now() + make_interval(secs => $2), updated_at = now()
		WHERE id = $1 AND status = 'paid' AND next_check_at <= now()
		RETURNING `+weakOrderCols, id, lease.Seconds())
}

// KickWeakOrders makes the paid orders of a subject (optionally of one
// user) due now — a generation finished, the reconciler should look.
func (r *BillingRepository) KickWeakOrders(ctx context.Context, subjectID, userID int64) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE weak_test_orders SET next_check_at = now()
		WHERE status = 'paid' AND subject_id = $1 AND ($2 = 0 OR user_id = $2) AND next_check_at > now()`,
		subjectID, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ClearReplaceTest forgets the test to replace once it has been deleted.
func (r *BillingRepository) ClearReplaceTest(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE weak_test_orders SET replace_test_id = NULL WHERE id = $1`, id)
	return err
}

// FulfillWeakOrder marks the order fulfilled with its test. changed = this
// call did the transition (only that caller notifies the user).
func (r *BillingRepository) FulfillWeakOrder(ctx context.Context, id, testID int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE weak_test_orders SET status = 'fulfilled', test_id = $2, fulfilled_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'paid'`, id, testID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// NoteWeakOrderRetry schedules the next check of a paid order; newAttempt
// counts one more generation attempt.
func (r *BillingRepository) NoteWeakOrderRetry(ctx context.Context, id int64, newAttempt bool, next time.Duration, msg string) error {
	inc := 0
	if newAttempt {
		inc = 1
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE weak_test_orders SET gen_attempts = gen_attempts + $2, last_error = left($4, 500),
		       next_check_at = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1 AND status = 'paid'`, id, inc, next.Seconds(), msg)
	return err
}

// RefundWeakOrder gives up on a paid order: order → refunded, its payment
// → refund_pending (the refund itself is done by the reconciler). changed
// = this call did the transition.
func (r *BillingRepository) RefundWeakOrder(ctx context.Context, id int64, reason string) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var charge string
	err = tx.QueryRow(ctx, `
		UPDATE weak_test_orders SET status = 'refunded', refunded_at = now(), last_error = left($2, 500), updated_at = now()
		WHERE id = $1 AND status = 'paid'
		RETURNING COALESCE(charge_id, '')`, id, reason).Scan(&charge)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if charge != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE payments SET status = 'refund_pending', refund_reason = left($2, 500), next_check_at = now()
			WHERE charge_id = $1 AND status = 'paid'`, charge, "weak_order:"+reason); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// --- Cleanup ---------------------------------------------------------------------

// DeleteOldUsage deletes daily_usage / test_completions rows older than
// keep (bounded growth), in batches.
func (r *BillingRepository) DeleteOldUsage(ctx context.Context, keep time.Duration, limit int) (int64, error) {
	cutoff, _, _ := r.cal.Day(r.now().Add(-keep))
	var total int64
	for _, q := range []string{
		`DELETE FROM daily_usage WHERE ctid IN (SELECT ctid FROM daily_usage WHERE day < $1::date LIMIT $2)`,
		`DELETE FROM test_completions WHERE ctid IN (SELECT ctid FROM test_completions WHERE day < $1::date LIMIT $2)`,
		`DELETE FROM weak_test_orders WHERE ctid IN (SELECT ctid FROM weak_test_orders
		     WHERE status IN ('created','canceled') AND created_at < $1::date LIMIT $2)`,
	} {
		tag, err := r.pool.Exec(ctx, q, cutoff, limit)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// qualifiedWeakOrderCols is weakOrderCols with a table prefix, so the
// RETURNING of an UPDATE … FROM is unambiguous.
func qualifiedWeakOrderCols(prefix string) string {
	return fmt.Sprintf(`%[1]sid, %[1]suser_id, %[1]stelegram_user_id, %[1]ssubject_id, %[1]samount, %[1]sstatus, COALESCE(%[1]scharge_id, ''),
	COALESCE(%[1]sreplace_test_id, 0), COALESCE(%[1]stest_id, 0), %[1]sgen_attempts, %[1]screated_at, %[1]spaid_at`, prefix)
}

// UserIDByTelegram resolves a Telegram user id to the internal user id
// (admin commands). ErrNotFound when the user never used the bot.
func (r *BillingRepository) UserIDByTelegram(ctx context.Context, tgUserID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `SELECT id FROM users WHERE telegram_id = $1`, tgUserID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

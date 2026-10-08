package repositories

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

type billFixture struct {
	ctx       context.Context
	bill      *BillingRepository
	attempts  *AttemptRepository
	users     *UserRepository
	subjects  *SubjectRepository
	gen       *GenerationRepository
	sid       int64
	chain     []*models.Test
	personal  *models.Test
	user      *models.User
	tgID      int64
	clock     time.Time
	clockMu   sync.Mutex
	questions map[int64][]int64
}

func (f *billFixture) now() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.clock
}

func (f *billFixture) setNow(t time.Time) {
	f.clockMu.Lock()
	f.clock = t
	f.clockMu.Unlock()
}

func newBillFixture(t *testing.T) *billFixture {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	f := &billFixture{ctx: ctx, users: NewUserRepository(pool), subjects: NewSubjectRepository(pool),
		gen: NewGenerationRepository(pool), clock: time.Now(), questions: map[int64][]int64{}}
	f.bill = NewBillingRepository(pool, billing.MustCatalog(billing.DefaultPlans()), billing.LoadLocation("Asia/Almaty"), time.Hour).WithClock(f.now)
	f.attempts = NewAttemptRepository(pool).WithQuota(f.bill)
	sid, err := testutil.CreateSubject(ctx, pool, "BILL "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	f.sid = sid
	f.tgID = time.Now().UnixNano()%1_000_000_000 + 7_000_000_000
	f.user, err = f.users.Upsert(ctx, &models.User{TelegramID: f.tgID})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(n int, kind string) *models.Test {
		qs := []models.SeedQuestion{
			{Text: fmt.Sprintf("BILL %d вопрос 1 %d", n, sid), Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "Тема B", Difficulty: 2},
			{Text: fmt.Sprintf("BILL %d вопрос 2 %d", n, sid), Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "Тема B", Difficulty: 2},
		}
		tt := &models.Test{SubjectID: sid, TestNumber: n, Title: fmt.Sprintf("Тест %d", n), Kind: kind}
		if kind == models.TestKindPersonal {
			tt.OwnerUserID = f.user.ID
			tt.TestNumber = 0
			tt.Topics = []string{"Тема B"}
		}
		test, err := f.gen.CreateGeneratedTest(ctx, tt, qs)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := f.subjects.TestQuestionIDs(ctx, test.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.questions[test.ID] = ids
		return test
	}
	for n := 1; n <= 3; n++ {
		f.chain = append(f.chain, mk(n, models.TestKindChain))
	}
	f.personal = mk(0, models.TestKindPersonal)
	return f
}

func (f *billFixture) start(testID int64, replace bool) (*models.TestAttempt, error) {
	ids := f.questions[testID]
	orders := make([][]string, len(ids))
	for i := range orders {
		orders[i] = []string{"A", "B", "C", "D"}
	}
	if replace {
		return f.attempts.CreateAttempt(f.ctx, f.user.ID, testID, ids, orders, true)
	}
	return f.attempts.CreateFreshAttempt(f.ctx, f.user.ID, testID, ids, orders)
}

// finish answers every question of the attempt; returns the result of the
// last answer.
func (f *billFixture) finish(t *testing.T, a *models.TestAttempt) *AnswerResult {
	t.Helper()
	var last *AnswerResult
	for pos := 1; pos <= len(f.questions[a.TestID]); pos++ {
		res, err := f.attempts.SubmitAnswer(f.ctx, f.user.ID, a.ID, pos, "A")
		if err != nil {
			t.Fatalf("answer %d: %v", pos, err)
		}
		if pos < len(f.questions[a.TestID]) && res.Charged {
			t.Fatalf("charged before the last answer")
		}
		last = res
	}
	return last
}

func (f *billFixture) usage(t *testing.T) *Usage {
	t.Helper()
	u, err := f.bill.Usage(f.ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestQuotaStartChargeAndDayReset: Free = 1 completion per day; a started
// test takes the slot (no parallel starts), finishing charges exactly
// once, personal tests are free, the limit resets at Almaty midnight.
func TestQuotaStartChargeAndDayReset(t *testing.T) {
	f := newBillFixture(t)
	u := f.usage(t)
	if u.Plan.Code != billing.PlanFree || u.Limit != 1 || u.Left != 1 || u.CanStart != 1 {
		t.Fatalf("fresh user: %+v", u)
	}

	a1, err := f.start(f.chain[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if u := f.usage(t); u.OpenToday != 1 || u.CanStart != 0 || u.Left != 1 {
		t.Fatalf("after start: %+v", u)
	}
	// A second test can not be started while the first holds the slot …
	if _, err := f.start(f.chain[1].ID, false); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("parallel start: want ErrQuotaExceeded, got %v", err)
	}
	// … but re-opening the same test resumes the open attempt.
	again, err := f.start(f.chain[0].ID, false)
	if err != nil || again.ID != a1.ID {
		t.Fatalf("double tap: %v %+v", err, again)
	}
	// Personal (paid weak-topics) tests are never limited nor charged.
	pa, err := f.start(f.personal.ID, false)
	if err != nil {
		t.Fatalf("personal start: %v", err)
	}
	if res := f.finish(t, pa); !res.Finished || res.Charged {
		t.Fatalf("personal finish: %+v", res)
	}

	res := f.finish(t, a1)
	if !res.Finished || !res.Charged {
		t.Fatalf("chain finish: %+v", res)
	}
	if u := f.usage(t); u.Used != 1 || u.Left != 0 || u.CanStart != 0 || u.OpenToday != 0 {
		t.Fatalf("after finish: %+v", u)
	}
	// Idempotent: charging the same attempt again changes nothing.
	tx, err := f.bill.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	charged, err := f.bill.ChargeCompletionTx(f.ctx, tx, f.user.ID, a1.ID, a1.TestID)
	if err != nil || charged {
		t.Fatalf("second charge: %v %v", charged, err)
	}
	_ = tx.Commit(f.ctx)
	if u := f.usage(t); u.Used != 1 {
		t.Fatalf("double charge: %+v", u)
	}
	// A duplicate final answer is AlreadyAnswered / not in progress — no charge.
	if _, err := f.attempts.SubmitAnswer(f.ctx, f.user.ID, a1.ID, 2, "A"); err == nil {
		t.Fatalf("answer after finish must fail")
	}
	// Retrying a passed test is a NEW completion: refused today.
	if _, err := f.start(f.chain[0].ID, true); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("retry over limit: %v", err)
	}

	// Next day (Almaty midnight): the limit is back.
	u = f.usage(t)
	f.setNow(u.ResetAt.Add(time.Minute))
	if u := f.usage(t); u.Used != 0 || u.Left != 1 || u.CanStart != 1 {
		t.Fatalf("next day: %+v", u)
	}
	a2, err := f.start(f.chain[0].ID, true)
	if err != nil {
		t.Fatalf("retry next day: %v", err)
	}
	if res := f.finish(t, a2); !res.Charged {
		t.Fatalf("retry completion must be charged: %+v", res)
	}
	var days int
	_ = f.bill.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM daily_usage WHERE user_id = $1`, f.user.ID).Scan(&days)
	if days != 2 {
		t.Fatalf("daily_usage rows: %d", days)
	}
}

// TestQuotaConcurrentStarts: 10 simultaneous starts of different tests
// with one slot left — exactly one wins (advisory lock in the gate).
func TestQuotaConcurrentStarts(t *testing.T) {
	f := newBillFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := 0; i < 9; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.start(f.chain[i%3].ID, true)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrQuotaExceeded):
				refused++
			default:
				t.Errorf("start: %v", err)
			}
		}(i)
	}
	wg.Wait()
	var open int
	_ = f.bill.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM test_attempts WHERE user_id = $1 AND status = 'in_progress'`, f.user.ID).Scan(&open)
	if open != 1 {
		t.Fatalf("open attempts after concurrent starts: %d (ok=%d refused=%d)", open, ok, refused)
	}
}

// TestPlanGrantAndExpiry: a paid plan raises the limit until it expires
// (+ grace), then the user is on Free again without any job.
func TestPlanGrantAndExpiry(t *testing.T) {
	f := newBillFixture(t)
	until := f.now().Add(24 * time.Hour)
	if err := f.bill.GrantPlan(f.ctx, f.user.ID, billing.PlanPlus, until); err != nil {
		t.Fatal(err)
	}
	u := f.usage(t)
	if u.Plan.Code != billing.PlanPlus || u.Limit != 4 || u.CanStart != 4 || !u.ExpiresAt.Equal(until.Truncate(time.Microsecond)) {
		t.Fatalf("granted: %+v", u)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.start(f.chain[i].ID, false); err != nil {
			t.Fatalf("start %d on Plus: %v", i, err)
		}
	}
	if u := f.usage(t); u.CanStart != 1 {
		t.Fatalf("3 open of 4: %+v", u)
	}
	f.setNow(until.Add(30 * time.Minute)) // inside the grace hour
	if u := f.usage(t); u.Plan.Code != billing.PlanPlus {
		t.Fatalf("grace: %+v", u.Plan)
	}
	f.setNow(until.Add(2 * time.Hour))
	if u := f.usage(t); u.Plan.Code != billing.PlanFree || u.Limit != 1 || !u.ExpiresAt.IsZero() {
		t.Fatalf("expired: %+v", u)
	}
	if err := f.bill.GrantPlan(f.ctx, f.user.ID, billing.PlanFree, time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.setNow(time.Now())
	if u := f.usage(t); u.Plan.Code != billing.PlanFree {
		t.Fatalf("revoked: %+v", u)
	}
}

// TestSubscriptionPaymentsIdempotentUpgradeRefund: payments are applied
// once per charge id; upgrade switches at once; a cheaper renewal while a
// better plan is active is queued for refund.
func TestSubscriptionPaymentsIdempotentUpgradeRefund(t *testing.T) {
	f := newBillFixture(t)
	now := f.now()
	rec := func(plan, charge string, exp time.Time, recurring bool) PaymentRecord {
		return PaymentRecord{ChargeID: charge, UserID: f.user.ID, TelegramUserID: f.tgID, Kind: billing.KindSubscription,
			Plan: plan, Amount: 10, Currency: "XTR", Payload: billing.SubscriptionPayload(plan), SubExpiresAt: exp, IsRecurring: recurring}
	}
	pfx := fmt.Sprintf("t%d-", f.tgID)
	exp := now.Add(billing.SubscriptionPeriod).Truncate(time.Second)
	dup, dec, err := f.bill.ApplySubscriptionPayment(f.ctx, rec("plus", pfx+"c1", exp, false))
	if err != nil || dup || dec.Kind != billing.DecisionNew {
		t.Fatalf("first payment: %v %v %+v", err, dup, dec)
	}
	if u := f.usage(t); u.Plan.Code != "plus" || !u.ExpiresAt.Equal(exp) {
		t.Fatalf("plus active: %+v", u)
	}
	// Re-delivered update: nothing changes.
	dup, _, err = f.bill.ApplySubscriptionPayment(f.ctx, rec("plus", pfx+"c1", exp.Add(time.Hour), false))
	if err != nil || !dup {
		t.Fatalf("duplicate: %v %v", err, dup)
	}
	if u := f.usage(t); !u.ExpiresAt.Equal(exp) {
		t.Fatalf("duplicate changed the expiry: %s", u.ExpiresAt)
	}
	// Renewal.
	exp2 := exp.Add(billing.SubscriptionPeriod)
	if _, dec, err = f.bill.ApplySubscriptionPayment(f.ctx, rec("plus", pfx+"c2", exp2, true)); err != nil || dec.Kind != billing.DecisionRenewal {
		t.Fatalf("renewal: %v %+v", err, dec)
	}
	if u := f.usage(t); !u.ExpiresAt.Equal(exp2) || u.Stored.SubChargeID != pfx+"c1" {
		t.Fatalf("renewed: %+v %+v", u, u.Stored)
	}
	// Upgrade to Pro: at once, the Plus subscription must be cancelled.
	exp3 := now.Add(billing.SubscriptionPeriod + time.Hour).Truncate(time.Second)
	_, dec, err = f.bill.ApplySubscriptionPayment(f.ctx, rec("pro", pfx+"p1", exp3, false))
	if err != nil || dec.Kind != billing.DecisionUpgrade || dec.CancelChargeID != pfx+"c1" {
		t.Fatalf("upgrade: %v %+v", err, dec)
	}
	if u := f.usage(t); u.Plan.Code != "pro" || u.Limit != 10 {
		t.Fatalf("pro active: %+v", u)
	}
	// A late Plus renewal while Pro is active: refund.
	_, dec, err = f.bill.ApplySubscriptionPayment(f.ctx, rec("plus", pfx+"c3", exp2.Add(billing.SubscriptionPeriod), true))
	if err != nil || !dec.Refund {
		t.Fatalf("stale lower: %v %+v", err, dec)
	}
	if u := f.usage(t); u.Plan.Code != "pro" {
		t.Fatalf("stale renewal changed the plan: %+v", u.Plan)
	}
	due, err := f.bill.ClaimDueRefunds(f.ctx, 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range due {
		if p.ChargeID == pfx+"c3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("refund not claimed: %+v", due)
	}
	// The lease hides it from the next claim.
	again, _ := f.bill.ClaimDueRefunds(f.ctx, 100, time.Minute)
	for _, p := range again {
		if p.ChargeID == pfx+"c3" {
			t.Fatal("leased refund claimed twice")
		}
	}
	if ch, err := f.bill.MarkRefunded(f.ctx, pfx+"c3"); err != nil || !ch {
		t.Fatalf("mark refunded: %v %v", ch, err)
	}
	if ch, _ := f.bill.MarkRefunded(f.ctx, pfx+"c3"); ch {
		t.Fatal("mark refunded twice")
	}
	// Telegram refunded the Pro payment itself: the plan ends now.
	if p, err := f.bill.OnExternalRefund(f.ctx, pfx+"p1"); err != nil || p == nil {
		t.Fatalf("external refund: %v %+v", err, p)
	}
	if u := f.usage(t); u.Plan.Code != billing.PlanFree {
		t.Fatalf("after external refund: %+v", u.Plan)
	}
	ps, err := f.bill.RecentPayments(f.ctx, f.user.ID, 10)
	if err != nil || len(ps) != 4 {
		t.Fatalf("payment log: %v %d", err, len(ps))
	}
}

// TestWeakOrderLifecycle: one open invoice per subject, payment idempotent
// by charge id, a second payment of the same order is refunded, claim
// leases, refund of a failed order.
func TestWeakOrderLifecycle(t *testing.T) {
	f := newBillFixture(t)
	o1, err := f.bill.OpenWeakOrder(f.ctx, f.user.ID, f.tgID, f.sid, 10)
	if err != nil || o1 == nil || o1.Status != OrderCreated {
		t.Fatalf("open: %v %+v", err, o1)
	}
	o2, err := f.bill.OpenWeakOrder(f.ctx, f.user.ID, f.tgID, f.sid, 10)
	if err != nil || o2.ID != o1.ID {
		t.Fatalf("re-open must reuse the invoice: %v %+v", err, o2)
	}
	// A price change replaces the open order.
	o3, err := f.bill.OpenWeakOrder(f.ctx, f.user.ID, f.tgID, f.sid, 15)
	if err != nil || o3.ID == o1.ID || o3.Amount != 15 {
		t.Fatalf("price change: %v %+v", err, o3)
	}
	pfx := fmt.Sprintf("w%d-", f.tgID)
	rec := PaymentRecord{ChargeID: pfx + "1", UserID: f.user.ID, TelegramUserID: f.tgID, Kind: billing.KindWeakTest,
		OrderID: o3.ID, Amount: 15, Currency: "XTR", Payload: billing.WeakTestPayload(o3.ID)}
	dup, accepted, err := f.bill.PayWeakOrder(f.ctx, rec, f.personal.ID)
	if err != nil || dup || !accepted {
		t.Fatalf("pay: %v %v %v", err, dup, accepted)
	}
	dup, _, err = f.bill.PayWeakOrder(f.ctx, rec, 0)
	if err != nil || !dup {
		t.Fatalf("re-delivered payment: %v %v", err, dup)
	}
	// Another charge for the same (already paid) order: refunded.
	rec2 := rec
	rec2.ChargeID = pfx + "2"
	_, accepted, err = f.bill.PayWeakOrder(f.ctx, rec2, 0)
	if err != nil || accepted {
		t.Fatalf("second charge accepted: %v %v", err, accepted)
	}
	paid, err := f.bill.PaidWeakOrder(f.ctx, f.user.ID, f.sid)
	if err != nil || paid == nil || paid.ID != o3.ID || paid.ReplaceTestID != f.personal.ID || paid.PaidAt == nil {
		t.Fatalf("paid order: %v %+v", err, paid)
	}
	c1, err := f.bill.ClaimWeakOrder(f.ctx, o3.ID, time.Minute)
	if err != nil || c1 == nil {
		t.Fatalf("claim: %v %+v", err, c1)
	}
	if c2, _ := f.bill.ClaimWeakOrder(f.ctx, o3.ID, time.Minute); c2 != nil {
		t.Fatal("leased order claimed twice")
	}
	if n, err := f.bill.KickWeakOrders(f.ctx, f.sid, f.user.ID); err != nil || n != 1 {
		t.Fatalf("kick: %v %d", err, n)
	}
	if err := f.bill.NoteWeakOrderRetry(f.ctx, o3.ID, true, 0, "gen failed"); err != nil {
		t.Fatal(err)
	}
	if ch, err := f.bill.RefundWeakOrder(f.ctx, o3.ID, "timeout"); err != nil || !ch {
		t.Fatalf("refund order: %v %v", err, ch)
	}
	if ch, _ := f.bill.FulfillWeakOrder(f.ctx, o3.ID, f.personal.ID); ch {
		t.Fatal("a refunded order must not be fulfilled")
	}
	got, _ := f.bill.WeakOrderByID(f.ctx, o3.ID)
	if got.Status != OrderRefunded || got.GenAttempts != 1 {
		t.Fatalf("refunded order: %+v", got)
	}
	var st1, st2 string
	_ = f.bill.pool.QueryRow(f.ctx, `SELECT status FROM payments WHERE charge_id = $1`, pfx+"1").Scan(&st1)
	_ = f.bill.pool.QueryRow(f.ctx, `SELECT status FROM payments WHERE charge_id = $1`, pfx+"2").Scan(&st2)
	if st1 != "refund_pending" || st2 != "refund_pending" {
		t.Fatalf("payment statuses: %s %s", st1, st2)
	}
}

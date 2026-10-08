package services

// Integration tests of the Telegram Stars billing (skipped without
// TEST_DATABASE_URL): pre-checkout rules, idempotent payments, paid
// weak-topics orders built / retried / refunded through the reconciler.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

type fakeStars struct {
	mu       sync.Mutex
	refunds  []string
	cancels  []string
	failNext int
}

func (f *fakeStars) RefundStarPayment(_ context.Context, _ int64, charge string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return errors.New("telegram refundStarPayment: Bad Gateway")
	}
	f.refunds = append(f.refunds, charge)
	return nil
}

func (f *fakeStars) EditUserStarSubscription(_ context.Context, _ int64, charge string, canceled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if canceled {
		f.cancels = append(f.cancels, charge)
	}
	return nil
}

type fakeBuild struct {
	mu      sync.Mutex
	test    *models.Test
	pending bool
	topics  []string
	err     error
	calls   int
	tests   *fakeTests
}

func (f *fakeBuild) EnsurePersonalTestPaid(context.Context, int64, int64) (*models.Test, bool, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.pending && f.tests != nil {
		f.tests.mu.Lock()
		f.tests.job++ // every pending answer queued a NEW job
		f.tests.mu.Unlock()
	}
	return f.test, f.pending, f.topics, f.err
}

type fakeTests struct {
	mu      sync.Mutex
	current *models.Test
	deleted []int64
	job     int64
	topics  []string
}

func (f *fakeTests) FindPersonalTest(context.Context, int64, int64) (*models.Test, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, nil
}

func (f *fakeTests) DeletePersonalTest(_ context.Context, _, testID, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, testID)
	if f.current != nil && f.current.ID == testID {
		f.current = nil
		return nil
	}
	return repositories.ErrNotFound
}

func (f *fakeTests) ActivePersonalJobID(context.Context, int64, int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.job, nil
}

func (f *fakeTests) WeakTopicKeys(context.Context, int64, int64, int) ([]string, error) {
	return f.topics, nil
}

type fakeNotify struct {
	mu       sync.Mutex
	ready    []int64
	refunded []string
}

func (f *fakeNotify) WeakTestReady(_ context.Context, _ int64, t *models.Test) {
	f.mu.Lock()
	f.ready = append(f.ready, t.ID)
	f.mu.Unlock()
}

func (f *fakeNotify) PaymentRefunded(_ context.Context, _ int64, p repositories.Payment) {
	f.mu.Lock()
	f.refunded = append(f.refunded, p.ChargeID)
	f.mu.Unlock()
}

type billEnv struct {
	ctx    context.Context
	repo   *repositories.BillingRepository
	svc    *BillingService
	stars  *fakeStars
	build  *fakeBuild
	tests  *fakeTests
	notify *fakeNotify
	user   *models.User
	tgID   int64
	sid    int64
	exec   func(sql string, args ...any)
}

func newBillEnv(t *testing.T) *billEnv {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	e := &billEnv{ctx: ctx, stars: &fakeStars{}, tests: &fakeTests{topics: []string{"t1"}}, notify: &fakeNotify{}}
	e.build = &fakeBuild{topics: []string{"Тема"}, tests: e.tests}
	e.repo = repositories.NewBillingRepository(pool, billing.MustCatalog(billing.DefaultPlans()), billing.LoadLocation("Asia/Almaty"), time.Hour)
	e.svc = NewBillingService(e.repo, e.tests, e.build, BillingSettings{WeakTestPrice: 10, OrderTimeout: time.Hour, MaxGenAttempts: 2}).
		WithStars(e.stars).WithNotifier(e.notify)
	e.sid, err = testutil.CreateSubject(ctx, pool, "BILLSVC "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	e.tgID = time.Now().UnixNano()%1_000_000_000 + 8_000_000_000
	e.user, err = repositories.NewUserRepository(pool).Upsert(ctx, &models.User{TelegramID: e.tgID})
	if err != nil {
		t.Fatal(err)
	}
	e.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *billEnv) dueNow() {
	e.exec(`UPDATE weak_test_orders SET next_check_at = now() - interval '1 second' WHERE user_id = $1`, e.user.ID)
	e.exec(`UPDATE payments SET next_check_at = now() - interval '1 second' WHERE user_id = $1`, e.user.ID)
}

func TestBillingPreCheckoutAndSubscription(t *testing.T) {
	e := newBillEnv(t)
	pc := func(cur string, amount int, payload string) bool {
		ok, _ := e.svc.PreCheckout(e.ctx, e.user.ID, cur, amount, payload)
		return ok
	}
	if !pc("XTR", 10, billing.SubscriptionPayload("plus")) {
		t.Fatal("plus 10⭐ must pass")
	}
	if pc("XTR", 5, billing.SubscriptionPayload("plus")) || pc("USD", 10, billing.SubscriptionPayload("plus")) || pc("XTR", 10, "garbage") {
		t.Fatal("wrong amount / currency / payload must be refused")
	}
	charge := fmt.Sprintf("s%d-1", e.tgID)
	out, err := e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 40,
		Payload: billing.SubscriptionPayload("pro"), ChargeID: charge, SubExpiresAt: time.Now().Add(billing.SubscriptionPeriod)})
	if err != nil || out.Kind != OutcomeSubscription || out.Plan.Code != "pro" || out.Duplicate {
		t.Fatalf("pro payment: %v %+v", err, out)
	}
	out, err = e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 40,
		Payload: billing.SubscriptionPayload("pro"), ChargeID: charge})
	if err != nil || !out.Duplicate {
		t.Fatalf("duplicate: %v %+v", err, out)
	}
	if pc("XTR", 10, billing.SubscriptionPayload("plus")) || pc("XTR", 40, billing.SubscriptionPayload("pro")) {
		t.Fatal("downgrade / same plan must be refused while Pro is active")
	}
	if !pc("XTR", 50, billing.SubscriptionPayload("premium")) {
		t.Fatal("upgrade must pass")
	}
	_, err = e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 50,
		Payload: billing.SubscriptionPayload("premium"), ChargeID: charge + "u", SubExpiresAt: time.Now().Add(billing.SubscriptionPeriod)})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.stars.cancels) != 1 || e.stars.cancels[0] != charge {
		t.Fatalf("upgrade must cancel the Pro subscription: %v", e.stars.cancels)
	}
	if u, _ := e.svc.Usage(e.ctx, e.user.ID); u.Plan.Code != "premium" || u.Limit != 20 {
		t.Fatalf("premium: %+v", u)
	}
	// Admin grant / revoke.
	if _, _, err := e.svc.Grant(e.ctx, e.tgID, "plus", 3); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.svc.Usage(e.ctx, e.user.ID); u.Plan.Code != "plus" || u.Stored.Source != billing.SourceAdmin {
		t.Fatalf("granted: %+v", u)
	}
	if _, _, err := e.svc.Grant(e.ctx, e.tgID, "gold", 3); !errors.Is(err, ErrUnknownPlan) {
		t.Fatalf("unknown plan: %v", err)
	}
	if _, _, err := e.svc.Grant(e.ctx, 1, "plus", 3); !errors.Is(err, repositories.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

func (e *billEnv) payWeak(t *testing.T, charge string) (*repositories.WeakOrder, *PaymentOutcome) {
	t.Helper()
	o, err := e.svc.OpenWeakOrder(e.ctx, e.user.ID, e.tgID, e.sid)
	if err != nil || o == nil {
		t.Fatalf("open order: %v", err)
	}
	if ok, msg := e.svc.PreCheckout(e.ctx, e.user.ID, "XTR", 10, billing.WeakTestPayload(o.ID)); !ok {
		t.Fatalf("pre-checkout refused: %s", msg)
	}
	out, err := e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 10,
		Payload: billing.WeakTestPayload(o.ID), ChargeID: charge})
	if err != nil {
		t.Fatal(err)
	}
	return o, out
}

func TestBillingWeakTestReadyAtOnce(t *testing.T) {
	e := newBillEnv(t)
	old := &models.Test{ID: 9_000_001}
	e.tests.current = old
	e.build.test = &models.Test{ID: 9_000_002}
	charge := fmt.Sprintf("w%d-ready", e.tgID)
	o, out := e.payWeak(t, charge)
	if out.Kind != OutcomeWeakReady || out.Test.ID != 9_000_002 {
		t.Fatalf("outcome: %+v", out)
	}
	if len(e.tests.deleted) != 1 || e.tests.deleted[0] != old.ID {
		t.Fatalf("the replaced test must be deleted: %v", e.tests.deleted)
	}
	// The handler renders the ready message itself — no async notice.
	if len(e.notify.ready) != 0 {
		t.Fatalf("notified twice: %v", e.notify.ready)
	}
	// Paying the same order again is refused before charging.
	if ok, _ := e.svc.PreCheckout(e.ctx, e.user.ID, "XTR", 10, billing.WeakTestPayload(o.ID)); ok {
		t.Fatal("a paid order must not be payable again")
	}
	// Re-delivered successful_payment: nothing happens.
	out, err := e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 10,
		Payload: billing.WeakTestPayload(o.ID), ChargeID: charge})
	if err != nil || !out.Duplicate {
		t.Fatalf("duplicate: %v %+v", err, out)
	}
	if e.build.calls != 1 {
		t.Fatalf("generation must run once: %d", e.build.calls)
	}
	got, _ := e.repo.WeakOrderByID(e.ctx, o.ID)
	if got.Status != repositories.OrderFulfilled || got.TestID != 9_000_002 {
		t.Fatalf("order: %+v", got)
	}
}

func TestBillingWeakTestPendingThenReady(t *testing.T) {
	e := newBillEnv(t)
	e.build.pending = true
	o, out := e.payWeak(t, fmt.Sprintf("w%d-pend", e.tgID))
	if out.Kind != OutcomeWeakPending {
		t.Fatalf("outcome: %+v", out)
	}
	// The job finished: the hook kicks the order, the reconciler fulfils it
	// and notifies the user once.
	e.build.mu.Lock()
	e.build.pending, e.build.test = false, &models.Test{ID: 9_000_010}
	e.build.mu.Unlock()
	e.svc.OnGenerationFinished(e.ctx, &models.GenerationJob{Kind: models.TestKindPersonal, SubjectID: e.sid, OwnerUserID: e.user.ID})
	e.svc.ReconcileOnce(e.ctx)
	e.svc.ReconcileOnce(e.ctx)
	if len(e.notify.ready) != 1 || e.notify.ready[0] != 9_000_010 {
		t.Fatalf("ready notices: %v", e.notify.ready)
	}
	got, _ := e.repo.WeakOrderByID(e.ctx, o.ID)
	if got.Status != repositories.OrderFulfilled || got.GenAttempts != 1 {
		t.Fatalf("order: %+v", got)
	}
}

func TestBillingWeakTestFailedIsRefunded(t *testing.T) {
	e := newBillEnv(t)
	e.build.pending = true
	e.stars.failNext = 1 // the first refund call fails, the retry succeeds
	charge := fmt.Sprintf("w%d-fail", e.tgID)
	o, _ := e.payWeak(t, charge) // attempt 1 queued
	// Every generation fails: the job ends, a new one is queued (attempt 2),
	// then the order is refunded instead of a 3rd attempt (MaxGenAttempts=2).
	for i := 0; i < 2; i++ {
		e.tests.mu.Lock()
		e.tests.job = 0 // previous job failed — nothing active
		e.tests.mu.Unlock()
		e.dueNow()
		e.svc.ReconcileOnce(e.ctx)
	}
	got, _ := e.repo.WeakOrderByID(e.ctx, o.ID)
	if got.Status != repositories.OrderRefunded {
		t.Fatalf("order must be refunded: %+v", got)
	}
	if len(e.stars.refunds) != 0 || len(e.notify.refunded) != 0 {
		t.Fatalf("first refund call failed: %v %v", e.stars.refunds, e.notify.refunded)
	}
	e.dueNow()
	e.svc.ReconcileOnce(e.ctx)
	e.dueNow()
	e.svc.ReconcileOnce(e.ctx)
	if len(e.stars.refunds) != 1 || e.stars.refunds[0] != charge || len(e.notify.refunded) != 1 {
		t.Fatalf("refund: %v notices %v", e.stars.refunds, e.notify.refunded)
	}
	if e.build.calls != 2 {
		t.Fatalf("generation attempts: %d", e.build.calls)
	}
}

func TestBillingWeakTestTimeoutAndNoTopics(t *testing.T) {
	e := newBillEnv(t)
	e.build.pending = true
	o, _ := e.payWeak(t, fmt.Sprintf("w%d-to", e.tgID))
	e.exec(`UPDATE weak_test_orders SET paid_at = now() - interval '2 hours' WHERE id = $1`, o.ID)
	e.dueNow()
	e.svc.ReconcileOnce(e.ctx)
	if got, _ := e.repo.WeakOrderByID(e.ctx, o.ID); got.Status != repositories.OrderRefunded {
		t.Fatalf("timeout must refund: %+v", got)
	}

	// No weak topics any more: refused at pre-checkout.
	e.tests.topics = nil
	o2, err := e.svc.OpenWeakOrder(e.ctx, e.user.ID, e.tgID, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.PreCheckout(e.ctx, e.user.ID, "XTR", 10, billing.WeakTestPayload(o2.ID)); ok {
		t.Fatal("no weak topics: pre-checkout must refuse")
	}
	// … and if paid anyway, the order is refunded.
	e.build.pending, e.build.topics = false, nil
	out, err := e.svc.OnSuccessfulPayment(e.ctx, e.user.ID, e.tgID, PaymentInfo{Currency: "XTR", Amount: 10,
		Payload: billing.WeakTestPayload(o2.ID), ChargeID: fmt.Sprintf("w%d-nt", e.tgID)})
	if err != nil || out.Kind != OutcomeWeakRefunded {
		t.Fatalf("no topics: %v %+v", err, out)
	}
	// Another user can not pay this user's order.
	if ok, _ := e.svc.PreCheckout(e.ctx, e.user.ID+1, "XTR", 10, billing.WeakTestPayload(o2.ID)); ok {
		t.Fatal("foreign order must be refused")
	}
}

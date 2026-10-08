package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// StarsAPI is the part of the Telegram client the billing needs (refunds,
// cancelling the auto-renewal of a replaced subscription).
type StarsAPI interface {
	RefundStarPayment(ctx context.Context, tgUserID int64, chargeID string) error
	EditUserStarSubscription(ctx context.Context, tgUserID int64, chargeID string, canceled bool) error
}

// BillingNotifier tells users about asynchronous billing results (a paid
// weak-topics test became ready, a payment was refunded). Implemented by
// the handlers (Telegram messages).
type BillingNotifier interface {
	WeakTestReady(ctx context.Context, tgUserID int64, test *models.Test)
	PaymentRefunded(ctx context.Context, tgUserID int64, p repositories.Payment)
}

// paidPersonalBuilder builds a personal weak-topics test for a paid order
// (GeneratorService.EnsurePersonalTestPaid).
type paidPersonalBuilder interface {
	EnsurePersonalTestPaid(ctx context.Context, userID, subjectID int64) (*models.Test, bool, []string, error)
}

// personalTests is the part of GenerationRepository used by the billing.
type personalTests interface {
	FindPersonalTest(ctx context.Context, subjectID, userID int64) (*models.Test, error)
	DeletePersonalTest(ctx context.Context, userID, testID, subjectID int64) error
	ActivePersonalJobID(ctx context.Context, subjectID, userID int64) (int64, error)
	WeakTopicKeys(ctx context.Context, userID, subjectID int64, limit int) ([]string, error)
}

// BillingSettings tunes the billing service.
type BillingSettings struct {
	WeakTestPrice     int           // Stars per weak-topics test (0 = weak tests are free, no orders)
	OrderTimeout      time.Duration // paid order without a test → refund
	MaxGenAttempts    int           // generation attempts per paid order
	ReconcileInterval time.Duration
}

// BillingService implements subscriptions, the daily quota picture and
// the paid weak-topics tests on top of BillingRepository. It keeps no
// state in memory (except a wake-up channel): every decision is taken in
// PostgreSQL, so any instance (ROLE=web/worker/all) can process any
// payment update and the reconciler can run on every worker.
type BillingService struct {
	repo   *repositories.BillingRepository
	tests  personalTests
	build  paidPersonalBuilder
	stars  StarsAPI
	notify BillingNotifier
	set    BillingSettings
	wake   chan struct{}
	// isAdmin (optional) recognises administrators (ADMIN_IDS): their
	// purchases may be applied without an invoice (AdminBypassPurchase).
	isAdmin func(tgUserID int64) bool
}

// NewBillingService wires the service. build may be nil (AI generation
// disabled — paid orders are then fulfilled from the bank/clones only, or
// refunded).
func NewBillingService(repo *repositories.BillingRepository, tests personalTests, build paidPersonalBuilder, set BillingSettings) *BillingService {
	if set.MaxGenAttempts <= 0 {
		set.MaxGenAttempts = 3
	}
	if set.OrderTimeout <= 0 {
		set.OrderTimeout = 45 * time.Minute
	}
	if set.ReconcileInterval <= 0 {
		set.ReconcileInterval = time.Minute
	}
	return &BillingService{repo: repo, tests: tests, build: build, set: set, wake: make(chan struct{}, 1)}
}

// WithStars wires the Telegram Stars API (refunds / cancellations).
func (s *BillingService) WithStars(api StarsAPI) *BillingService { s.stars = api; return s }

// WithAdmins installs the administrator check used by AdminBypassPurchase
// (nil = nobody may bypass the payment).
func (s *BillingService) WithAdmins(isAdmin func(tgUserID int64) bool) *BillingService {
	s.isAdmin = isAdmin
	return s
}

// IsAdmin reports whether the Telegram user is an administrator.
func (s *BillingService) IsAdmin(tgUserID int64) bool { return s.isAdmin != nil && s.isAdmin(tgUserID) }

// WithNotifier wires the user notifications.
func (s *BillingService) WithNotifier(n BillingNotifier) *BillingService { s.notify = n; return s }

// Catalog returns the plan catalog.
func (s *BillingService) Catalog() *billing.Catalog { return s.repo.Catalog() }

// WeakTestPrice is the price of one weak-topics test (0 = free).
func (s *BillingService) WeakTestPrice() int { return s.set.WeakTestPrice }

// Now is the billing clock.
func (s *BillingService) Now() time.Time { return s.repo.Now() }

// Usage returns today's quota picture of the user.
func (s *BillingService) Usage(ctx context.Context, userID int64) (*repositories.Usage, error) {
	return s.repo.Usage(ctx, userID)
}

// CanBuy applies the purchase rules for the user's current plan.
func (s *BillingService) CanBuy(ctx context.Context, userID int64, plan string) (billing.Plan, billing.BuyVerdict, *repositories.Usage, error) {
	u, err := s.repo.Usage(ctx, userID)
	if err != nil {
		return billing.Plan{}, billing.BuyUnknownPlan, nil, err
	}
	p, v := s.Catalog().CanBuy(u.Plan, plan)
	return p, v, u, nil
}

// --- Pre-checkout ---------------------------------------------------------------

// Pre-checkout rejection texts (shown by Telegram in the payment form).
const (
	errPayStale     = "Счёт устарел. Открой меню и попробуй ещё раз."
	errPayActive    = "Этот план уже активен — он продлится автоматически."
	errPayDowngrade = "Сейчас активен более дорогой план. Перейти на этот можно после его окончания."
	errPayNoTopics  = "Слабых тем в этом предмете больше нет — оплата не нужна."
	errPayInternal  = "Не удалось проверить оплату. Попробуй через минуту."
	errPayInFlight  = "Тест по слабым темам этого предмета уже оплачен и собирается — я пришлю его, как только он будет готов."
)

// PreCheckout validates a payment before Telegram charges the user. ok =
// false comes with the text Telegram shows in the payment form.
func (s *BillingService) PreCheckout(ctx context.Context, userID int64, currency string, amount int, payload string) (bool, string) {
	pl, ok := billing.ParsePayload(payload)
	if !ok || currency != billing.CurrencyStars {
		return false, errPayStale
	}
	switch pl.Kind {
	case billing.KindSubscription:
		p, verdict, _, err := s.CanBuy(ctx, userID, pl.Plan)
		if err != nil {
			log.Printf("billing: pre-checkout user %d plan %s: %v", userID, pl.Plan, err)
			return false, errPayInternal
		}
		switch verdict {
		case billing.BuyAllowed:
		case billing.BuyAlreadyActive:
			return false, errPayActive
		case billing.BuyDowngrade:
			return false, errPayDowngrade
		default:
			return false, errPayStale
		}
		if amount != p.PriceStars {
			return false, errPayStale // the price changed since the link was made
		}
		return true, ""
	case billing.KindWeakTest:
		if s.set.WeakTestPrice <= 0 {
			return false, errPayStale
		}
		o, err := s.repo.WeakOrderByID(ctx, pl.OrderID)
		if errors.Is(err, repositories.ErrNotFound) {
			return false, errPayStale
		}
		if err != nil {
			log.Printf("billing: pre-checkout order %d: %v", pl.OrderID, err)
			return false, errPayInternal
		}
		if o.UserID != userID || o.Status != repositories.OrderCreated || o.Amount != amount {
			return false, errPayStale
		}
		// Another order of this subject is already paid and being built
		// (an older invoice / «Купить новый тест» tapped meanwhile): a
		// second payment could only be refunded — do not take it at all.
		inflight, err := s.repo.PaidWeakOrder(ctx, userID, o.SubjectID)
		if err != nil {
			log.Printf("billing: pre-checkout order %d in-flight lookup: %v", o.ID, err)
			return false, errPayInternal
		}
		if inflight != nil {
			return false, errPayInFlight
		}
		keys, err := s.tests.WeakTopicKeys(ctx, userID, o.SubjectID, 1)
		if err != nil {
			log.Printf("billing: pre-checkout order %d weak topics: %v", o.ID, err)
			return false, errPayInternal
		}
		if len(keys) == 0 {
			return false, errPayNoTopics
		}
		return true, ""
	}
	return false, errPayStale
}

// --- Successful payment -----------------------------------------------------------

// PaymentInfo is a successful_payment received from Telegram.
type PaymentInfo struct {
	Currency         string
	Amount           int
	Payload          string
	ChargeID         string
	ProviderChargeID string
	SubExpiresAt     time.Time
	IsRecurring      bool
	IsFirstRecurring bool
	// AdminBypass: a test purchase of an administrator (no real Stars).
	AdminBypass bool
}

// Payment outcome kinds.
const (
	OutcomeSubscription = "subscription"
	OutcomeWeakReady    = "weak_ready"    // the paid test exists — open it
	OutcomeWeakPending  = "weak_pending"  // being generated — the user is notified later
	OutcomeWeakRefunded = "weak_refunded" // could not be built — refunded
	OutcomeRejected     = "rejected"      // not payable (refund scheduled)
	OutcomeUnknown      = "unknown"
)

// PaymentOutcome tells the handler what to show after a payment.
type PaymentOutcome struct {
	Kind      string
	Duplicate bool // the payment had been processed already
	Decision  string
	Plan      billing.Plan
	ExpiresAt time.Time
	Test      *models.Test
}

// OnSuccessfulPayment records and applies a payment exactly once
// (idempotent by telegram_payment_charge_id).
func (s *BillingService) OnSuccessfulPayment(ctx context.Context, userID, tgUserID int64, pi PaymentInfo) (*PaymentOutcome, error) {
	pl, ok := billing.ParsePayload(pi.Payload)
	if !ok || pi.ChargeID == "" {
		// Not an invoice of this bot (or a bot of an older version):
		// nothing was bought — give the Stars back.
		log.Printf("billing: payment %q of tg user %d with unknown payload %q — refunding", pi.ChargeID, tgUserID, pi.Payload)
		if s.stars != nil && pi.ChargeID != "" {
			if err := s.stars.RefundStarPayment(ctx, tgUserID, pi.ChargeID); err != nil {
				log.Printf("billing: refund unknown payment %q: %v", pi.ChargeID, err)
			}
		}
		return &PaymentOutcome{Kind: OutcomeUnknown}, nil
	}
	rec := repositories.PaymentRecord{
		ChargeID: pi.ChargeID, ProviderChargeID: pi.ProviderChargeID, UserID: userID, TelegramUserID: tgUserID,
		Kind: pl.Kind, Plan: pl.Plan, OrderID: pl.OrderID, Amount: pi.Amount, Currency: pi.Currency,
		Payload: pi.Payload, IsRecurring: pi.IsRecurring, IsFirstRecurring: pi.IsFirstRecurring, SubExpiresAt: pi.SubExpiresAt,
		AdminBypass: pi.AdminBypass,
	}
	switch pl.Kind {
	case billing.KindSubscription:
		return s.applySubscription(ctx, tgUserID, rec)
	default:
		return s.applyWeakPayment(ctx, tgUserID, rec)
	}
}

func (s *BillingService) applySubscription(ctx context.Context, tgUserID int64, rec repositories.PaymentRecord) (*PaymentOutcome, error) {
	dup, dec, err := s.repo.ApplySubscriptionPayment(ctx, rec)
	if err != nil {
		return nil, err
	}
	out := &PaymentOutcome{Kind: OutcomeSubscription, Duplicate: dup, Decision: dec.Kind}
	if dup {
		log.Printf("billing: duplicate subscription payment %s (user %d) ignored", rec.ChargeID, rec.UserID)
	} else {
		metrics.Inc(metrics.Payments, "kind", billing.KindSubscription, "decision", decisionLabel(dec.Kind, rec.AdminBypass))
		log.Printf("billing: subscription payment %s: user %d (tg %d) plan %s %d⭐ recurring=%t first=%t admin_bypass=%t → %s",
			rec.ChargeID, rec.UserID, tgUserID, rec.Plan, rec.Amount, rec.IsRecurring, rec.IsFirstRecurring, rec.AdminBypass, dec.Kind)
		if dec.CancelChargeID != "" && s.stars != nil && !IsAdminBypassCharge(dec.CancelChargeID) {
			if err := s.stars.EditUserStarSubscription(ctx, tgUserID, dec.CancelChargeID, true); err != nil {
				log.Printf("billing: cancel auto-renewal of subscription %s (tg %d): %v", dec.CancelChargeID, tgUserID, err)
			}
		}
		if dec.Refund {
			s.Wake()
		}
	}
	u, err := s.repo.Usage(ctx, rec.UserID)
	if err == nil {
		out.Plan, out.ExpiresAt = u.Plan, u.ExpiresAt
	}
	if dec.Refund {
		out.Kind = OutcomeRejected
	}
	return out, nil
}

func (s *BillingService) applyWeakPayment(ctx context.Context, tgUserID int64, rec repositories.PaymentRecord) (*PaymentOutcome, error) {
	var replace int64
	if o, err := s.repo.WeakOrderByID(ctx, rec.OrderID); err == nil && o.UserID == rec.UserID {
		// The user explicitly paid for a NEW test of this subject: the
		// current personal test (if any) is replaced by it.
		if t, terr := s.tests.FindPersonalTest(ctx, o.SubjectID, rec.UserID); terr == nil && t != nil {
			replace = t.ID
		}
	}
	dup, accepted, err := s.repo.PayWeakOrder(ctx, rec, replace)
	if err != nil {
		return nil, err
	}
	if dup {
		log.Printf("billing: duplicate weak-test payment %s (user %d) ignored", rec.ChargeID, rec.UserID)
		return &PaymentOutcome{Kind: OutcomeWeakPending, Duplicate: true}, nil
	}
	metrics.Inc(metrics.Payments, "kind", billing.KindWeakTest, "decision", decisionLabel(fmt.Sprint(accepted), rec.AdminBypass))
	log.Printf("billing: weak-test payment %s: user %d (tg %d) order %d %d⭐ accepted=%t admin_bypass=%t",
		rec.ChargeID, rec.UserID, tgUserID, rec.OrderID, rec.Amount, accepted, rec.AdminBypass)
	if !accepted {
		s.Wake() // refund
		return &PaymentOutcome{Kind: OutcomeRejected}, nil
	}
	o, err := s.repo.ClaimWeakOrder(ctx, rec.OrderID, orderLease)
	if err != nil || o == nil {
		if err != nil {
			log.Printf("billing: claim order %d: %v", rec.OrderID, err)
		}
		s.Wake()
		return &PaymentOutcome{Kind: OutcomeWeakPending}, nil
	}
	state, test := s.processOrder(ctx, o, false)
	return &PaymentOutcome{Kind: state, Test: test}, nil
}

// orderLease: a claimed order is invisible to other workers this long.
const orderLease = 2 * time.Minute

// processOrder advances one claimed paid order: deletes the test it
// replaces, builds the paid test (bank / clone / AI generation, no daily
// limit), fulfils it, or refunds after OrderTimeout / MaxGenAttempts.
// notify = the user is told about the result here (reconciler); the
// payment handler renders the result itself.
func (s *BillingService) processOrder(ctx context.Context, o *repositories.WeakOrder, notify bool) (string, *models.Test) {
	refund := func(reason string) (string, *models.Test) {
		changed, err := s.repo.RefundWeakOrder(ctx, o.ID, reason)
		if err != nil {
			log.Printf("billing: refund order %d: %v", o.ID, err)
			return OutcomeWeakPending, nil
		}
		if changed {
			metrics.Inc(metrics.Payments, "kind", billing.KindWeakTest, "decision", "refund")
			log.Printf("billing: order %d (user %d subject %d) refunded: %s", o.ID, o.UserID, o.SubjectID, reason)
			s.Wake()
		}
		return OutcomeWeakRefunded, nil
	}
	retry := func(newAttempt bool, next time.Duration, msg string) (string, *models.Test) {
		if err := s.repo.NoteWeakOrderRetry(ctx, o.ID, newAttempt, next, msg); err != nil {
			log.Printf("billing: order %d retry: %v", o.ID, err)
		}
		return OutcomeWeakPending, nil
	}
	if o.ReplaceTestID > 0 {
		err := s.tests.DeletePersonalTest(ctx, o.UserID, o.ReplaceTestID, o.SubjectID)
		if err != nil && !errors.Is(err, repositories.ErrNotFound) {
			return retry(false, 30*time.Second, "delete replaced test: "+err.Error())
		}
		if err := s.repo.ClearReplaceTest(ctx, o.ID); err != nil {
			return retry(false, 30*time.Second, "clear replaced test: "+err.Error())
		}
		o.ReplaceTestID = 0
	}
	if o.PaidAt != nil && s.Now().Sub(*o.PaidAt) > s.set.OrderTimeout {
		// Last look: the test may have been built meanwhile.
		if t, err := s.tests.FindPersonalTest(ctx, o.SubjectID, o.UserID); err == nil && t != nil {
			return s.fulfil(ctx, o, t, notify)
		}
		return refund("timeout")
	}
	if s.build == nil {
		return refund("generation disabled")
	}
	activeBefore, err := s.tests.ActivePersonalJobID(ctx, o.SubjectID, o.UserID)
	if err != nil {
		return retry(false, 30*time.Second, err.Error())
	}
	if activeBefore == 0 && o.GenAttempts >= s.set.MaxGenAttempts {
		if t, err := s.tests.FindPersonalTest(ctx, o.SubjectID, o.UserID); err == nil && t != nil {
			return s.fulfil(ctx, o, t, notify)
		}
		return refund(fmt.Sprintf("generation failed %d times", o.GenAttempts))
	}
	test, pending, topics, err := s.build.EnsurePersonalTestPaid(ctx, o.UserID, o.SubjectID)
	switch {
	case err != nil:
		return retry(false, time.Minute, err.Error())
	case test != nil:
		return s.fulfil(ctx, o, test, notify)
	case len(topics) == 0:
		return refund("no weak topics")
	case !pending:
		return refund("generation unavailable")
	}
	activeAfter, _ := s.tests.ActivePersonalJobID(ctx, o.SubjectID, o.UserID)
	newAttempt := activeAfter != 0 && activeAfter != activeBefore
	return retry(newAttempt, 45*time.Second, "")
}

func (s *BillingService) fulfil(ctx context.Context, o *repositories.WeakOrder, t *models.Test, notify bool) (string, *models.Test) {
	changed, err := s.repo.FulfillWeakOrder(ctx, o.ID, t.ID)
	if err != nil {
		log.Printf("billing: fulfil order %d: %v", o.ID, err)
		return OutcomeWeakPending, nil
	}
	if changed {
		log.Printf("billing: order %d fulfilled with test %d (user %d)", o.ID, t.ID, o.UserID)
		if notify && s.notify != nil {
			s.notify.WeakTestReady(ctx, o.TelegramUserID, t)
		}
	}
	return OutcomeWeakReady, t
}

// --- Weak-topics orders (UI side) -------------------------------------------------

// OpenWeakOrder returns the invoice order for a new weak-topics test.
func (s *BillingService) OpenWeakOrder(ctx context.Context, userID, tgUserID, subjectID int64) (*repositories.WeakOrder, error) {
	return s.repo.OpenWeakOrder(ctx, userID, tgUserID, subjectID, s.set.WeakTestPrice)
}

// PaidWeakOrder returns the user's paid order of the subject that is still
// being built (nil when none).
func (s *BillingService) PaidWeakOrder(ctx context.Context, userID, subjectID int64) (*repositories.WeakOrder, error) {
	return s.repo.PaidWeakOrder(ctx, userID, subjectID)
}

// --- Refunds / external events -------------------------------------------------------

// OnRefundedPayment handles Telegram's refunded_payment service message.
// An error (database unavailable) is returned so the update is retried —
// otherwise a refunded subscription would stay active.
func (s *BillingService) OnRefundedPayment(ctx context.Context, chargeID string) error {
	p, err := s.repo.OnExternalRefund(ctx, chargeID)
	if err != nil {
		log.Printf("billing: refunded_payment %s: %v", chargeID, err)
		return fmt.Errorf("refunded_payment %s: %w", chargeID, err)
	}
	if p != nil {
		log.Printf("billing: payment %s (user %d, %s) refunded by Telegram", chargeID, p.UserID, p.Kind)
	}
	return nil
}

func (s *BillingService) processRefund(ctx context.Context, p repositories.Payment) {
	if p.AdminBypass || IsAdminBypassCharge(p.ChargeID) {
		// No real Stars were paid: never call refundStarPayment (the
		// repository already excludes these rows — defence in depth).
		if _, err := s.repo.MarkRefunded(ctx, p.ChargeID); err != nil {
			log.Printf("billing: close admin-bypass refund %s: %v", p.ChargeID, err)
		}
		return
	}
	if s.stars == nil {
		return
	}
	err := s.stars.RefundStarPayment(ctx, p.TelegramUserID, p.ChargeID)
	if err != nil && !isAlreadyRefunded(err) {
		next := refundBackoff(p.Attempts)
		log.Printf("billing: refund %s (tg %d, %d⭐) failed, retry in %s: %v", p.ChargeID, p.TelegramUserID, p.Amount, next, err)
		if nerr := s.repo.NoteRefundFailure(ctx, p.ChargeID, next, err.Error()); nerr != nil {
			log.Printf("billing: note refund failure %s: %v", p.ChargeID, nerr)
		}
		return
	}
	changed, merr := s.repo.MarkRefunded(ctx, p.ChargeID)
	if merr != nil {
		log.Printf("billing: mark refunded %s: %v", p.ChargeID, merr)
		return
	}
	if changed {
		metrics.Inc(metrics.Payments, "kind", p.Kind, "decision", "refunded")
		log.Printf("billing: refunded %s (tg %d, %d⭐, %s)", p.ChargeID, p.TelegramUserID, p.Amount, p.RefundReason)
		if s.notify != nil {
			s.notify.PaymentRefunded(ctx, p.TelegramUserID, p)
		}
	}
}

// isAlreadyRefunded: Telegram says the charge is already refunded (the
// refund is done — not an error).
func isAlreadyRefunded(err error) bool {
	return strings.Contains(strings.ToUpper(err.Error()), "ALREADY_REFUNDED")
}

// refundBackoff: 1, 2, 4 … minutes, at most 6 hours.
func refundBackoff(attempts int) time.Duration {
	d := time.Minute << min(attempts, 9)
	return min(d, 6*time.Hour)
}

// --- Reconciler ---------------------------------------------------------------------

// Wake makes the reconciler run at once (non-blocking).
func (s *BillingService) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// OnGenerationFinished is the generator's job-finished hook: a personal /
// topic-batch job of a subject finished, paid orders of that subject are
// re-checked right away (not at the next tick).
func (s *BillingService) OnGenerationFinished(ctx context.Context, job *models.GenerationJob) {
	if job == nil || job.Kind == models.TestKindChain {
		return
	}
	userID := int64(0)
	if job.Kind == models.TestKindPersonal {
		userID = job.OwnerUserID
	}
	n, err := s.repo.KickWeakOrders(ctx, job.SubjectID, userID)
	if err != nil {
		log.Printf("billing: kick orders of subject %d: %v", job.SubjectID, err)
		return
	}
	if n > 0 {
		s.Wake()
	}
}

// reconcileBatch bounds one pass (thousands of users: a pass never holds
// the pool for long; the next pass follows at once when a batch was full).
const reconcileBatch = 50

// ReconcileOnce advances due paid orders and pending refunds. Safe on any
// number of workers at once (lease + SKIP LOCKED). full = a batch was
// full (more work is waiting).
func (s *BillingService) ReconcileOnce(ctx context.Context) (full bool) {
	orders, err := s.repo.ClaimDueWeakOrders(ctx, reconcileBatch, orderLease)
	if err != nil {
		log.Printf("billing: claim due orders: %v", err)
	}
	for i := range orders {
		s.processOrder(ctx, &orders[i], true)
	}
	refunds, err := s.repo.ClaimDueRefunds(ctx, reconcileBatch, 5*time.Minute)
	if err != nil {
		log.Printf("billing: claim due refunds: %v", err)
	}
	for _, p := range refunds {
		s.processRefund(ctx, p)
	}
	return len(orders) == reconcileBatch || len(refunds) == reconcileBatch
}

// usageRetention: daily_usage / test_completions rows are kept this long.
const usageRetention = 90 * 24 * time.Hour

// Run is the reconciler loop of a worker: every ReconcileInterval and on
// every Wake; once a day it trims old quota rows.
func (s *BillingService) Run(ctx context.Context) {
	t := time.NewTicker(s.set.ReconcileInterval)
	defer t.Stop()
	lastCleanup := time.Time{}
	for {
		for s.ReconcileOnce(ctx) && ctx.Err() == nil {
		}
		if time.Since(lastCleanup) > 24*time.Hour {
			lastCleanup = time.Now()
			if n, err := s.repo.DeleteOldUsage(ctx, usageRetention, 5000); err != nil {
				log.Printf("billing: cleanup: %v", err)
			} else if n > 0 {
				log.Printf("billing: cleanup: %d old quota/order rows deleted", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// --- Admin ------------------------------------------------------------------------------

// ErrUnknownPlan: no such plan in the catalog.
var ErrUnknownPlan = errors.New("unknown plan")

// Grant sets a plan by hand (admin) for days days; plan "free" revokes.
func (s *BillingService) Grant(ctx context.Context, tgUserID int64, plan string, days int) (int64, time.Time, error) {
	p, ok := s.Catalog().Get(plan)
	if !ok {
		return 0, time.Time{}, ErrUnknownPlan
	}
	userID, err := s.repo.UserIDByTelegram(ctx, tgUserID)
	if err != nil {
		return 0, time.Time{}, err
	}
	until := s.Now().Add(time.Duration(days) * 24 * time.Hour)
	if err := s.repo.GrantPlan(ctx, userID, p.Code, until); err != nil {
		return 0, time.Time{}, err
	}
	log.Printf("billing: admin grant: tg %d (user %d) → %s until %s", tgUserID, userID, p.Code, until.Format(time.RFC3339))
	return userID, until, nil
}

// AdminInfo returns the quota picture and the latest payments of a user.
func (s *BillingService) AdminInfo(ctx context.Context, tgUserID int64) (*repositories.Usage, []repositories.Payment, error) {
	userID, err := s.repo.UserIDByTelegram(ctx, tgUserID)
	if err != nil {
		return nil, nil, err
	}
	u, err := s.repo.Usage(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	ps, err := s.repo.RecentPayments(ctx, userID, 10)
	return u, ps, err
}

// AdminRefund schedules the refund of a payment (the reconciler performs it).
func (s *BillingService) AdminRefund(ctx context.Context, chargeID string) (bool, error) {
	ok, err := s.repo.RequestRefund(ctx, chargeID, "admin")
	if ok {
		log.Printf("billing: admin refund requested for %s", chargeID)
		s.Wake()
	}
	return ok, err
}

// --- Admin-bypass purchases (test mode of the payments) ---------------------------

// AdminBypassChargePrefix starts the charge id of an admin-bypass purchase
// (a real Telegram charge id never looks like this).
const AdminBypassChargePrefix = "admin-bypass:"

// IsAdminBypassCharge reports whether chargeID belongs to an admin-bypass
// purchase.
func IsAdminBypassCharge(chargeID string) bool {
	return strings.HasPrefix(chargeID, AdminBypassChargePrefix)
}

// ErrNotAdmin: the Telegram user is not in ADMIN_IDS.
var ErrNotAdmin = errors.New("not an administrator")

// BypassRefusedError: the purchase is not allowed by the usual rules (the
// same checks as pre_checkout_query) — Reason is the user-facing text.
type BypassRefusedError struct{ Reason string }

func (e *BypassRefusedError) Error() string { return "admin bypass refused: " + e.Reason }

func decisionLabel(d string, bypass bool) string {
	if bypass {
		return d + "_admin_bypass"
	}
	return d
}

// AdminBypassPurchase applies a purchase of an ADMINISTRATOR at once, as
// if Telegram had confirmed the payment: no invoice, no payment window, no
// real Stars. It runs the very same pipeline as a real payment (the same
// pre-checkout rules, the same idempotent payment record — flagged
// admin_bypass — the same subscription / paid-order logic), so the admin
// tests exactly what a user gets. The administrator check is done HERE, on
// the server, whatever the caller checked before. Bypass payments are
// never refunded through Telegram.
func (s *BillingService) AdminBypassPurchase(ctx context.Context, userID, tgUserID int64, payload string) (*PaymentOutcome, error) {
	if !s.IsAdmin(tgUserID) {
		return nil, ErrNotAdmin
	}
	pl, ok := billing.ParsePayload(payload)
	if !ok {
		return nil, &BypassRefusedError{Reason: errPayStale}
	}
	amount := 0
	switch pl.Kind {
	case billing.KindSubscription:
		p, found := s.Catalog().Get(pl.Plan)
		if !found {
			return nil, &BypassRefusedError{Reason: errPayStale}
		}
		amount = p.PriceStars
	case billing.KindWeakTest:
		o, err := s.repo.WeakOrderByID(ctx, pl.OrderID)
		if err != nil {
			return nil, err
		}
		amount = o.Amount
	}
	if ok, msg := s.PreCheckout(ctx, userID, billing.CurrencyStars, amount, payload); !ok {
		return nil, &BypassRefusedError{Reason: msg}
	}
	pi := PaymentInfo{
		Currency: billing.CurrencyStars, Amount: amount, Payload: payload,
		ChargeID:    AdminBypassChargePrefix + randomID(),
		AdminBypass: true,
	}
	log.Printf("billing: ADMIN BYPASS purchase by tg %d (user %d): %s %d⭐ (no invoice, no real Stars)", tgUserID, userID, payload, amount)
	return s.OnSuccessfulPayment(ctx, userID, tgUserID, pi)
}

// randomID returns 16 random hex characters (unique charge ids).
func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

package services

// «✨ Свой тест»: a student picks a subject, describes the test they want
// (≤ 500 characters), a cheap AI pre-check accepts or refuses the request
// (a refusal costs nothing), the student pays CUSTOM_TEST_PRICE_STARS (free
// for ADMIN_IDS, free for everybody with the subscriptions off) and an
// urgent generation job writes the 20-question test. Like a weak-topics
// test it is finished at 15🟢 + 5🟡 («🏁 Закончить тест» deletes it).
//
// Rules: at most MaxPerSubject unfinished custom tests per subject (a test
// being generated counts), at most ONE paid generation in flight per user.
// A generation that fails for good or times out is refunded automatically,
// exactly once (the order's paid → refunded transition is the only path to
// the payment's refund).
//
// Like the billing, the service keeps no state in memory: every decision is
// taken in PostgreSQL, so any instance can serve any step.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// CustomNotifier tells users about asynchronous custom-test results.
type CustomNotifier interface {
	CustomTestReady(ctx context.Context, tgUserID int64, test *models.Test)
	// CustomTestFailed: a FREE / admin order could not be generated (paid
	// orders are announced by the refund notification of the billing).
	CustomTestFailed(ctx context.Context, tgUserID int64, o repositories.CustomOrder)
}

// customAI is the part of the generator the service needs.
type customAI interface {
	Enabled() bool
	CheckCustomRequest(ctx context.Context, subjectName, desc string) (*CustomVerdict, error)
	Wake()
}

// subjectLookup resolves subject names.
type subjectLookup interface {
	GetByID(ctx context.Context, id int64) (*models.Subject, error)
}

// CustomSettings tunes the custom tests.
type CustomSettings struct {
	Enabled           bool
	Price             int // Stars per test (0 = free)
	MaxPerSubject     int
	OrderTimeout      time.Duration
	ReconcileInterval time.Duration
	DraftTTL          time.Duration // how long «send the description» waits
}

// CustomTestService implements «✨ Свой тест».
type CustomTestService struct {
	repo     *repositories.CustomTestRepository
	gen      *repositories.GenerationRepository
	ai       customAI // nil on a ROLE=web process (pre-checkout only)
	subjects subjectLookup
	billing  *BillingService // nil = subscriptions off: tests are free
	notify   CustomNotifier
	isAdmin  func(tgUserID int64) bool
	set      CustomSettings
	wake     chan struct{}
}

// NewCustomTestService wires the service. ai may be nil (pre-checkout only).
func NewCustomTestService(repo *repositories.CustomTestRepository, gen *repositories.GenerationRepository, subjects subjectLookup, ai customAI, set CustomSettings) *CustomTestService {
	if set.MaxPerSubject <= 0 {
		set.MaxPerSubject = 3
	}
	if set.OrderTimeout <= 0 {
		set.OrderTimeout = 70 * time.Minute
	}
	if set.ReconcileInterval <= 0 {
		set.ReconcileInterval = time.Minute
	}
	if set.DraftTTL <= 0 {
		set.DraftTTL = 30 * time.Minute
	}
	s := &CustomTestService{repo: repo, gen: gen, subjects: subjects, set: set, wake: make(chan struct{}, 1)}
	if ai != nil {
		s.ai = ai
	}
	return s
}

// WithBilling wires the Stars billing (nil = free tests). The billing
// routes custom-test payments to this service.
func (s *CustomTestService) WithBilling(b *BillingService) *CustomTestService {
	s.billing = b
	if b != nil {
		b.custom = s
	}
	return s
}

// WithNotifier wires the user notifications.
func (s *CustomTestService) WithNotifier(n CustomNotifier) *CustomTestService { s.notify = n; return s }

// WithAdmins installs the administrator check (ADMIN_IDS: free tests).
func (s *CustomTestService) WithAdmins(isAdmin func(int64) bool) *CustomTestService {
	s.isAdmin = isAdmin
	return s
}

// Enabled reports whether new custom tests can be requested here.
func (s *CustomTestService) Enabled() bool {
	return s != nil && s.set.Enabled && s.ai != nil && s.ai.Enabled()
}

// Price is the price of one test in Stars (0 = free).
func (s *CustomTestService) Price() int {
	if s.billing == nil {
		return 0
	}
	return s.set.Price
}

// MaxPerSubject is the cap of unfinished custom tests per subject.
func (s *CustomTestService) MaxPerSubject() int { return s.set.MaxPerSubject }

// Payment modes of a new order.
const (
	CustomPayStars = "stars" // invoice
	CustomPayAdmin = "admin" // administrator: admin-bypass purchase, no Stars
	CustomPayFree  = "free"  // price 0 / subscriptions off
)

// PayMode tells how the user pays for a new test.
func (s *CustomTestService) PayMode(tgUserID int64) string {
	switch {
	case s.Price() <= 0:
		return CustomPayFree
	case s.isAdmin != nil && s.isAdmin(tgUserID):
		return CustomPayAdmin
	}
	return CustomPayStars
}

// --- Screens ---------------------------------------------------------------------------

// CustomSubjectState is what the subject screen shows.
type CustomSubjectState struct {
	Subject  *models.Subject
	Tests    []models.Test
	Inflight *repositories.CustomOrder // the user's order being generated (any subject)
	Used     int                       // tests + generations of THIS subject
}

// SubjectState loads the custom-test screen of a subject.
func (s *CustomTestService) SubjectState(ctx context.Context, userID, subjectID int64) (*CustomSubjectState, error) {
	subj, err := s.subjects.GetByID(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	tests, err := s.repo.CustomTests(ctx, userID, subjectID)
	if err != nil {
		return nil, err
	}
	inflight, err := s.repo.InflightOrder(ctx, userID)
	if err != nil {
		return nil, err
	}
	st := &CustomSubjectState{Subject: subj, Tests: tests, Inflight: inflight, Used: len(tests)}
	if inflight != nil && inflight.SubjectID == subjectID {
		st.Used++
	}
	return st, nil
}

// Counts returns the user's custom tests per subject (subject picker).
func (s *CustomTestService) Counts(ctx context.Context, userID int64) (map[int64]int, error) {
	return s.repo.CustomTestCounts(ctx, userID)
}

// Refusals of a new test before any AI call / payment.
var (
	// ErrCustomInFlight: another custom test of the user is being generated.
	ErrCustomInFlight = errors.New("a custom test is already being generated")
	// ErrCustomCap: the subject already has MaxPerSubject custom tests.
	ErrCustomCap = errors.New("custom test cap of the subject reached")
	// ErrCustomDisabled: the feature is off / no AI provider.
	ErrCustomDisabled = errors.New("custom tests are disabled")
)

// canStart applies the cap and the one-in-flight rule.
func (s *CustomTestService) canStart(ctx context.Context, userID, subjectID int64) error {
	inflight, err := s.repo.InflightOrder(ctx, userID)
	if err != nil {
		return err
	}
	if inflight != nil {
		return ErrCustomInFlight
	}
	used, err := s.repo.SlotsUsed(ctx, userID, subjectID)
	if err != nil {
		return err
	}
	if used >= s.set.MaxPerSubject {
		return ErrCustomCap
	}
	return nil
}

// StartDraft: «➕ Новый тест» — the user's next text message is the
// description of a test of the subject.
func (s *CustomTestService) StartDraft(ctx context.Context, userID, subjectID int64) error {
	if !s.Enabled() {
		return ErrCustomDisabled
	}
	if err := s.canStart(ctx, userID, subjectID); err != nil {
		return err
	}
	return s.repo.SetDraft(ctx, userID, subjectID)
}

// Draft returns the subject the user is writing a description for.
func (s *CustomTestService) Draft(ctx context.Context, userID int64) (int64, bool, error) {
	return s.repo.Draft(ctx, userID, s.set.DraftTTL)
}

// ClearDraft cancels the «send the description» step.
func (s *CustomTestService) ClearDraft(ctx context.Context, userID int64) error {
	return s.repo.ClearDraft(ctx, userID)
}

// Submit outcomes.
const (
	CustomSubmitTooLong     = "too_long"
	CustomSubmitTooShort    = "too_short"
	CustomSubmitRefused     = "refused"     // the AI pre-check refused (Reason)
	CustomSubmitInFlight    = "in_flight"   // another generation is running
	CustomSubmitCap         = "cap"         // the subject is full
	CustomSubmitUnavailable = "unavailable" // the pre-check could not run
	CustomSubmitAccepted    = "accepted"    // Order created — pay / confirm it
)

// CustomSubmit is the result of a description.
type CustomSubmit struct {
	Kind    string
	Reason  string
	Runes   int
	Subject *models.Subject
	Order   *repositories.CustomOrder
}

// SubmitDescription checks a description and, when the AI pre-check
// accepts it, creates the order (nothing is charged here). The draft stays
// on refusals (the student may write again) and is cleared on acceptance.
func (s *CustomTestService) SubmitDescription(ctx context.Context, userID, tgUserID, subjectID int64, text string) (*CustomSubmit, error) {
	if !s.Enabled() {
		return nil, ErrCustomDisabled
	}
	desc := strings.TrimSpace(strings.ToValidUTF8(text, ""))
	n := utf8.RuneCountInString(desc)
	switch {
	case n > CustomDescMaxRunes:
		return &CustomSubmit{Kind: CustomSubmitTooLong, Runes: n}, nil
	case n < CustomDescMinRunes:
		return &CustomSubmit{Kind: CustomSubmitTooShort, Runes: n}, nil
	}
	switch err := s.canStart(ctx, userID, subjectID); {
	case errors.Is(err, ErrCustomInFlight):
		return &CustomSubmit{Kind: CustomSubmitInFlight}, nil
	case errors.Is(err, ErrCustomCap):
		return &CustomSubmit{Kind: CustomSubmitCap}, nil
	case err != nil:
		return nil, err
	}
	subj, err := s.subjects.GetByID(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	v, err := s.ai.CheckCustomRequest(ctx, subj.Name, desc)
	if err != nil {
		log.Printf("custom: pre-check user %d subject %d: %v", userID, subjectID, err)
		return &CustomSubmit{Kind: CustomSubmitUnavailable, Subject: subj}, nil
	}
	if !v.OK {
		metrics.Inc(metrics.Payments, "kind", billing.KindCustomTest, "decision", "precheck_refused")
		log.Printf("custom: request of user %d (subject %d, %d chars) REFUSED by the pre-check: %s", userID, subjectID, n, v.Reason)
		return &CustomSubmit{Kind: CustomSubmitRefused, Reason: v.Reason, Subject: subj}, nil
	}
	o, err := s.repo.CreateOrder(ctx, userID, tgUserID, subjectID, s.Price(), desc, v.Title)
	if err != nil {
		return nil, err
	}
	if err := s.repo.ClearDraft(ctx, userID); err != nil {
		log.Printf("custom: clear draft of user %d: %v", userID, err)
	}
	log.Printf("custom: order %d created: user %d subject %d %d⭐ title %q (%d chars)", o.ID, userID, subjectID, o.Amount, o.Title, n)
	return &CustomSubmit{Kind: CustomSubmitAccepted, Subject: subj, Order: o}, nil
}

// --- Payment ------------------------------------------------------------------------

// Pre-checkout refusal texts.
const (
	errCustomInFlight = "Другой твой тест уже генерируется — дождись его, а потом закажи следующий."
	errCustomCap      = "В этом предмете уже максимум своих тестов — закончи один из них."
)

// PreCheckout validates a custom-test payment before Telegram charges it.
func (s *CustomTestService) PreCheckout(ctx context.Context, userID int64, amount int, orderID int64) (bool, string) {
	if !s.set.Enabled {
		return false, errPayStale
	}
	o, err := s.repo.OrderByID(ctx, orderID)
	if errors.Is(err, repositories.ErrNotFound) {
		return false, errPayStale
	}
	if err != nil {
		log.Printf("custom: pre-checkout order %d: %v", orderID, err)
		return false, errPayInternal
	}
	if o.UserID != userID || o.Status != repositories.OrderCreated || o.Amount != amount {
		return false, errPayStale
	}
	switch err := s.canStart(ctx, userID, o.SubjectID); {
	case errors.Is(err, ErrCustomInFlight):
		return false, errCustomInFlight
	case errors.Is(err, ErrCustomCap):
		return false, errCustomCap
	case err != nil:
		log.Printf("custom: pre-checkout order %d rules: %v", orderID, err)
		return false, errPayInternal
	}
	return true, ""
}

// OrderAmount is the price of an order (admin-bypass purchases).
func (s *CustomTestService) OrderAmount(ctx context.Context, orderID int64) (int, error) {
	o, err := s.repo.OrderByID(ctx, orderID)
	if err != nil {
		return 0, err
	}
	return o.Amount, nil
}

// Payment outcome kinds of custom tests.
const (
	OutcomeCustomPending  = "custom_pending"  // paid, the test is being generated
	OutcomeCustomReady    = "custom_ready"    // the test exists already
	OutcomeCustomRefunded = "custom_refunded" // could not be generated — refunded
)

// ApplyPayment records a custom-test payment (called by the billing,
// idempotent by charge id) and starts the generation.
func (s *CustomTestService) ApplyPayment(ctx context.Context, tgUserID int64, rec repositories.PaymentRecord) (*PaymentOutcome, error) {
	dup, accepted, err := s.repo.PayOrder(ctx, rec, s.set.MaxPerSubject)
	if err != nil {
		return nil, err
	}
	if dup {
		log.Printf("custom: duplicate payment %s (user %d) ignored", rec.ChargeID, rec.UserID)
		return &PaymentOutcome{Kind: OutcomeCustomPending, Duplicate: true}, nil
	}
	metrics.Inc(metrics.Payments, "kind", billing.KindCustomTest, "decision", decisionLabel(fmt.Sprint(accepted), rec.AdminBypass))
	log.Printf("custom: payment %s: user %d (tg %d) order %d %d⭐ accepted=%t admin_bypass=%t",
		rec.ChargeID, rec.UserID, tgUserID, rec.OrderID, rec.Amount, accepted, rec.AdminBypass)
	if !accepted {
		if s.billing != nil {
			s.billing.Wake() // refund
		}
		return &PaymentOutcome{Kind: OutcomeRejected}, nil
	}
	return s.startPaid(ctx, rec.OrderID), nil
}

// ConfirmFree starts a FREE order (price 0 / subscriptions off).
func (s *CustomTestService) ConfirmFree(ctx context.Context, userID, orderID int64) (*PaymentOutcome, error) {
	if s.Price() > 0 {
		return nil, fmt.Errorf("custom order %d is not free", orderID)
	}
	ok, err := s.repo.ConfirmFree(ctx, orderID, userID, s.set.MaxPerSubject)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &PaymentOutcome{Kind: OutcomeRejected}, nil
	}
	log.Printf("custom: free order %d of user %d confirmed", orderID, userID)
	return s.startPaid(ctx, orderID), nil
}

// startPaid advances a just-paid order at once (queues its generation).
func (s *CustomTestService) startPaid(ctx context.Context, orderID int64) *PaymentOutcome {
	o, err := s.repo.ClaimOrder(ctx, orderID, orderLease)
	if err != nil || o == nil {
		if err != nil {
			log.Printf("custom: claim order %d: %v", orderID, err)
		}
		s.Wake()
		return &PaymentOutcome{Kind: OutcomeCustomPending}
	}
	kind, test := s.processOrder(ctx, o, false)
	return &PaymentOutcome{Kind: kind, Test: test}
}

// processOrder advances one claimed paid order: queues its generation job,
// fulfils it when the test exists, refunds it when the job failed for good
// or the order timed out. notify = the user is told here (reconciler).
func (s *CustomTestService) processOrder(ctx context.Context, o *repositories.CustomOrder, notify bool) (string, *models.Test) {
	retry := func(next time.Duration, msg string) (string, *models.Test) {
		if err := s.repo.NoteRetry(ctx, o.ID, next, msg); err != nil {
			log.Printf("custom: order %d retry: %v", o.ID, err)
		}
		return OutcomeCustomPending, nil
	}
	refund := func(reason string) (string, *models.Test) {
		changed, err := s.repo.RefundOrder(ctx, o.ID, reason)
		if err != nil {
			log.Printf("custom: refund order %d: %v", o.ID, err)
			return OutcomeCustomPending, nil
		}
		if changed {
			metrics.Inc(metrics.Payments, "kind", billing.KindCustomTest, "decision", "refund")
			log.Printf("custom: order %d (user %d subject %d) refunded: %s", o.ID, o.UserID, o.SubjectID, reason)
			if s.billing != nil {
				s.billing.Wake()
			}
			// Free / admin orders have no Stars refund to announce them.
			if notify && s.notify != nil && (o.ChargeID == "" || IsAdminBypassCharge(o.ChargeID)) {
				s.notify.CustomTestFailed(ctx, o.TelegramUserID, *o)
			}
		}
		return OutcomeCustomRefunded, nil
	}
	if t, err := s.repo.CustomTestByOrder(ctx, o.ID); err != nil {
		return retry(30*time.Second, err.Error())
	} else if t != nil {
		return s.fulfil(ctx, o, t, notify)
	}
	if o.JobID == 0 {
		jobID, err := s.gen.EnqueueCustomJob(ctx, o.SubjectID, o.UserID, o.ID)
		if err != nil {
			return retry(30*time.Second, "enqueue: "+err.Error())
		}
		if err := s.repo.SetOrderJob(ctx, o.ID, jobID); err != nil {
			return retry(30*time.Second, "link job: "+err.Error())
		}
		o.JobID = jobID
		log.Printf("custom: order %d → generation job %d queued (urgent)", o.ID, jobID)
		if s.ai != nil {
			s.ai.Wake()
		}
		return retry(45*time.Second, "")
	}
	testID, pending, err := s.gen.JobState(ctx, o.JobID)
	if err != nil {
		return retry(30*time.Second, err.Error())
	}
	if testID > 0 {
		if t, err := s.repo.CustomTestByOrder(ctx, o.ID); err == nil && t != nil {
			return s.fulfil(ctx, o, t, notify)
		}
	}
	if !pending {
		return refund("generation failed")
	}
	if o.PaidAt != nil && time.Since(*o.PaidAt) > s.set.OrderTimeout {
		return refund("timeout")
	}
	return retry(45*time.Second, "")
}

func (s *CustomTestService) fulfil(ctx context.Context, o *repositories.CustomOrder, t *models.Test, notify bool) (string, *models.Test) {
	changed, err := s.repo.FulfillOrder(ctx, o.ID, t.ID)
	if err != nil {
		log.Printf("custom: fulfil order %d: %v", o.ID, err)
		return OutcomeCustomPending, nil
	}
	if changed {
		log.Printf("custom: order %d fulfilled with test %d (user %d)", o.ID, t.ID, o.UserID)
		if notify && s.notify != nil {
			s.notify.CustomTestReady(ctx, o.TelegramUserID, t)
		}
	}
	return OutcomeCustomReady, t
}

// --- Reconciler ---------------------------------------------------------------------

// Wake makes the reconciler run at once (non-blocking).
func (s *CustomTestService) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// OnGenerationFinished: a custom job finished — its order is checked now.
func (s *CustomTestService) OnGenerationFinished(ctx context.Context, job *models.GenerationJob) {
	if job == nil || job.Kind != models.TestKindCustom || job.CustomOrderID <= 0 {
		return
	}
	if _, err := s.repo.KickOrder(ctx, job.CustomOrderID); err != nil {
		log.Printf("custom: kick order %d: %v", job.CustomOrderID, err)
		return
	}
	s.Wake()
}

// ReconcileOnce advances due paid orders (safe on any number of workers).
func (s *CustomTestService) ReconcileOnce(ctx context.Context) (full bool) {
	orders, err := s.repo.ClaimDueOrders(ctx, reconcileBatch, orderLease)
	if err != nil {
		log.Printf("custom: claim due orders: %v", err)
	}
	for i := range orders {
		s.processOrder(ctx, &orders[i], true)
	}
	return len(orders) == reconcileBatch
}

// Run is the reconciler loop of a worker.
func (s *CustomTestService) Run(ctx context.Context) {
	t := time.NewTicker(s.set.ReconcileInterval)
	defer t.Stop()
	for {
		for s.ReconcileOnce(ctx) && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// --- Finish -----------------------------------------------------------------------------

// FinishTest deletes the user's custom test («🏁 Закончить тест») and
// returns its subject. Only the owner can finish it.
func (s *CustomTestService) FinishTest(ctx context.Context, userID, testID int64) (int64, error) {
	sid, err := s.repo.DeleteCustomTest(ctx, userID, testID)
	if err == nil {
		log.Printf("custom: test %d of user %d finished and deleted", testID, userID)
	}
	return sid, err
}

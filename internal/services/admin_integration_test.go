package services

// Integration tests of the admin panel services (skipped without
// TEST_DATABASE_URL): admin-bypass purchases, plan changes with the audit
// log, statistics / search / card, and the durable broadcast sender
// (429 / 403 / restart / cancel / one sender at a time).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

func TestAdminBypassPurchaseIntegration(t *testing.T) {
	e := newBillEnv(t)
	adminTG := e.tgID
	e.svc.WithAdmins(func(id int64) bool { return id == adminTG })
	countPayments := func() int {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT COUNT(*) FROM payments WHERE user_id = $1`, e.user.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A non-admin can never bypass the payment (checked in the service).
	if _, err := e.svc.AdminBypassPurchase(e.ctx, e.user.ID, adminTG+1, billing.SubscriptionPayload("plus")); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin bypass: %v", err)
	}
	if countPayments() != 0 {
		t.Fatal("a refused bypass recorded a payment")
	}

	// Admin buys Plus: applied at once, flagged, no Telegram subscription.
	out, err := e.svc.AdminBypassPurchase(e.ctx, e.user.ID, adminTG, billing.SubscriptionPayload("plus"))
	if err != nil || out.Kind != OutcomeSubscription || out.Plan.Code != "plus" {
		t.Fatalf("bypass plus: %v %+v", err, out)
	}
	u, err := e.svc.Usage(e.ctx, e.user.ID)
	if err != nil || u.Plan.Code != "plus" || u.Stored.Source != billing.SourceAdmin || u.Stored.SubChargeID != "" {
		t.Fatalf("usage after bypass: %v %+v %+v", err, u, u.Stored)
	}
	if d := time.Until(u.ExpiresAt); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("bypass plan must last 30 days, got %s", d)
	}
	var charge string
	var bypass bool
	var amount int
	if err := e.pool.QueryRow(e.ctx, `SELECT charge_id, admin_bypass, amount FROM payments WHERE user_id = $1`, e.user.ID).
		Scan(&charge, &bypass, &amount); err != nil {
		t.Fatal(err)
	}
	if !bypass || !IsAdminBypassCharge(charge) || amount != 10 {
		t.Fatalf("payment row: %s bypass=%t amount=%d", charge, bypass, amount)
	}
	// The usual purchase rules still apply: the same plan again is refused.
	var refused *BypassRefusedError
	if _, err := e.svc.AdminBypassPurchase(e.ctx, e.user.ID, adminTG, billing.SubscriptionPayload("plus")); !errors.As(err, &refused) {
		t.Fatalf("same plan again: %v", err)
	}
	// Upgrade: no Telegram call to cancel anything (there is no real subscription).
	if out, err := e.svc.AdminBypassPurchase(e.ctx, e.user.ID, adminTG, billing.SubscriptionPayload("premium")); err != nil || out.Decision != billing.DecisionUpgrade {
		t.Fatalf("upgrade: %v %+v", err, out)
	}
	// A refund of a bypass payment never reaches Telegram.
	ok, err := e.svc.AdminRefund(e.ctx, charge)
	if err != nil || !ok {
		t.Fatalf("admin refund: %v %t", err, ok)
	}
	var status string
	_ = e.pool.QueryRow(e.ctx, `SELECT status FROM payments WHERE charge_id = $1`, charge).Scan(&status)
	if status != "refunded" {
		t.Fatalf("bypass refund status %q", status)
	}
	e.dueNow()
	for i := 0; i < 20 && e.svc.ReconcileOnce(e.ctx); i++ {
	}
	e.svc.ReconcileOnce(e.ctx)

	// Paid weak-topics test: the order is paid at once; it can not be built
	// here (no generator result) → «refunded» without any Telegram refund.
	e.build.topics = []string{"Тема"}
	o, err := e.svc.OpenWeakOrder(e.ctx, e.user.ID, adminTG, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	out, err = e.svc.AdminBypassPurchase(e.ctx, e.user.ID, adminTG, billing.WeakTestPayload(o.ID))
	if err != nil || out.Kind != OutcomeWeakRefunded {
		t.Fatalf("weak bypass: %v %+v", err, out)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT status FROM payments WHERE order_id = $1`, o.ID).Scan(&status)
	if status != "refunded" {
		t.Fatalf("weak bypass payment status %q", status)
	}
	e.dueNow()
	e.svc.ReconcileOnce(e.ctx)
	e.stars.mu.Lock()
	defer e.stars.mu.Unlock()
	for _, r := range e.stars.refunds {
		if IsAdminBypassCharge(r) {
			t.Fatalf("refundStarPayment called for a bypass payment %s", r)
		}
	}
	for _, c := range e.stars.cancels {
		if IsAdminBypassCharge(c) {
			t.Fatalf("editUserStarSubscription called for a bypass payment %s", c)
		}
	}
	// A bypass weak order with a built test is fulfilled like a real one.
	e.build.test = &models.Test{ID: 777, Kind: models.TestKindPersonal}
	o2, err := e.svc.OpenWeakOrder(e.ctx, e.user.ID, adminTG, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := e.svc.AdminBypassPurchase(e.ctx, e.user.ID, adminTG, billing.WeakTestPayload(o2.ID)); err != nil || out.Kind != OutcomeWeakReady {
		t.Fatalf("weak bypass ready: %v %+v", err, out)
	}
}

func newAdminEnv(t *testing.T) (*billEnv, *AdminService, *repositories.AdminRepository) {
	e := newBillEnv(t)
	repo := repositories.NewAdminRepository(e.pool)
	admin := e.tgID + 500
	svc := NewAdminService(repo, e.svc, func(id int64) bool { return id == admin }, billing.LoadLocation("Asia/Almaty"))
	return e, svc, repo
}

func TestAdminSetPlanStatsSearchCard(t *testing.T) {
	e, svc, repo := newAdminEnv(t)
	adminTG := e.tgID + 500
	uname := fmt.Sprintf("adm_%d", e.tgID)
	e.exec(`UPDATE users SET username = $2, first_name = 'Жансая', last_name = 'Тестова' WHERE id = $1`, e.user.ID, uname)

	if _, err := svc.SetPlan(e.ctx, e.tgID, e.user.ID, "pro", 30); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin set plan: %v", err)
	}
	if _, err := svc.SetPlan(e.ctx, adminTG, e.user.ID, "gold", 30); !errors.Is(err, ErrUnknownPlan) {
		t.Fatalf("unknown plan: %v", err)
	}
	until, err := svc.SetPlan(e.ctx, adminTG, e.user.ID, "pro", 30)
	if err != nil || time.Until(until) < 29*24*time.Hour {
		t.Fatalf("set pro: %v %s", err, until)
	}
	u, _ := e.svc.Usage(e.ctx, e.user.ID)
	if u.Plan.Code != "pro" || u.Stored.Source != billing.SourceAdmin {
		t.Fatalf("plan %+v", u)
	}
	if _, err := svc.SetPlan(e.ctx, adminTG, e.user.ID, "free", 0); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.svc.Usage(e.ctx, e.user.ID); !u.Plan.IsFree() {
		t.Fatalf("free: %+v", u.Plan)
	}
	acts, err := repo.RecentActions(e.ctx, e.user.ID, 10)
	if err != nil || len(acts) != 2 || acts[0].Action != ActionSetPlan || acts[0].AdminTgID != adminTG ||
		!strings.Contains(acts[0].Details, "plan=free") || !strings.Contains(acts[0].Details, "было: pro") ||
		!strings.Contains(acts[1].Details, "plan=pro days=30") {
		t.Fatalf("audit log: %v %+v", err, acts)
	}

	// Search: @username (any case), Telegram id, internal id, part of the name.
	for _, q := range []string{"@" + strings.ToUpper(uname), fmt.Sprint(e.tgID), fmt.Sprint(e.user.ID), "https://t.me/" + uname} {
		found, err := svc.Search(e.ctx, q)
		if err != nil || len(found) == 0 || found[0].ID != e.user.ID {
			t.Fatalf("search %q: %v %+v", q, err, found)
		}
	}
	found, err := svc.Search(e.ctx, "жансая тест")
	if err != nil || len(found) == 0 {
		t.Fatalf("name search: %v %+v", err, found)
	}
	if found, _ := svc.Search(e.ctx, "%"); len(found) > 0 {
		for _, f := range found {
			if !strings.Contains(f.FirstName+" "+f.LastName+f.Username, "%") {
				t.Fatalf("%% must be literal, matched %+v", f)
			}
		}
	}

	// Card: activity, plan, weak topics, audit log.
	e.exec(`INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent)
		VALUES ($1, $2, 'дроби', 'Дроби', 0, 5, '00000')`, e.user.ID, e.sid)
	card, err := svc.Card(e.ctx, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if card.User.Username != uname || card.Usage == nil || !card.Usage.Plan.IsFree() || len(card.Actions) == 0 {
		t.Fatalf("card: %+v", card)
	}
	if len(card.WeakTopics) != 1 || card.WeakTopics[0].Topics[0].Topic != "Дроби" {
		t.Fatalf("weak topics: %+v", card.WeakTopics)
	}

	// Statistics: sane numbers (the shared test database holds other data).
	st, err := svc.Stats(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.UsersTotal < 1 || st.NewToday < 1 || st.NewToday > st.New7d || st.New7d > st.New30d || st.New30d > st.UsersTotal ||
		st.ActiveToday > st.Active7d || st.CompletedToday > st.CompletedTotal {
		t.Fatalf("stats: %+v", st)
	}
}

// --- Broadcasts ------------------------------------------------------------------------

type fakeBroadcastAPI struct {
	mu        sync.Mutex
	delivered map[int64]int // successful deliveries per chat
	calls     map[int64]int // all calls per chat
	fail      map[int64]error
	limitOnce map[int64]bool // 429 on the first call
	after     func(n int)    // called after every successful delivery
	ok        int
}

func newFakeBroadcastAPI() *fakeBroadcastAPI {
	return &fakeBroadcastAPI{delivered: map[int64]int{}, calls: map[int64]int{}, fail: map[int64]error{}, limitOnce: map[int64]bool{}}
}

func (f *fakeBroadcastAPI) SendBroadcast(_ context.Context, chatID int64, m bot.BroadcastMessage) (int64, error) {
	f.mu.Lock()
	f.calls[chatID]++
	if err := f.fail[chatID]; err != nil {
		f.mu.Unlock()
		return 0, err
	}
	if f.limitOnce[chatID] {
		f.limitOnce[chatID] = false
		f.mu.Unlock()
		return 0, &bot.RateLimitError{Method: "sendMessage", RetryAfter: time.Second}
	}
	f.delivered[chatID]++
	f.ok++
	n := f.ok
	after := f.after
	f.mu.Unlock()
	if after != nil {
		after(n)
	}
	return 1, nil
}

type fakeBcNotify struct {
	mu   sync.Mutex
	done []*repositories.Broadcast
}

func (f *fakeBcNotify) BroadcastFinished(_ context.Context, b *repositories.Broadcast) {
	f.mu.Lock()
	f.done = append(f.done, b)
	f.mu.Unlock()
}

// quiesceBroadcasts stops broadcasts left by other tests (shared database:
// only one broadcast is sent at a time, a leftover would be sent first).
func quiesceBroadcasts(e *billEnv) {
	e.exec(`UPDATE broadcasts SET status = 'canceled', finished_at = now() WHERE status IN ('queued','sending')`)
}

func makeUsers(t *testing.T, e *billEnv, n int) []*models.User {
	users := repositories.NewUserRepository(e.pool)
	base := time.Now().UnixNano()%1_000_000_000 + 7_000_000_000
	var out []*models.User
	for i := 0; i < n; i++ {
		u, err := users.Upsert(e.ctx, &models.User{TelegramID: base + int64(i)*7, FirstName: fmt.Sprintf("BC%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func confirmBroadcast(t *testing.T, e *billEnv, svc *AdminService, text string) *repositories.Broadcast {
	t.Helper()
	adminTG := e.tgID + 500
	d, err := svc.CreateBroadcastDraft(e.ctx, adminTG, &repositories.Broadcast{Text: text,
		Buttons: [][]repositories.BroadcastButton{{{Text: "Сайт", URL: "https://example.com"}}}})
	if err != nil {
		t.Fatal(err)
	}
	changed, total, err := svc.ConfirmBroadcast(e.ctx, adminTG, d.ID)
	if err != nil || !changed || total < 1 {
		t.Fatalf("confirm: %v %t %d", err, changed, total)
	}
	// A double tap / retried update does not queue it twice.
	if changed, _, err := svc.ConfirmBroadcast(e.ctx, adminTG, d.ID); err != nil || changed {
		t.Fatalf("second confirm: %v %t", err, changed)
	}
	b, err := svc.Repo().BroadcastByID(e.ctx, d.ID)
	if err != nil || b.Status != repositories.BroadcastQueued || b.Total != total {
		t.Fatalf("queued: %v %+v", err, b)
	}
	return b
}

func runUntilDone(t *testing.T, e *billEnv, s *BroadcastService, repo *repositories.AdminRepository, id int64) *repositories.Broadcast {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.RunOnce(e.ctx); err != nil {
			t.Fatal(err)
		}
		b, err := repo.BroadcastByID(e.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status == repositories.BroadcastDone || b.Status == repositories.BroadcastCanceled {
			return b
		}
	}
	t.Fatal("broadcast did not finish")
	return nil
}

func TestBroadcastDeliveryIntegration(t *testing.T) {
	e, svc, repo := newAdminEnv(t)
	quiesceBroadcasts(e)
	if _, err := svc.CreateBroadcastDraft(e.ctx, e.tgID, &repositories.Broadcast{Text: "x"}); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin draft: %v", err)
	}
	us := makeUsers(t, e, 3)
	okU, blockedU, limitedU := us[0], us[1], us[2]
	api := newFakeBroadcastAPI()
	api.fail[blockedU.TelegramID] = &bot.APIError{Method: "sendMessage", Code: 403, Description: "Forbidden: bot was blocked by the user"}
	api.limitOnce[limitedU.TelegramID] = true
	notify := &fakeBcNotify{}
	sender := NewBroadcastService(repo, api, "w1", BroadcastSettings{RPS: 1000, Lease: 10 * time.Second}).WithNotifier(notify)

	b := confirmBroadcast(t, e, svc, "Привет всем!")
	fin := runUntilDone(t, e, sender, repo, b.ID)
	if fin.Status != repositories.BroadcastDone {
		t.Fatalf("status %s", fin.Status)
	}
	api.mu.Lock()
	for chat, n := range api.delivered {
		if n > 1 {
			t.Errorf("chat %d got the broadcast %d times", chat, n)
		}
	}
	if api.delivered[okU.TelegramID] != 1 || api.delivered[limitedU.TelegramID] != 1 || api.calls[limitedU.TelegramID] != 2 ||
		api.delivered[blockedU.TelegramID] != 0 || api.calls[blockedU.TelegramID] != 1 {
		t.Fatalf("deliveries: ok=%d limited=%d/%d blocked=%d/%d", api.delivered[okU.TelegramID],
			api.delivered[limitedU.TelegramID], api.calls[limitedU.TelegramID], api.delivered[blockedU.TelegramID], api.calls[blockedU.TelegramID])
	}
	api.mu.Unlock()
	c, err := repo.CountRecipients(e.ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fin.Total != c.Total || fin.Sent != c.Sent || fin.Blocked != c.Blocked || fin.Failed != c.Failed || c.Pending+c.Sending != 0 ||
		fin.Blocked < 1 || fin.Sent+fin.Blocked+fin.Failed != fin.Total || fin.FinishedAt == nil {
		t.Fatalf("counters: %+v vs %+v", fin, c)
	}
	var blockedAt *time.Time
	_ = e.pool.QueryRow(e.ctx, `SELECT blocked_at FROM users WHERE id = $1`, blockedU.ID).Scan(&blockedAt)
	if blockedAt == nil {
		t.Fatal("the user who blocked the bot is not marked")
	}
	notify.mu.Lock()
	if len(notify.done) != 1 || notify.done[0].ID != b.ID {
		t.Fatalf("finish report: %+v", notify.done)
	}
	notify.mu.Unlock()
	// The next broadcast skips users who blocked the bot …
	b2 := confirmBroadcast(t, e, svc, "Второе")
	var has bool
	_ = e.pool.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM broadcast_recipients WHERE broadcast_id = $1 AND user_id = $2)`, b2.ID, blockedU.ID).Scan(&has)
	if has {
		t.Fatal("a blocked user is a recipient again")
	}
	if _, err := svc.CancelBroadcast(e.ctx, e.tgID+500, b2.ID); err != nil {
		t.Fatal(err)
	}
	// … until the user writes to the bot again.
	if _, err := repositories.NewUserRepository(e.pool).Upsert(e.ctx, &models.User{TelegramID: blockedU.TelegramID, FirstName: blockedU.FirstName}); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT blocked_at FROM users WHERE id = $1`, blockedU.ID).Scan(&blockedAt)
	if blockedAt != nil {
		t.Fatal("blocked_at not cleared by a new message")
	}
}

// A restart in the middle (graceful: lease released; crash: lease expires
// with one row in flight) never sends a message twice.
func TestBroadcastRestartNoDuplicates(t *testing.T) {
	e, svc, repo := newAdminEnv(t)
	quiesceBroadcasts(e)
	makeUsers(t, e, 5)
	b := confirmBroadcast(t, e, svc, "Рестарт")
	if b.Total < 5 {
		t.Fatalf("total %d", b.Total)
	}
	api := newFakeBroadcastAPI()
	// Worker 1 is shut down after 2 deliveries.
	ctx1, stop := context.WithCancel(e.ctx)
	api.after = func(n int) {
		if n == 2 {
			stop()
		}
	}
	w1 := NewBroadcastService(repo, api, "w1", BroadcastSettings{RPS: 1000, Lease: 10 * time.Second})
	if _, err := w1.RunOnce(ctx1); err != nil {
		t.Fatal(err)
	}
	api.after = nil
	var lockedBy *string
	_ = e.pool.QueryRow(e.ctx, `SELECT locked_by FROM broadcasts WHERE id = $1`, b.ID).Scan(&lockedBy)
	if lockedBy != nil {
		t.Fatalf("graceful shutdown must release the lease, locked by %v", *lockedBy)
	}
	// Simulated crash: one row was in flight, the lease of the dead worker
	// expired.
	var inflight int64
	if err := e.pool.QueryRow(e.ctx, `UPDATE broadcast_recipients SET status = 'sending'
		WHERE (broadcast_id, user_id) = (SELECT broadcast_id, user_id FROM broadcast_recipients
		                                 WHERE broadcast_id = $1 AND status = 'pending' LIMIT 1)
		RETURNING telegram_id`, b.ID).Scan(&inflight); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE broadcasts SET locked_by = 'dead', locked_until = now() - interval '1 second' WHERE id = $1`, b.ID)
	// While another live worker holds the lease nobody else sends.
	e.exec(`UPDATE broadcasts SET locked_until = now() + interval '1 minute' WHERE id = $1`, b.ID)
	if got, err := repo.ClaimBroadcast(e.ctx, "w2", time.Minute); err != nil || got != nil {
		t.Fatalf("claimed a broadcast leased by another worker: %v %+v", err, got)
	}
	e.exec(`UPDATE broadcasts SET locked_until = now() - interval '1 second' WHERE id = $1`, b.ID)
	w2 := NewBroadcastService(repo, api, "w2", BroadcastSettings{RPS: 1000, Lease: 10 * time.Second})
	fin := runUntilDone(t, e, w2, repo, b.ID)
	api.mu.Lock()
	defer api.mu.Unlock()
	for chat, n := range api.delivered {
		if n > 1 {
			t.Errorf("chat %d got the broadcast %d times", chat, n)
		}
	}
	if api.calls[inflight] != 0 {
		t.Fatalf("the in-flight row of the crashed worker was sent again")
	}
	if fin.Failed < 1 || fin.Sent != api.ok || fin.Sent+fin.Failed+fin.Blocked != fin.Total {
		t.Fatalf("counters %+v, delivered %d", fin, api.ok)
	}
}

func TestBroadcastCancelIntegration(t *testing.T) {
	e, svc, repo := newAdminEnv(t)
	quiesceBroadcasts(e)
	makeUsers(t, e, 2)
	b := confirmBroadcast(t, e, svc, "Отмена")
	if _, err := svc.CancelBroadcast(e.ctx, e.tgID, b.ID); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("non-admin cancel: %v", err)
	}
	changed, err := svc.CancelBroadcast(e.ctx, e.tgID+500, b.ID)
	if err != nil || !changed {
		t.Fatalf("cancel: %v %t", err, changed)
	}
	if changed, _ := svc.CancelBroadcast(e.ctx, e.tgID+500, b.ID); changed {
		t.Fatal("cancelled twice")
	}
	api := newFakeBroadcastAPI()
	w := NewBroadcastService(repo, api, "w1", BroadcastSettings{RPS: 1000})
	if worked, err := w.RunOnce(e.ctx); err != nil || worked {
		t.Fatalf("a cancelled broadcast was sent: %v %t", err, worked)
	}
	c, _ := repo.CountRecipients(e.ctx, b.ID)
	if c.Pending != 0 || c.Canceled != c.Total || api.ok != 0 {
		t.Fatalf("counts %+v sent %d", c, api.ok)
	}
	acts, _ := repo.RecentActions(e.ctx, 0, 20)
	seen := 0
	for _, a := range acts {
		if strings.Contains(a.Details, fmt.Sprintf("broadcast=%d", b.ID)) {
			seen++
		}
	}
	if seen != 2 { // confirm + cancel
		t.Fatalf("audit log entries for the broadcast: %d", seen)
	}
}

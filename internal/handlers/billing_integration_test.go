package handlers

// Scenario test of the Telegram Stars subscriptions through the real
// handler, a real PostgreSQL (TEST_DATABASE_URL, skipped otherwise) and a
// fake Bot API: the quota toast, the charge on the final answer, the
// «limit used up» screen, buying a plan (invoice link → pre_checkout →
// successful_payment, re-delivered) and the admin /grant.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

type tgCall struct {
	method string
	p      map[string]any
}

type payTG struct {
	mu    sync.Mutex
	calls []tgCall
}

func (f *payTG) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		m := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		f.calls = append(f.calls, tgCall{m, p})
		f.mu.Unlock()
		switch m {
		case "createInvoiceLink":
			_, _ = io.WriteString(w, `{"ok":true,"result":"https://t.me/$fakeinvoice"}`)
		case "answerCallbackQuery", "answerPreCheckoutQuery", "deleteMessage", "deleteMessages", "refundStarPayment", "editUserStarSubscription":
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *payTG) since(n int) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tgCall(nil), f.calls[n:]...)
}

func (f *payTG) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func findCall(calls []tgCall, method, textPart string) *tgCall {
	for i := range calls {
		if calls[i].method != method {
			continue
		}
		if textPart == "" {
			return &calls[i]
		}
		for _, k := range []string{"text", "error_message"} {
			if s, ok := calls[i].p[k].(string); ok && strings.Contains(s, textPart) {
				return &calls[i]
			}
		}
	}
	return nil
}

func dumpCalls(calls []tgCall) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "%s %v\n", c.method, c.p["text"])
	}
	return b.String()
}

func TestSubscriptionFlowThroughHandler(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &payTG{}
	srv := f.server(t)

	subjects := repositories.NewSubjectRepository(pool)
	users := repositories.NewUserRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	bill := repositories.NewBillingRepository(pool, billing.MustCatalog(billing.DefaultPlans()), billing.LoadLocation("Asia/Almaty"), time.Hour)
	attempts := repositories.NewAttemptRepository(pool).WithQuota(bill)
	genSvc := services.NewGeneratorService(nil, &config.Config{}, gen, subjects, state)
	quiz := services.NewQuizService(subjects, attempts, state, gen, genSvc, users)
	tg := bot.NewClient("T").WithBaseURL(srv.URL)
	billSvc := services.NewBillingService(bill, gen, genSvc, services.BillingSettings{WeakTestPrice: 10}).WithStars(tg)
	tgID := time.Now().UnixNano()%1_000_000_000 + 9_000_000_000
	adminID := tgID + 1
	h := New(tg, users, quiz).WithBilling(billSvc).WithAdmins(func(id int64) bool { return id == adminID })
	billSvc.WithNotifier(h)

	sid, err := testutil.CreateSubject(ctx, pool, "Подписка "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain},
		[]models.SeedQuestion{{Text: "Сколько будет 2+2? " + itoa(sid), Options: [4]string{"4", "3", "5", "6"}, Correct: 0, Topic: "Арифметика", Difficulty: 1}})
	if err != nil {
		t.Fatal(err)
	}
	from := &bot.TgUser{ID: tgID, FirstName: "Аян"}
	chat := bot.Chat{ID: tgID, Type: "private"}
	tap := func(data string) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: "cb" + data, From: from, Data: data,
			Message: &bot.Message{MessageID: 1, Chat: chat}}})
		return f.since(n)
	}
	say := func(text string, who *bot.TgUser) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{Message: &bot.Message{MessageID: 2, From: who, Chat: bot.Chat{ID: who.ID, Type: "private"}, Text: text}})
		return f.since(n)
	}
	answerAll := func() []tgCall {
		var attemptID int64
		if err := pool.QueryRow(ctx, `SELECT a.id FROM test_attempts a JOIN users u ON u.id = a.user_id
			WHERE u.telegram_id = $1 AND a.status = 'in_progress'`, tgID).Scan(&attemptID); err != nil {
			t.Fatalf("attempt: %v", err)
		}
		return tap(fmt.Sprintf("%s%d:1:0", cbAnswer, attemptID))
	}

	// 1. Opening the test shows the remaining completions.
	calls := tap(cbOpenTest + itoa(test.ID))
	if c := findCall(calls, "answerCallbackQuery", ""); c == nil || !strings.Contains(fmt.Sprint(c.p["text"]), "Осталось прохождений сегодня: 1/1") {
		t.Fatalf("quota toast missing:\n%s", dumpCalls(calls))
	}
	// 2. The final answer charges one completion.
	calls = answerAll()
	if findCall(calls, "editMessageText", "списано 1 прохождение. Осталось сегодня: 0/1") == nil {
		t.Fatalf("charge line missing:\n%s", dumpCalls(calls))
	}
	var attemptID int64
	_ = pool.QueryRow(ctx, `SELECT a.id FROM test_attempts a JOIN users u ON u.id = a.user_id WHERE u.telegram_id = $1 ORDER BY a.id DESC LIMIT 1`, tgID).Scan(&attemptID)
	// 3. Retrying the passed test: the limit is used up.
	calls = tap(cbRetry + itoa(attemptID))
	c := findCall(calls, "sendMessage", "На сегодня прохождения закончились")
	if c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbPlans) {
		t.Fatalf("quota exceeded screen missing:\n%s", dumpCalls(calls))
	}
	// 4. The plans screen.
	calls = say(kbPlans, from)
	if c := findCall(calls, "sendMessage", "Твой план: Free"); c == nil || !strings.Contains(fmt.Sprint(c.p["text"]), "Plus — 10⭐/мес, 4 прохождения в день") {
		t.Fatalf("plans screen:\n%s", dumpCalls(calls))
	}
	// 5. Buying Plus: a subscription invoice link (XTR, 30 days).
	calls = tap(cbBuyPlan + "plus")
	link := findCall(calls, "createInvoiceLink", "")
	if link == nil || link.p["currency"] != "XTR" || link.p["subscription_period"] != float64(2592000) || link.p["payload"] != "sub:plus" {
		t.Fatalf("invoice link:\n%s %+v", dumpCalls(calls), link)
	}
	if findCall(calls, "sendMessage", "План Plus — 10⭐ в месяц") == nil {
		t.Fatalf("buy message:\n%s", dumpCalls(calls))
	}
	// 6. pre_checkout_query is answered OK; a wrong amount is refused.
	pre := func(amount int) bool {
		n := f.n()
		if err := h.ProcessUpdate(ctx, &bot.Update{UpdateID: 1, PreCheckoutQuery: &bot.PreCheckoutQuery{ID: "pq", From: from,
			Currency: "XTR", TotalAmount: amount, InvoicePayload: "sub:plus"}}); err != nil {
			t.Fatal(err)
		}
		c := findCall(f.since(n), "answerPreCheckoutQuery", "")
		return c != nil && c.p["ok"] == true
	}
	if !pre(10) || pre(1) {
		t.Fatal("pre-checkout answers")
	}
	// The fast path of the webhook (AnswerPreCheckout, outside the update
	// queues) validates exactly like the queued path.
	direct := func(amount int, payload string) bool {
		n := f.n()
		h.AnswerPreCheckout(ctx, &bot.PreCheckoutQuery{ID: "pd", From: from, Currency: "XTR", TotalAmount: amount, InvoicePayload: payload})
		c := findCall(f.since(n), "answerPreCheckoutQuery", "")
		return c != nil && c.p["ok"] == true
	}
	if !direct(10, "sub:plus") || direct(1, "sub:plus") || direct(10, "bogus") {
		t.Fatal("pre-checkout fast path answers")
	}
	// 7. successful_payment activates Plus; a re-delivery changes nothing.
	pay := &bot.Update{UpdateID: 2, Message: &bot.Message{MessageID: 3, From: from, Chat: chat, SuccessfulPayment: &bot.SuccessfulPayment{
		Currency: "XTR", TotalAmount: 10, InvoicePayload: "sub:plus", TelegramPaymentChargeID: fmt.Sprintf("hc%d", tgID),
		SubscriptionExpirationDate: time.Now().Add(billing.SubscriptionPeriod).Unix(), IsFirstRecurring: true, IsRecurring: true}}}
	n := f.n()
	if err := h.ProcessUpdate(ctx, pay); err != nil {
		t.Fatal(err)
	}
	if findCall(f.since(n), "sendMessage", "План Plus активен") == nil {
		t.Fatalf("payment message:\n%s", dumpCalls(f.since(n)))
	}
	n = f.n()
	if err := h.ProcessUpdate(ctx, pay); err != nil {
		t.Fatal(err)
	}
	if len(f.since(n)) != 0 {
		t.Fatalf("duplicate payment produced messages:\n%s", dumpCalls(f.since(n)))
	}
	// 8. The retry is allowed now (Plus: 4 a day).
	calls = tap(cbRetry + itoa(attemptID))
	if findCall(calls, "sendMessage", "закончились") != nil {
		t.Fatalf("Plus user refused:\n%s", dumpCalls(calls))
	}
	// 9. Admin commands: only for ADMIN_IDS.
	calls = say(fmt.Sprintf("/grant %d premium 7", tgID), from)
	if findCall(calls, "sendMessage", "выдан план") != nil {
		t.Fatal("a non-admin must not grant plans")
	}
	admin := &bot.TgUser{ID: adminID, FirstName: "Admin"}
	calls = say(fmt.Sprintf("/grant %d premium 7", tgID), admin)
	if findCall(calls, "sendMessage", "выдан план premium") == nil {
		t.Fatalf("grant:\n%s", dumpCalls(calls))
	}
	calls = say(fmt.Sprintf("/subinfo %d", tgID), admin)
	if findCall(calls, "sendMessage", "План: Premium") == nil || findCall(calls, "sendMessage", "hc") == nil {
		t.Fatalf("subinfo:\n%s", dumpCalls(calls))
	}
	// 10. Paid weak-topics test: the tap sends a Stars invoice (10⭐) …
	u, err := users.Upsert(ctx, &models.User{TelegramID: tgID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent)
		VALUES ($1, $2, 'арифметика', 'Арифметика', 0, 5, '00000')
		ON CONFLICT (user_id, subject_id, topic_key) DO UPDATE SET correct_count = 0, wrong_count = 5, recent = '00000'`, u.ID, sid); err != nil {
		t.Fatal(err)
	}
	calls = tap(cbWeakSubject + itoa(sid))
	inv := findCall(calls, "sendInvoice", "")
	if inv == nil || inv.p["currency"] != "XTR" || !strings.HasPrefix(fmt.Sprint(inv.p["payload"]), "weak:") ||
		!strings.Contains(fmt.Sprint(inv.p["prices"]), "10") || !strings.Contains(fmt.Sprint(inv.p["reply_markup"]), "pay:true") {
		t.Fatalf("weak invoice:\n%s %+v", dumpCalls(calls), inv)
	}
	payload := fmt.Sprint(inv.p["payload"])
	n = f.n()
	if err := h.ProcessUpdate(ctx, &bot.Update{UpdateID: 3, PreCheckoutQuery: &bot.PreCheckoutQuery{ID: "pw", From: from,
		Currency: "XTR", TotalAmount: 10, InvoicePayload: payload}}); err != nil {
		t.Fatal(err)
	}
	if c := findCall(f.since(n), "answerPreCheckoutQuery", ""); c == nil || c.p["ok"] != true {
		t.Fatalf("weak pre-checkout: %+v", c)
	}
	// … the payment builds the test (bank) or, with no AI and no bank
	// questions, refunds it automatically.
	n = f.n()
	if err := h.ProcessUpdate(ctx, &bot.Update{UpdateID: 4, Message: &bot.Message{MessageID: 4, From: from, Chat: chat,
		SuccessfulPayment: &bot.SuccessfulPayment{Currency: "XTR", TotalAmount: 10, InvoicePayload: payload,
			TelegramPaymentChargeID: fmt.Sprintf("hw%d", tgID)}}}); err != nil {
		t.Fatal(err)
	}
	after := f.since(n)
	switch {
	case findCall(after, "sendMessage", "Тест по слабым темам готов") != nil:
	case findCall(after, "sendMessage", "Не получилось собрать тест") != nil:
		// The shared test database may hold refunds of other tests: run the
		// reconciler until OUR charge is refunded.
		n = f.n()
		refunded := false
		for i := 0; i < 50 && !refunded; i++ {
			billSvc.ReconcileOnce(ctx)
			for _, c := range f.since(n) {
				if c.method == "refundStarPayment" && c.p["telegram_payment_charge_id"] == fmt.Sprintf("hw%d", tgID) {
					refunded = true
				}
			}
		}
		after = f.since(n)
		if !refunded {
			t.Fatalf("refund call:\n%s", dumpCalls(after))
		}
		if findCall(after, "sendMessage", "возвращены") == nil {
			t.Fatalf("refund notice:\n%s", dumpCalls(after))
		}
	default:
		t.Fatalf("weak payment outcome:\n%s", dumpCalls(after))
	}
}

// Without billing a pre_checkout_query is refused (never left unanswered).
func TestPreCheckoutWithoutBilling(t *testing.T) {
	f := &payTG{}
	srv := f.server(t)
	h := New(bot.NewClient("T").WithBaseURL(srv.URL), nil, nil)
	if err := h.ProcessUpdate(context.Background(), &bot.Update{UpdateID: 1, PreCheckoutQuery: &bot.PreCheckoutQuery{ID: "x",
		From: &bot.TgUser{ID: 5}, Currency: "XTR", TotalAmount: 10, InvoicePayload: "sub:plus"}}); err != nil {
		t.Fatal(err)
	}
	c := findCall(f.since(0), "answerPreCheckoutQuery", "")
	if c == nil || c.p["ok"] != false {
		t.Fatalf("answer: %+v", c)
	}
	// The reply keyboard has no «⭐ Подписка» without billing.
	if strings.Contains(fmt.Sprint(mainMenuKeyboardFor(false)), kbPlans) || !strings.Contains(fmt.Sprint(mainMenuKeyboardFor(true)), kbPlans) {
		t.Fatal("menu keyboard")
	}
}

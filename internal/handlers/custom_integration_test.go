package handlers

// Scenario test of «✨ Свой тест» through the real handler, a real
// PostgreSQL (TEST_DATABASE_URL, skipped otherwise), a fake Bot API and a
// fake Groq pre-check: menu → subject → «➕ Новый тест» → description (too
// long, refused, accepted) → invoice → pre_checkout → successful_payment →
// generation → «Твой тест готов» → solve to 15🟢+5🟡 → «🏁 Закончить тест».

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/billing"
	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

func TestCustomTestFlowThroughHandler(t *testing.T) {
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
	var refuse atomic.Bool
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		content := `{"ok":true,"reason":"","title":"Квадратные уравнения"}`
		if refuse.Load() {
			content = `{"ok":false,"reason":"Это игра, а не учебная тема.","title":""}`
		}
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
		_, _ = w.Write(b)
	}))
	defer ai.Close()

	subjects := repositories.NewSubjectRepository(pool)
	users := repositories.NewUserRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	bill := repositories.NewBillingRepository(pool, billing.MustCatalog(billing.DefaultPlans()), billing.LoadLocation("Asia/Almaty"), time.Hour)
	attempts := repositories.NewAttemptRepository(pool).WithQuota(bill)
	genSvc := services.NewGeneratorService(nil, &config.Config{}, gen, subjects, state).WithGroq(groq.New("k", ai.URL))
	quiz := services.NewQuizService(subjects, attempts, state, gen, genSvc, users)
	tg := bot.NewClient("T").WithBaseURL(srv.URL)
	billSvc := services.NewBillingService(bill, gen, genSvc, services.BillingSettings{WeakTestPrice: 10}).WithStars(tg)
	custom := services.NewCustomTestService(repositories.NewCustomTestRepository(pool), gen, subjects, genSvc,
		services.CustomSettings{Enabled: true, Price: 15, MaxPerSubject: 3, OrderTimeout: time.Hour}).WithBilling(billSvc)
	h := New(tg, users, quiz).WithBilling(billSvc).WithCustomTests(custom)
	billSvc.WithNotifier(h)
	custom.WithNotifier(h)

	sid, err := testutil.CreateSubject(ctx, pool, "Математика CUSTOMH "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	tgID := time.Now().UnixNano()%1_000_000_000 + 6_000_000_000
	from := &bot.TgUser{ID: tgID, FirstName: "Аружан"}
	chat := bot.Chat{ID: tgID, Type: "private"}
	tap := func(data string) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: "cb" + data + time.Now().String(), From: from, Data: data,
			Message: &bot.Message{MessageID: 1, Chat: chat}}})
		return f.since(n)
	}
	say := func(text string) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{Message: &bot.Message{MessageID: 2, From: from, Chat: chat, Text: text}})
		return f.since(n)
	}
	sidS := itoa(sid)

	// Main menu carries the button; it opens the subject picker.
	calls := say("/start")
	if c := findCall(calls, "sendMessage", "Добро пожаловать"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), kbCustom) {
		t.Fatalf("main menu without «✨ Свой тест»:\n%s", dumpCalls(calls))
	}
	calls = say(kbCustom)
	if c := findCall(calls, "sendMessage", "Опиши своими словами"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbCustomSubject+sidS) {
		t.Fatalf("subject picker:\n%s", dumpCalls(calls))
	}
	calls = tap(cbCustomSubject + sidS)
	if c := findCall(calls, "editMessageText", "Своих тестов по этому предмету пока нет"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), "Новый тест — 15⭐") {
		t.Fatalf("subject screen:\n%s", dumpCalls(calls))
	}
	calls = tap(cbCustomNew + sidS)
	if findCall(calls, "editMessageText", "Напиши одним сообщением") == nil {
		t.Fatalf("description prompt:\n%s", dumpCalls(calls))
	}
	// Too long → shorten; refused → no invoice; accepted → invoice.
	calls = say(strings.Repeat("а", 501))
	if findCall(calls, "editMessageText", "Слишком длинно: 501") == nil {
		t.Fatalf("too long:\n%s", dumpCalls(calls))
	}
	refuse.Store(true)
	calls = say("Давай поиграем в города")
	if findCall(calls, "editMessageText", "Это игра, а не учебная тема.") == nil || findCall(calls, "sendInvoice", "") != nil {
		t.Fatalf("refusal:\n%s", dumpCalls(calls))
	}
	refuse.Store(false)
	calls = say("Квадратные уравнения, теорема Виета, уровень ЕНТ")
	inv := findCall(calls, "sendInvoice", "")
	if inv == nil || inv.p["currency"] != "XTR" || !strings.HasPrefix(fmt.Sprint(inv.p["payload"]), "cust:") {
		t.Fatalf("invoice:\n%s", dumpCalls(calls))
	}
	payload := fmt.Sprint(inv.p["payload"])
	// After the order the next text is NOT a description any more.
	calls = say("привет")
	if findCall(calls, "sendMessage", "Главное меню") == nil {
		t.Fatalf("the draft must be over:\n%s", dumpCalls(calls))
	}
	// pre_checkout + successful_payment.
	n := f.n()
	if err := h.ProcessUpdate(ctx, &bot.Update{UpdateID: 1, PreCheckoutQuery: &bot.PreCheckoutQuery{ID: "pq", From: from,
		Currency: "XTR", TotalAmount: 15, InvoicePayload: payload}}); err != nil {
		t.Fatal(err)
	}
	if c := findCall(f.since(n), "answerPreCheckoutQuery", ""); c == nil || c.p["ok"] != true {
		t.Fatalf("pre-checkout:\n%s", dumpCalls(f.since(n)))
	}
	n = f.n()
	if err := h.ProcessUpdate(ctx, &bot.Update{UpdateID: 2, Message: &bot.Message{MessageID: 3, From: from, Chat: chat,
		SuccessfulPayment: &bot.SuccessfulPayment{Currency: "XTR", TotalAmount: 15, InvoicePayload: payload,
			TelegramPaymentChargeID: fmt.Sprintf("hcust%d", tgID)}}}); err != nil {
		t.Fatal(err)
	}
	if findCall(f.since(n), "sendMessage", "Генерирую твой тест") == nil {
		t.Fatalf("payment message:\n%s", dumpCalls(f.since(n)))
	}
	// While generating, «➕ Новый тест» is refused (one in flight).
	calls = tap(cbCustomNew + sidS)
	if findCall(calls, "answerCallbackQuery", "ещё генерируется") == nil {
		t.Fatalf("in-flight alert:\n%s", dumpCalls(calls))
	}
	// The worker generates the test (stored directly here) and the
	// reconciler announces it.
	var orderID, jobID, userID int64
	if err := pool.QueryRow(ctx, `SELECT id, job_id, user_id FROM custom_test_orders WHERE telegram_user_id = $1 AND status = 'paid'`, tgID).
		Scan(&orderID, &jobID, &userID); err != nil {
		t.Fatalf("paid order: %v", err)
	}
	seed := make([]models.SeedQuestion, 0, 20)
	for i := 0; i < 20; i++ {
		seed = append(seed, models.SeedQuestion{Text: fmt.Sprintf("Найди сумму корней уравнения №%d (%d)", i+1, tgID),
			Options: [4]string{"1", "2", "3", "4"}, Correct: i % 4, Topic: "Теорема Виета", Difficulty: 2, QualityChecked: true})
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, Title: "✨ Квадратные уравнения", Kind: models.TestKindCustom,
		OwnerUserID: userID, CustomOrderID: orderID}, seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.CompleteJob(ctx, jobID, test.ID); err != nil {
		t.Fatal(err)
	}
	n = f.n()
	custom.OnGenerationFinished(ctx, &models.GenerationJob{ID: jobID, Kind: models.TestKindCustom, CustomOrderID: orderID})
	if _, err := pool.Exec(ctx, `UPDATE custom_test_orders SET next_check_at = now() - interval '1 second' WHERE id = $1`, orderID); err != nil {
		t.Fatal(err)
	}
	custom.ReconcileOnce(ctx)
	if c := findCall(f.since(n), "sendMessage", "Твой тест готов"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbOpenTest+itoa(test.ID)) {
		t.Fatalf("ready message:\n%s", dumpCalls(f.since(n)))
	}
	// Solve it twice, all correct: 20🟢 → «🏁 Закончить тест».
	solve := func() []tgCall {
		var last []tgCall
		calls := tap(cbOpenTest + itoa(test.ID))
		if findCall(calls, "answerCallbackQuery", "Осталось прохождений") != nil {
			t.Fatal("a custom test must not show the daily quota")
		}
		var attemptID int64
		if err := pool.QueryRow(ctx, `SELECT id FROM test_attempts WHERE user_id = $1 AND test_id = $2 AND status = 'in_progress'`, userID, test.ID).Scan(&attemptID); err != nil {
			t.Fatalf("attempt: %v", err)
		}
		for pos := 1; pos <= 20; pos++ {
			var order []byte
			var correct string
			if err := pool.QueryRow(ctx, `SELECT aq.option_order, q.correct_answer FROM attempt_questions aq JOIN questions q ON q.id = aq.question_id
				WHERE aq.attempt_id = $1 AND aq.position = $2`, attemptID, pos).Scan(&order, &correct); err != nil {
				t.Fatal(err)
			}
			idx := strings.Index("ABCD", correct)
			if letters, err := repositories.DecodeOptionOrder(order); err == nil && len(letters) == 4 {
				for i, l := range letters {
					if l == correct {
						idx = i
					}
				}
			}
			last = tap(fmt.Sprintf("%s%d:%d:%d", cbAnswer, attemptID, pos, idx))
		}
		return last
	}
	calls = solve()
	if findCall(calls, "editMessageText", "Доведи тест до 15🟢 + 5🟡") == nil {
		t.Fatalf("first pass result:\n%s", dumpCalls(calls))
	}
	calls = solve()
	res := findCall(calls, "editMessageText", "🏁 Отличный результат")
	if res == nil || !strings.Contains(fmt.Sprint(res.p["reply_markup"]), cbCustomFinish+itoa(test.ID)) || !strings.Contains(fmt.Sprint(res.p["text"]), "✨ Свой тест ·") {
		t.Fatalf("result screen must offer the finish button and show the custom-test screen:\n%s", dumpCalls(calls))
	}
	if strings.Contains(fmt.Sprint(res.p["text"]), "списано") {
		t.Fatal("a custom test must not be charged against the daily quota")
	}
	calls = tap(cbCustomFinish + itoa(test.ID))
	if findCall(calls, "editMessageText", "Тест завершён и удалён") == nil {
		t.Fatalf("finish:\n%s", dumpCalls(calls))
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM tests WHERE id = $1`, test.ID).Scan(&left)
	if left != 0 {
		t.Fatal("the finished custom test must be deleted")
	}
}

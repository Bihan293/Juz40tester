package handlers

// Scenario test of the admin panel through the real handler, a real
// PostgreSQL (TEST_DATABASE_URL, skipped otherwise) and a fake Bot API:
// access control, statistics, user search → card → plan change (with the
// audit log), the correct answer shown to admins only, admin-bypass
// purchases and a broadcast composed step by step and sent by the worker.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

func TestAdminPanelThroughHandler(t *testing.T) {
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
	tg := bot.NewClient("T").WithBaseURL(srv.URL).WithMaxRPS(100000)

	userTG := time.Now().UnixNano()%1_000_000_000 + 6_000_000_000
	adminTG := userTG + 1
	isAdmin := func(id int64) bool { return id == adminTG }
	billSvc := services.NewBillingService(bill, gen, genSvc, services.BillingSettings{WeakTestPrice: 10}).WithStars(tg).WithAdmins(isAdmin)
	adminRepo := repositories.NewAdminRepository(pool)
	adminSvc := services.NewAdminService(adminRepo, billSvc, isAdmin, billing.LoadLocation("Asia/Almaty"))
	sender := services.NewBroadcastService(adminRepo, tg, "handler-test", services.BroadcastSettings{RPS: 100000, Lease: 10 * time.Second})
	h := New(tg, users, quiz).WithBilling(billSvc).WithAdmins(isAdmin).WithAdminPanel(adminSvc, sender)
	billSvc.WithNotifier(h)
	sender.WithNotifier(h)

	uname := fmt.Sprintf("student_%d", userTG)
	userFrom := &bot.TgUser{ID: userTG, FirstName: "Аружан", LastName: "Ученица", Username: uname}
	adminFrom := &bot.TgUser{ID: adminTG, FirstName: "Админ"}
	tapAs := func(from *bot.TgUser, data string) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: fmt.Sprintf("cb%d%s", from.ID, data), From: from, Data: data,
			Message: &bot.Message{MessageID: 1, Chat: bot.Chat{ID: from.ID, Type: "private"}}}})
		return f.since(n)
	}
	sayAs := func(from *bot.TgUser, m bot.Message) []tgCall {
		n := f.n()
		m.From, m.Chat, m.MessageID = from, bot.Chat{ID: from.ID, Type: "private"}, 2
		h.HandleUpdate(ctx, &bot.Update{Message: &m})
		return f.since(n)
	}
	say := func(from *bot.TgUser, text string) []tgCall { return sayAs(from, bot.Message{Text: text}) }
	only := func(calls []tgCall, allowed ...string) bool {
		for _, c := range calls {
			ok := false
			for _, a := range allowed {
				ok = ok || c.method == a
			}
			if !ok {
				return false
			}
		}
		return true
	}

	// 1. Access control: for a user the panel does not exist.
	calls := say(userFrom, "/start")
	if c := findCall(calls, "sendMessage", "Добро пожаловать"); c == nil || strings.Contains(fmt.Sprint(c.p["reply_markup"]), kbAdmin) {
		t.Fatalf("user main menu:\n%s", dumpCalls(calls))
	}
	calls = say(userFrom, "/admin")
	if findCall(calls, "sendMessage", "Админ-панель") != nil || findCall(calls, "sendMessage", "Главное меню") == nil {
		t.Fatalf("/admin of a user must look like an unknown command:\n%s", dumpCalls(calls))
	}
	calls = say(userFrom, kbAdmin)
	if findCall(calls, "sendMessage", "Админ-панель") != nil {
		t.Fatal("a user opened the panel with the button text")
	}
	for _, data := range []string{cbAdmStats, cbAdmUsers, cbAdmBcNew, cbAdmPlan + "1", fmt.Sprintf("%s%d:premium:365", cbAdmPlanApply, 1)} {
		if calls := tapAs(userFrom, data); !only(calls, "answerCallbackQuery") || findCall(calls, "answerCallbackQuery", "") == nil ||
			fmt.Sprint(findCall(calls, "answerCallbackQuery", "").p["text"]) != "<nil>" {
			t.Fatalf("forged %s by a user:\n%s", data, dumpCalls(calls))
		}
	}
	userRow, err := users.Upsert(ctx, &models.User{TelegramID: userTG, FirstName: "Аружан", LastName: "Ученица", Username: uname})
	if err != nil {
		t.Fatal(err)
	}
	if u, _ := billSvc.Usage(ctx, userRow.ID); !u.Plan.IsFree() {
		t.Fatal("a forged admin callback changed a plan")
	}

	// 2. The admin: button in the reply keyboard, /admin, statistics.
	calls = say(adminFrom, "/start")
	if c := findCall(calls, "sendMessage", "Добро пожаловать"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), kbAdmin) {
		t.Fatalf("admin main menu:\n%s", dumpCalls(calls))
	}
	calls = say(adminFrom, kbAdmin)
	if c := findCall(calls, "sendMessage", "Админ-панель"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbAdmStats) {
		t.Fatalf("admin menu:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmStats)
	if c := findCall(calls, "editMessageText", "Статистика бота"); c == nil || !strings.Contains(fmt.Sprint(c.p["text"]), "Подписки") ||
		!strings.Contains(fmt.Sprint(c.p["text"]), "Очереди") {
		t.Fatalf("stats:\n%s", dumpCalls(calls))
	}

	// 3. Search → card → plan change with confirmation and the audit log.
	tapAs(adminFrom, cbAdmUsers)
	calls = say(adminFrom, "@"+uname)
	card := findCall(calls, "sendMessage", "Ник: @"+uname)
	if card == nil || !strings.Contains(fmt.Sprint(card.p["text"]), "Подписка: Free") || !strings.Contains(fmt.Sprint(card.p["reply_markup"]), cbAdmPlan) {
		t.Fatalf("card:\n%s", dumpCalls(calls))
	}
	uid := itoa(userRow.ID)
	calls = tapAs(adminFrom, cbAdmPlan+uid)
	if c := findCall(calls, "editMessageText", "Подписка пользователя"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), "✅ Бесплатный (Free) — текущий") {
		t.Fatalf("plan picker:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmPlanDays+uid+":pro")
	if c := findCall(calls, "editMessageText", "На какой срок"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbAdmPlanAsk+uid+":pro:30") {
		t.Fatalf("terms:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmPlanAsk+uid+":pro:30")
	if findCall(calls, "editMessageText", "Подтверди: выдать Pro на 30 дн.") == nil {
		t.Fatalf("confirmation:\n%s", dumpCalls(calls))
	}
	if u, _ := billSvc.Usage(ctx, userRow.ID); !u.Plan.IsFree() {
		t.Fatal("the plan changed before the confirmation")
	}
	calls = tapAs(adminFrom, cbAdmPlanApply+uid+":pro:30")
	if c := findCall(calls, "answerCallbackQuery", ""); c == nil || !strings.Contains(fmt.Sprint(c.p["text"]), "Выдан Pro") {
		t.Fatalf("apply:\n%s", dumpCalls(calls))
	}
	if u, _ := billSvc.Usage(ctx, userRow.ID); u.Plan.Code != "pro" {
		t.Fatalf("plan after apply: %s", u.Plan.Code)
	}
	if c := findCall(calls, "editMessageText", "Подписка: Pro до"); c == nil {
		t.Fatalf("card after apply:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmPlan+uid)
	if c := findCall(calls, "editMessageText", "Сейчас: Pro"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), "✅ Pro — текущий") {
		t.Fatalf("current plan mark:\n%s", dumpCalls(calls))
	}
	tapAs(adminFrom, cbAdmPlanApply+uid+":free:0")
	if u, _ := billSvc.Usage(ctx, userRow.ID); !u.Plan.IsFree() {
		t.Fatal("free = revoke")
	}
	var logged int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_actions WHERE target_user_id = $1 AND admin_tg_id = $2 AND action = 'set_plan'`, userRow.ID, adminTG).Scan(&logged)
	if logged != 2 {
		t.Fatalf("audit log rows: %d", logged)
	}

	// 4. The correct answer is visible to the admin only; scoring unchanged.
	sid, err := testutil.CreateSubject(ctx, pool, "Админка "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain},
		[]models.SeedQuestion{{Text: "Столица Казахстана? " + itoa(sid), Options: [4]string{"Астана", "Алматы", "Шымкент", "Караганда"}, Correct: 0, Topic: "География", Difficulty: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// The admin buys Plus without paying (bypass): no invoice link at all.
	calls = tapAs(adminFrom, cbBuyPlan+"plus")
	if findCall(calls, "createInvoiceLink", "") != nil || findCall(calls, "sendInvoice", "") != nil {
		t.Fatalf("an invoice for the admin:\n%s", dumpCalls(calls))
	}
	if findCall(calls, "sendMessage", "Режим администратора") == nil || findCall(calls, "sendMessage", "План Plus активен") == nil {
		t.Fatalf("bypass messages:\n%s", dumpCalls(calls))
	}
	adminRow, _ := users.Upsert(ctx, &models.User{TelegramID: adminTG, FirstName: "Админ"})
	if u, _ := billSvc.Usage(ctx, adminRow.ID); u.Plan.Code != "plus" {
		t.Fatalf("admin plan after bypass: %s", u.Plan.Code)
	}
	var bypassRows int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE user_id = $1 AND admin_bypass`, adminRow.ID).Scan(&bypassRows)
	if bypassRows != 1 {
		t.Fatalf("bypass payment rows: %d", bypassRows)
	}
	// A user still gets the invoice.
	calls = tapAs(userFrom, cbBuyPlan+"plus")
	if c := findCall(calls, "sendMessage", "откроется оплата Telegram Stars"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), "Оплатить 10⭐") {
		t.Fatalf("user invoice:\n%s", dumpCalls(calls))
	}
	// The invoice link cache is process-wide: do not leak it into other tests.
	invoiceLinks.Range(func(k, _ any) bool { invoiceLinks.Delete(k); return true })

	question := func(from *bot.TgUser) string {
		calls := tapAs(from, cbOpenTest+itoa(test.ID))
		for _, c := range calls {
			if s, ok := c.p["text"].(string); ok && strings.Contains(s, "Вопрос 1/1") {
				return s
			}
		}
		t.Fatalf("no question:\n%s", dumpCalls(calls))
		return ""
	}
	if q := question(adminFrom); !strings.Contains(q, "Астана ✅") || !strings.Contains(q, "правильный ответ") {
		t.Fatalf("admin question:\n%s", q)
	}
	if q := question(userFrom); strings.Contains(q, "✅") || strings.Contains(q, "правильный ответ") {
		t.Fatalf("user question shows the answer:\n%s", q)
	}
	// The admin's completion is charged like anybody's (documented choice).
	var attemptID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM test_attempts WHERE user_id = $1 AND status = 'in_progress'`, adminRow.ID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	calls = tapAs(adminFrom, fmt.Sprintf("%s%d:1:0", cbAnswer, attemptID))
	if findCall(calls, "editMessageText", "списано 1 прохождение") == nil {
		t.Fatalf("admin completion:\n%s", dumpCalls(calls))
	}

	// 5. Broadcast wizard: text → photo → buttons → preview → send.
	adminRepo.ClearSession(ctx, adminTG) //nolint:errcheck
	_, _ = pool.Exec(ctx, `UPDATE broadcasts SET status = 'canceled', finished_at = now() WHERE status IN ('queued','sending')`)
	tapAs(adminFrom, cbAdmBcNew)
	ent := json.RawMessage(`[{"type":"bold","offset":0,"length":6}]`)
	calls = sayAs(adminFrom, bot.Message{Text: "Привет! Новые тесты уже в боте.", Entities: ent})
	if findCall(calls, "sendMessage", "Шаг 2/3") == nil {
		t.Fatalf("media step:\n%s", dumpCalls(calls))
	}
	calls = sayAs(adminFrom, bot.Message{Photo: []bot.PhotoSize{{FileID: "small"}, {FileID: "BIG"}}})
	if findCall(calls, "sendMessage", "Шаг 3/3") == nil {
		t.Fatalf("buttons step:\n%s", dumpCalls(calls))
	}
	calls = say(adminFrom, "без ссылки")
	if findCall(calls, "sendMessage", "Текст | ссылка") == nil {
		t.Fatalf("bad buttons:\n%s", dumpCalls(calls))
	}
	calls = say(adminFrom, "Открыть бота | https://t.me/juz40bot")
	preview := findCall(calls, "sendPhoto", "")
	if preview == nil || preview.p["photo"] != "BIG" || preview.p["caption"] != "Привет! Новые тесты уже в боте." ||
		!strings.Contains(fmt.Sprint(preview.p["caption_entities"]), "bold") || !strings.Contains(fmt.Sprint(preview.p["reply_markup"]), "https://t.me/juz40bot") ||
		preview.p["chat_id"] != float64(adminTG) {
		t.Fatalf("preview:\n%s %+v", dumpCalls(calls), preview)
	}
	confirm := findCall(calls, "sendMessage", "Отправить эту рассылку?")
	if confirm == nil {
		t.Fatalf("confirmation:\n%s", dumpCalls(calls))
	}
	var bcID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM broadcasts WHERE admin_tg_id = $1 AND status = 'draft' ORDER BY id DESC LIMIT 1`, adminTG).Scan(&bcID); err != nil {
		t.Fatal(err)
	}
	// Nothing is sent before the confirmation; a user can not confirm it.
	if calls := tapAs(userFrom, cbAdmBcSend+itoa(bcID)); !only(calls, "answerCallbackQuery") {
		t.Fatalf("user confirmed a broadcast:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmBcSend+itoa(bcID))
	if findCall(calls, "editMessageText", "запущена") == nil {
		t.Fatalf("start:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmBcSend+itoa(bcID)) // double tap
	if c := findCall(calls, "answerCallbackQuery", ""); c == nil || !strings.Contains(fmt.Sprint(c.p["text"]), "уже запущена") {
		t.Fatalf("double confirmation:\n%s", dumpCalls(calls))
	}
	n := f.n()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := sender.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		st, _ := adminRepo.BroadcastStatus(ctx, bcID)
		if st == repositories.BroadcastDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("broadcast not finished: %s", st)
		}
	}
	sent := f.since(n)
	perChat := map[float64]int{}
	for _, c := range sent {
		if c.method == "sendPhoto" {
			perChat[c.p["chat_id"].(float64)]++
		}
	}
	if perChat[float64(userTG)] != 1 || perChat[float64(adminTG)] != 1 {
		t.Fatalf("deliveries: user %d admin %d", perChat[float64(userTG)], perChat[float64(adminTG)])
	}
	for chat, k := range perChat {
		if k > 1 {
			t.Fatalf("chat %.0f got %d copies", chat, k)
		}
	}
	report := findCall(sent, "sendMessage", "Рассылка завершена")
	if report == nil || report.p["chat_id"] != float64(adminTG) || !strings.Contains(fmt.Sprint(report.p["text"]), "Отправлено:") {
		t.Fatalf("final report:\n%+v", report)
	}
	calls = tapAs(adminFrom, cbAdmBcList)
	if c := findCall(calls, "editMessageText", "Прошлые рассылки"); c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbAdmBcCard+itoa(bcID)) {
		t.Fatalf("history:\n%s", dumpCalls(calls))
	}
	calls = tapAs(adminFrom, cbAdmBcCard+itoa(bcID))
	if findCall(calls, "editMessageText", "— завершена") == nil {
		t.Fatalf("card:\n%s", dumpCalls(calls))
	}
}

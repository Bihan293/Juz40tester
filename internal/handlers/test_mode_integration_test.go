package handlers

// Test mode through the real handler, a real PostgreSQL (TEST_DATABASE_URL,
// skipped otherwise) and a fake Bot API: while a test is active the main
// menu is hidden and nothing else opens — for chain, weak-topics
// (personal), «✨ Свой тест» (custom) and admin tests; finishing / exiting
// brings the menu back; exit charges nothing.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

func TestTestModeHidesMenuAndBlocksOtherTests(t *testing.T) {
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
	f := &countTG{}
	srv := f.server(t)

	subjects := repositories.NewSubjectRepository(pool)
	users := repositories.NewUserRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	attempts := repositories.NewAttemptRepository(pool)
	genSvc := services.NewGeneratorService(nil, &config.Config{}, gen, subjects, state)
	quiz := services.NewQuizService(subjects, attempts, state, gen, genSvc, users)
	h := New(bot.NewClient("T").WithBaseURL(srv.URL), users, quiz)

	stamp := time.Now().Format("150405.000000")
	sidA, err := testutil.CreateSubject(ctx, pool, "Режим теста А "+stamp)
	if err != nil {
		t.Fatal(err)
	}
	sidB, err := testutil.CreateSubject(ctx, pool, "Режим теста Б "+stamp)
	if err != nil {
		t.Fatal(err)
	}
	const total = 2
	seeds := func(tag string) []models.SeedQuestion {
		s := make([]models.SeedQuestion, total)
		for i := range s {
			s[i] = models.SeedQuestion{Text: fmt.Sprintf("%s-%s-%d?", tag, stamp, i), Options: [4]string{"верно", "нет1", "нет2", "нет3"},
				Correct: 0, Topic: "Тема", Difficulty: 1}
		}
		return s
	}
	testA, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sidA, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, seeds("A"))
	if err != nil {
		t.Fatal(err)
	}
	testB, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sidB, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, seeds("B"))
	if err != nil {
		t.Fatal(err)
	}

	tgID := time.Now().UnixNano()%1_000_000_000 + 7_100_000_000
	from := &bot.TgUser{ID: tgID, FirstName: "Дана"}
	var seq atomic.Int64 // the race step taps from two goroutines
	tapAs := func(who *bot.TgUser, data string) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: fmt.Sprintf("tm%d", seq.Add(1)), From: who, Data: data,
			Message: &bot.Message{MessageID: 10, Chat: bot.Chat{ID: who.ID, Type: "private"}}}})
		return f.since(n)
	}
	tap := func(data string) []tgCall { return tapAs(from, data) }
	sayAs := func(who *bot.TgUser, text string) []tgCall {
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{Message: &bot.Message{MessageID: 2, From: who, Chat: bot.Chat{ID: who.ID, Type: "private"}, Text: text}})
		return f.since(n)
	}
	say := func(text string) []tgCall { return sayAs(from, text) }
	attemptOf := func(tg, testID int64) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `SELECT a.id FROM test_attempts a JOIN users u ON u.id = a.user_id
			WHERE u.telegram_id = $1 AND a.test_id = $2 AND a.status = 'in_progress'`, tg, testID).Scan(&id); err != nil {
			t.Fatalf("attempt of test %d: %v", testID, err)
		}
		return id
	}
	activeOf := func(tg int64) int64 {
		var id *int64
		if err := pool.QueryRow(ctx, `SELECT active_attempt_id FROM users WHERE telegram_id = $1`, tg).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id == nil {
			return 0
		}
		return *id
	}
	attemptsOf := func(tg, testID int64) int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM test_attempts a JOIN users u ON u.id = a.user_id
			WHERE u.telegram_id = $1 AND a.test_id = $2`, tg, testID).Scan(&n)
		return n
	}
	// isGuard: «Сначала заверши…» with «▶️ Продолжить тест» (this test) and
	// «🚪 Выйти из теста» (this attempt) — and never the main menu.
	isGuard := func(calls []tgCall, testID, attemptID int64) bool {
		g := findCall(calls, "sendMessage", "Сначала заверши текущий тест или выйди из него")
		if g == nil {
			return false
		}
		kb := fmt.Sprint(g.p["reply_markup"])
		if !strings.Contains(kb, cbOpenTest+itoa(testID)) || !strings.Contains(kb, cbExit+itoa(attemptID)) {
			return false
		}
		return findCall(calls, "sendMessage", "Главное меню") == nil && findCall(calls, "editMessageText", "Вопрос") == nil
	}

	// --- 1. Chain test: open → the menu is replaced by «🚪 Выйти из теста».
	calls := tap(cbOpenTest + itoa(testA.ID))
	if findCall(calls, "editMessageText", "Вопрос 1/2") == nil || !isTestModeKeyboard(findCall(calls, "sendMessage", testModeNotice)) {
		t.Fatalf("open A:\n%s", dumpCalls(calls))
	}
	aidA := attemptOf(tgID, testA.ID)
	if activeOf(tgID) != aidA {
		t.Fatalf("active attempt = %d, want %d", activeOf(tgID), aidA)
	}

	// --- 2. Menu buttons, commands, free text: only the guard.
	for _, text := range []string{"/start", "/menu", kbSubjects, kbWeak, kbCustom, kbProgress, kbTop, kbSettings, kbPlans, "/subjects", "/admin", "привет"} {
		calls := say(text)
		if !isGuard(calls, testA.ID, aidA) {
			t.Fatalf("%q during a test:\n%s", text, dumpCalls(calls))
		}
		// A main-menu button means the phone still shows the menu: hide it again.
		if isMenuButton(text) != isTestModeKeyboard(findCall(calls, "sendMessage", testModeNotice)) {
			t.Fatalf("%q: test-mode keyboard re-sent = %v:\n%s", text, !isMenuButton(text), dumpCalls(calls))
		}
	}
	// --- 3. Inline buttons of older messages: another test, menus, sections.
	for _, data := range []string{cbOpenTest + itoa(testB.ID), cbSubjects, cbSubject + itoa(sidB), cbWeakMenu, cbWeakSubject + itoa(sidA),
		cbCustomMenu, cbMainMenu, cbProgress, cbSettings, cbLeaderboard, cbPlans, cbRetry + itoa(aidA), cbAdmPrefix + "menu"} {
		calls := tap(data)
		if !isGuard(calls, testA.ID, aidA) || findCall(calls, "answerCallbackQuery", "") == nil {
			t.Fatalf("%q during a test:\n%s", data, dumpCalls(calls))
		}
	}
	if attemptsOf(tgID, testB.ID) != 0 {
		t.Fatal("another test was started during the active one")
	}
	// --- 4. The same test reopens (resume) — no second notice.
	calls = tap(cbOpenTest + itoa(testA.ID))
	if findCall(calls, "editMessageText", "Вопрос 1/2") == nil || findCall(calls, "sendMessage", testModeNotice) != nil {
		t.Fatalf("resume A:\n%s", dumpCalls(calls))
	}
	// --- 5. «🚪 Выйти из теста» (reply keyboard) → confirmation → «Нет».
	calls = say(kbExitTest)
	c := findCall(calls, "sendMessage", "Выйти из теста?")
	if c == nil || !strings.Contains(fmt.Sprint(c.p["reply_markup"]), cbExitYes+itoa(aidA)) {
		t.Fatalf("exit button:\n%s", dumpCalls(calls))
	}
	calls = tap(cbExitNo + itoa(aidA))
	if findCall(calls, "editMessageText", "Вопрос 1/2") == nil || activeOf(tgID) != aidA {
		t.Fatalf("cancel exit:\n%s", dumpCalls(calls))
	}
	// Answer 1 of 2 (stays in test mode, still ONE limited call per answer).
	if calls := tap(fmt.Sprintf("%s%d:1:0", cbAnswer, aidA)); len(limited(calls)) != 1 {
		t.Fatalf("answer in test mode:\n%s", dumpCalls(calls))
	}
	// --- 6. «✅ Да, выйти»: the menu comes back, the attempt stays
	// resumable (in progress, nothing completed / charged).
	calls = tap(cbExitYes + itoa(aidA))
	back := findCall(calls, "sendMessage", menuBackNotice)
	if back == nil || !strings.Contains(fmt.Sprint(back.p["reply_markup"]), kbSubjects) || activeOf(tgID) != 0 {
		t.Fatalf("exit:\n%s", dumpCalls(calls))
	}
	var status string
	var completed int
	_ = pool.QueryRow(ctx, `SELECT status FROM test_attempts WHERE id = $1`, aidA).Scan(&status)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM test_attempts a JOIN users u ON u.id = a.user_id
		WHERE u.telegram_id = $1 AND a.status = 'completed'`, tgID).Scan(&completed)
	if status != models.AttemptInProgress || completed != 0 {
		t.Fatalf("exit changed the attempt: status %s, completed %d", status, completed)
	}
	// The menu works again.
	if calls := say("/start"); findCall(calls, "sendMessage", "Привет") == nil {
		t.Fatalf("/start after exit:\n%s", dumpCalls(calls))
	}
	// A leftover «🚪 Выйти из теста» with no test: just the main menu.
	if calls := say(kbExitTest); findCall(calls, "sendMessage", "Главное меню") == nil {
		t.Fatalf("stale exit button:\n%s", dumpCalls(calls))
	}

	// --- 7. Another subject's test opens now; an old question of the
	// paused test A can not be answered meanwhile.
	calls = tap(cbOpenTest + itoa(testB.ID))
	if findCall(calls, "editMessageText", "Вопрос 1/2") == nil || findCall(calls, "sendMessage", testModeNotice) == nil {
		t.Fatalf("open B:\n%s", dumpCalls(calls))
	}
	aidB := attemptOf(tgID, testB.ID)
	calls = tap(fmt.Sprintf("%s%d:2:0", cbAnswer, aidA))
	if !isGuard(calls, testB.ID, aidB) {
		t.Fatalf("answer of the paused test during B:\n%s", dumpCalls(calls))
	}
	var posA int
	_ = pool.QueryRow(ctx, `SELECT current_position FROM test_attempts WHERE id = $1`, aidA).Scan(&posA)
	// --- 8. Finishing B brings the menu back.
	tap(fmt.Sprintf("%s%d:1:0", cbAnswer, aidB))
	calls = tap(fmt.Sprintf("%s%d:2:0", cbAnswer, aidB))
	if findCall(calls, "editMessageText", "Тест завершён") == nil || findCall(calls, "sendMessage", menuBackNotice) == nil || activeOf(tgID) != 0 {
		t.Fatalf("finish B:\n%s", dumpCalls(calls))
	}
	// --- 9. Answering the paused test A's old question re-enters it.
	calls = tap(fmt.Sprintf("%s%d:2:0", cbAnswer, aidA))
	if findCall(calls, "editMessageText", "Тест завершён") == nil || activeOf(tgID) != 0 {
		t.Fatalf("finish paused A by its old message:\n%s", dumpCalls(calls))
	}

	// --- 10. Weak-topics (personal) and «✨ Свой тест» (custom) tests.
	u, err := users.Upsert(ctx, &models.User{TelegramID: tgID, FirstName: "Дана"})
	if err != nil {
		t.Fatal(err)
	}
	personal, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sidA, Title: "Слабые темы", Kind: models.TestKindPersonal,
		OwnerUserID: u.ID, Topics: []string{"Тема"}}, seeds("P"))
	if err != nil {
		t.Fatal(err)
	}
	custom, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sidB, Title: "✨ Дроби", Kind: models.TestKindCustom,
		OwnerUserID: u.ID}, seeds("C"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		test  *models.Test
		label string
	}{{personal, "🎯 Слабые темы"}, {custom, "✨ Свой тест"}} {
		calls := tap(cbOpenTest + itoa(tc.test.ID))
		if findCall(calls, "editMessageText", "Вопрос 1/2") == nil || findCall(calls, "sendMessage", testModeNotice) == nil {
			t.Fatalf("open %s:\n%s", tc.label, dumpCalls(calls))
		}
		aid := attemptOf(tgID, tc.test.ID)
		calls = tap(cbOpenTest + itoa(testA.ID))
		if !isGuard(calls, tc.test.ID, aid) || !strings.Contains(fmt.Sprint(findCall(calls, "sendMessage", "Сначала").p["text"]), tc.label) {
			t.Fatalf("%s guard:\n%s", tc.label, dumpCalls(calls))
		}
		if !isGuard(say(kbWeak), tc.test.ID, aid) {
			t.Fatalf("%s: menu not blocked", tc.label)
		}
		calls = tap(cbExitYes + itoa(aid))
		if findCall(calls, "sendMessage", menuBackNotice) == nil || activeOf(tgID) != 0 {
			t.Fatalf("%s exit:\n%s", tc.label, dumpCalls(calls))
		}
	}

	// --- 11. A stale pointer (the attempt was reaped / restarted
	// elsewhere) is no test mode: the menu works and the pointer is cleared.
	tap(cbOpenTest + itoa(testB.ID))
	aidB2 := attemptOf(tgID, testB.ID)
	if activeOf(tgID) != aidB2 {
		t.Fatal("B not active")
	}
	if _, err := pool.Exec(ctx, `UPDATE test_attempts SET status = 'abandoned' WHERE id = $1`, aidB2); err != nil {
		t.Fatal(err)
	}
	if calls := say("/start"); findCall(calls, "sendMessage", "Привет") == nil || activeOf(tgID) != 0 {
		t.Fatalf("stale active attempt blocks the menu:\n%s", dumpCalls(calls))
	}

	// --- 12. Admin test: the same rules (✅ rendering only); the admin
	// text commands stay available.
	admTG := tgID + 1
	h.isAdmin = func(id int64) bool { return id == admTG }
	adm := &bot.TgUser{ID: admTG, FirstName: "Админ"}
	calls = tapAs(adm, cbOpenTest+itoa(testA.ID))
	if c := findCall(calls, "editMessageText", "Вопрос 1/2"); c == nil || !strings.Contains(fmt.Sprint(c.p["text"]), "✅") ||
		findCall(calls, "sendMessage", testModeNotice) == nil {
		t.Fatalf("admin open:\n%s", dumpCalls(calls))
	}
	admAid := attemptOf(admTG, testA.ID)
	if !isGuard(sayAs(adm, "/admin"), testA.ID, admAid) || !isGuard(tapAs(adm, cbOpenTest+itoa(testB.ID)), testA.ID, admAid) {
		t.Fatal("admin: menu / other test not blocked")
	}
	if !adminTextCommand(commandKey("/grant 1 plus")) {
		t.Fatal("/grant must stay available to admins")
	}
	h.isAdmin = nil

	// --- 13. Two different tests opened at the same moment (double tap on
	// two old buttons, two instances): exactly one test mode wins.
	raceTG := tgID + 2
	racer := &bot.TgUser{ID: raceTG, FirstName: "Гонка"}
	var wg sync.WaitGroup
	for _, id := range []int64{testA.ID, testB.ID} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			tapAs(racer, cbOpenTest+itoa(id))
		}(id)
	}
	wg.Wait()
	var live int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM users u JOIN test_attempts a ON a.id = u.active_attempt_id AND a.status = 'in_progress'
		WHERE u.telegram_id = $1`, raceTG).Scan(&live)
	if live != 1 {
		t.Fatalf("race: %d active tests", live)
	}
}

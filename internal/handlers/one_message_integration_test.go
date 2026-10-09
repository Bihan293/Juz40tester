package handlers

// One rate-limited Telegram call per answer: the answer flow through the
// real handler, a real PostgreSQL (TEST_DATABASE_URL, skipped otherwise)
// and a fake Bot API that counts every call except answerCallbackQuery
// (the only call outside the TG_MAX_RPS limiter).

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

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// countTG is a fake Bot API recording every call; editErr (if set) is the
// Telegram error description returned for editMessageText.
type countTG struct {
	mu      sync.Mutex
	calls   []tgCall
	editErr string
}

func (f *countTG) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		m := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		f.calls = append(f.calls, tgCall{m, p})
		editErr := f.editErr
		f.mu.Unlock()
		switch {
		case m == "editMessageText" && editErr != "":
			w.WriteHeader(http.StatusBadRequest)
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": 400, "description": editErr})
			_, _ = w.Write(b)
		case m == "answerCallbackQuery" || m == "deleteMessage" || m == "deleteMessages" || m == "editMessageText":
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *countTG) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *countTG) since(n int) []tgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tgCall(nil), f.calls[n:]...)
}

func (f *countTG) setEditErr(s string) {
	f.mu.Lock()
	f.editErr = s
	f.mu.Unlock()
}

// limited returns the calls that go through the TG_MAX_RPS limiter.
func limited(calls []tgCall) []tgCall {
	var out []tgCall
	for _, c := range calls {
		if c.method != "answerCallbackQuery" {
			out = append(out, c)
		}
	}
	return out
}

func TestOneTelegramCallPerAnswer(t *testing.T) {
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

	sid, err := testutil.CreateSubject(ctx, pool, "Один вызов "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	const total = 4
	seeds := make([]models.SeedQuestion, total)
	for i := range seeds {
		seeds[i] = models.SeedQuestion{Text: fmt.Sprintf("Вопрос-%d-%d?", sid, i), Options: [4]string{"верно", "нет1", "нет2", "нет3"},
			Correct: 0, Topic: "Тема", Difficulty: 1}
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, seeds)
	if err != nil {
		t.Fatal(err)
	}
	tgID := time.Now().UnixNano()%1_000_000_000 + 8_000_000_000
	from := &bot.TgUser{ID: tgID, FirstName: "Тимур"}
	chat := bot.Chat{ID: tgID, Type: "private"}
	seq := 0
	tap := func(data string) []tgCall {
		seq++
		n := f.n()
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: fmt.Sprintf("cb%d", seq), From: from, Data: data,
			Message: &bot.Message{MessageID: 10, Chat: chat}}})
		return f.since(n)
	}
	attemptID := func() int64 {
		var id int64
		if err := pool.QueryRow(ctx, `SELECT a.id FROM test_attempts a JOIN users u ON u.id = a.user_id
			WHERE u.telegram_id = $1 AND a.status = 'in_progress'`, tgID).Scan(&id); err != nil {
			t.Fatalf("attempt: %v", err)
		}
		return id
	}

	// Opening the test: the question (one edit) + test mode on — the main
	// menu is REPLACED by the single «🚪 Выйти из теста» button (one send,
	// once per entry, never per answer).
	if calls := tap(cbOpenTest + itoa(test.ID)); findCall(calls, "editMessageText", fmt.Sprintf("Вопрос 1/%d", total)) == nil ||
		len(limited(calls)) != 2 || !isTestModeKeyboard(findCall(calls, "sendMessage", testModeNotice)) {
		t.Fatalf("first question:\n%s", dumpCalls(calls))
	}
	aid := attemptID()
	for pos := 1; pos <= total; pos++ {
		calls := tap(fmt.Sprintf("%s%d:%d:0", cbAnswer, aid, pos))
		ack := findCall(calls, "answerCallbackQuery", "")
		if ack == nil {
			t.Fatalf("answer %d not acknowledged:\n%s", pos, dumpCalls(calls))
		}
		if s := fmt.Sprint(ack.p["text"]); s != "🟢 Правильно" && s != "🔴 Неправильно" {
			t.Fatalf("answer %d toast = %q", pos, s)
		}
		lim := limited(calls)
		if pos < total {
			// ONE limited call: the same message edited into verdict + next question.
			if len(lim) != 1 || lim[0].method != "editMessageText" || lim[0].p["message_id"] != float64(10) {
				t.Fatalf("answer %d = %d limited calls, want 1 edit:\n%s", pos, len(lim), dumpCalls(calls))
			}
			text := fmt.Sprint(lim[0].p["text"])
			if !strings.HasPrefix(text, "🟢 Правильно") && !strings.HasPrefix(text, "🔴 Неправильно — правильный ответ: ") {
				t.Fatalf("answer %d: no verdict header:\n%s", pos, text)
			}
			if !strings.Contains(text, fmt.Sprintf("Вопрос %d/%d", pos+1, total)) || !strings.Contains(fmt.Sprint(lim[0].p["reply_markup"]), cbAnswer) {
				t.Fatalf("answer %d: next question / keyboard missing:\n%s", pos, text)
			}
			// A double tap on the answered question: only a toast, no Telegram traffic.
			again := tap(fmt.Sprintf("%s%d:%d:1", cbAnswer, aid, pos))
			if len(limited(again)) != 0 || findCall(again, "answerCallbackQuery", "") == nil {
				t.Fatalf("stale tap on %d:\n%s", pos, dumpCalls(again))
			}
			continue
		}
		// Last answer: the question turns into the result ABOVE the
		// subject's tests grid (ONE edit) — the user is back in the subject
		// menu — and the main menu keyboard comes back (one send).
		res := findCall(lim, "editMessageText", "Тест завершён")
		back := findCall(lim, "sendMessage", menuBackNotice)
		if len(lim) != 2 || res == nil || back == nil || !strings.Contains(fmt.Sprint(back.p["reply_markup"]), kbSubjects) {
			t.Fatalf("final answer = %d limited calls:\n%s", len(lim), dumpCalls(calls))
		}
		kb := fmt.Sprint(res.p["reply_markup"])
		if !strings.Contains(fmt.Sprint(res.p["text"]), "Открыто тестов") ||
			!strings.Contains(kb, cbOpenTest+itoa(test.ID)) || !strings.Contains(kb, cbSubjects) || !strings.Contains(kb, cbMainMenu) {
			t.Fatalf("result must show the subject's tests list:\n%s", dumpCalls(calls))
		}
	}

	// Edit impossible (message deleted / too old): the next question comes
	// as a NEW message, the test never gets stuck.
	if calls := tap(cbOpenTest + itoa(test.ID)); findCall(calls, "editMessageText", fmt.Sprintf("Вопрос 1/%d", total)) == nil {
		t.Fatalf("second run:\n%s", dumpCalls(calls))
	}
	aid = attemptID()
	f.setEditErr("Bad Request: message to edit not found")
	calls := tap(fmt.Sprintf("%s%d:1:0", cbAnswer, aid))
	if findCall(calls, "editMessageText", "") == nil || findCall(calls, "sendMessage", fmt.Sprintf("Вопрос 2/%d", total)) == nil {
		t.Fatalf("fallback send missing:\n%s", dumpCalls(calls))
	}
	// «message is not modified» is ignored: no duplicate message.
	f.setEditErr("Bad Request: message is not modified: specified new message content and reply markup are exactly the same")
	calls = tap(fmt.Sprintf("%s%d:2:0", cbAnswer, aid))
	if lim := limited(calls); len(lim) != 1 || lim[0].method != "editMessageText" {
		t.Fatalf("not-modified must not send a copy:\n%s", dumpCalls(calls))
	}

	f.setEditErr("")

	// Exit (🚪 → ✅ Да, выйти): back to the subject's tests grid with the
	// resumable test marked ⏸ — not a mini-menu «К предметам / Главное меню».
	aid = attemptID()
	if calls := tap(cbExit + itoa(aid)); findCall(calls, "editMessageText", "Выйти из теста?") == nil {
		t.Fatalf("exit confirmation:\n%s", dumpCalls(calls))
	}
	calls = tap(cbExitYes + itoa(aid))
	ex := findCall(calls, "editMessageText", "Вы вышли из теста")
	if ex == nil || len(limited(calls)) != 2 || findCall(calls, "sendMessage", menuBackNotice) == nil {
		t.Fatalf("exit:\n%s", dumpCalls(calls))
	}
	if kb := fmt.Sprint(ex.p["reply_markup"]); !strings.Contains(fmt.Sprint(ex.p["text"]), "Открыто тестов") ||
		!strings.Contains(kb, "⏸ ") || !strings.Contains(kb, cbOpenTest+itoa(test.ID)) || !strings.Contains(kb, cbSubjects) {
		t.Fatalf("exit must show the subject's tests list:\n%s", dumpCalls(calls))
	}

	// Nothing in the whole flow may pop up the system keyboard on the
	// phone: the reply keyboard is only ever REPLACED (test mode ↔ main
	// menu), never removed, and no force_reply / placeholder.
	for _, c := range f.since(0) {
		for _, bad := range []string{"remove_keyboard", "force_reply", "input_field_placeholder"} {
			if strings.Contains(fmt.Sprint(c.p), bad) {
				t.Fatalf("%s sent %q: %v", c.method, bad, c.p)
			}
		}
		if rm, ok := c.p["reply_markup"].(map[string]any); ok && rm["keyboard"] != nil {
			text := fmt.Sprint(c.p["text"])
			if text != testModeNotice && text != menuBackNotice {
				t.Fatalf("%s sent a reply keyboard outside the test-mode switch: %v", c.method, c.p)
			}
		}
	}
}

// isTestModeKeyboard: the call carries the test-mode reply keyboard — one
// «🚪 Выйти из теста» button and nothing of the main menu.
func isTestModeKeyboard(c *tgCall) bool {
	if c == nil {
		return false
	}
	kb := fmt.Sprint(c.p["reply_markup"])
	return strings.Contains(kb, kbExitTest) && !strings.Contains(kb, kbSubjects) && !strings.Contains(kb, kbWeak)
}

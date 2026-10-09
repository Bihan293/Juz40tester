package handlers

// R-4: opening a not-yet-translated test as a Kazakh user must NOT keep
// the update handler busy while the model translates. The handler answers
// «⏳ Перевод готовится…» and returns at once; the first question (in
// Kazakh) arrives by itself once the background worker is done.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// slowKK is a fake translation provider that takes `delay` per call.
func slowKK(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		user := req.Messages[len(req.Messages)-1].Content
		var in []struct {
			ID       int      `json:"id"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
			Topic    string   `json:"topic"`
		}
		_ = json.Unmarshal([]byte(user[strings.LastIndex(user, "\n")+1:]), &in)
		type tq struct {
			ID       int      `json:"id"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
			Topic    string   `json:"topic"`
		}
		var out struct {
			Translations []tq `json:"translations"`
		}
		for _, q := range in {
			opts := make([]string, len(q.Options))
			for i, o := range q.Options {
				opts[i] = "KK " + o
			}
			out.Translations = append(out.Translations, tq{q.ID, "KK " + q.Question, opts, q.Topic})
		}
		b, _ := json.Marshal(out)
		resp, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": string(b)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenTestDoesNotWaitForTranslation(t *testing.T) {
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

	f := &fakeTG{}
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		if txt, ok := p["text"].(string); ok {
			f.mu.Lock()
			f.texts = append(f.texts, txt)
			f.mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer tgSrv.Close()

	// A subject (not a language subject) with one 20-question chain test.
	sid, err := testutil.CreateSubject(ctx, pool, "География kk "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	var testID int64
	if err := pool.QueryRow(ctx, `INSERT INTO tests (subject_id, test_number, title) VALUES ($1, 1, 'Тест 1') RETURNING id`, sid).Scan(&testID); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		var qid int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d, correct_answer, topic)
			VALUES ($1, $2, 'а', 'б', 'в', 'г', 'B', 'Тема') RETURNING id`, sid, fmt.Sprintf("Вопрос номер %d %d", i, time.Now().UnixNano())).Scan(&qid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO test_questions (test_id, question_id, position) VALUES ($1, $2, $3)`, testID, qid, i); err != nil {
			t.Fatal(err)
		}
	}

	subjects := repositories.NewSubjectRepository(pool)
	users := repositories.NewUserRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	attempts := repositories.NewAttemptRepository(pool)
	tr := services.NewTranslatorService(nil, repositories.NewTranslationRepository(pool)).
		WithGroq(groq.New("k", slowKK(t, 1500*time.Millisecond).URL))
	quiz := services.NewQuizService(subjects, attempts, state, gen, nil, users).WithTranslator(tr)
	h := New(bot.NewClient("T").WithBaseURL(tgSrv.URL), users, quiz)

	tgID := time.Now().UnixNano()%1_000_000_000 + 7_000_000_000
	u, err := users.Upsert(ctx, &models.User{TelegramID: tgID, FirstName: "Айгерим"})
	if err != nil {
		t.Fatal(err)
	}
	if err := users.SetTestLang(ctx, u.ID, models.TestLangKK); err != nil {
		t.Fatal(err)
	}

	wctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { tr.RunWorker(wctx); close(done) }()

	from := &bot.TgUser{ID: tgID, FirstName: "Айгерим"}
	start := time.Now()
	h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: "c1", From: from,
		Data: cbOpenTest + itoa(testID), Message: &bot.Message{MessageID: 1, Chat: bot.Chat{ID: tgID, Type: "private"}}}})
	if d := time.Since(start); d > 1200*time.Millisecond {
		t.Fatalf("open-test handler took %v — it must not wait for the translation", d)
	}
	if !strings.HasPrefix(f.last(), "⏳ Перевод готовится") {
		t.Fatalf("expected the «⏳ Перевод готовится…» note, last message: %q", f.last())
	}

	// The question arrives by itself — in Kazakh.
	deadline := time.Now().Add(30 * time.Second)
	for {
		// The question, then (test mode) the menu is replaced by
		// «🚪 Выйти из теста».
		if txt := f.find("❓ Вопрос 1/20"); txt != "" && f.find(testModeNotice) != "" {
			if !strings.Contains(txt, "KK Вопрос номер") {
				t.Fatalf("question shown untranslated:\n%s", txt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("question never shown; last message: %q", f.last())
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	<-done
}

package handlers

// Scenario test of the 📊 Статистика screen through the real handler, a
// real PostgreSQL (TEST_DATABASE_URL, skipped otherwise) and a fake Telegram
// Bot API: the 🔥 line must show the CURRENT streak of the user, and passing
// a test (button taps) must not break it.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

type fakeTG struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeTG) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.texts) == 0 {
		return ""
	}
	return f.texts[len(f.texts)-1]
}

func TestStatisticsShowsStreak(t *testing.T) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		if txt, ok := p["text"].(string); ok && (strings.HasSuffix(r.URL.Path, "/sendMessage") || strings.HasSuffix(r.URL.Path, "/editMessageText")) {
			f.mu.Lock()
			f.texts = append(f.texts, txt)
			f.mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`)
	}))
	defer srv.Close()

	subjects := repositories.NewSubjectRepository(pool)
	users := repositories.NewUserRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	attempts := repositories.NewAttemptRepository(pool)
	genSvc := services.NewGeneratorService(nil, &config.Config{}, gen, subjects, state)
	quiz := services.NewQuizService(subjects, attempts, state, gen, genSvc, users)
	h := New(bot.NewClient("T").WithBaseURL(srv.URL), users, quiz)

	tgID := time.Now().UnixNano()%1_000_000_000 + 3_000_000_000
	from := &bot.TgUser{ID: tgID, FirstName: "Аружан"}
	statsMsg := func() string {
		h.HandleUpdate(ctx, &bot.Update{Message: &bot.Message{From: from, Chat: bot.Chat{ID: tgID}, Text: kbProgress}})
		return f.last()
	}

	// Day 1 — first visit.
	txt := statsMsg()
	if !strings.HasPrefix(txt, "🔥 У вас 1-й день огонька!") || !strings.Contains(txt, "📊 Ваша статистика по предметам:") {
		t.Fatalf("day 1 stats:\n%s", txt)
	}

	// Simulate 4 previous consecutive days: streak 4, last active yesterday.
	yesterday := models.StreakToday(time.Now()).AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := pool.Exec(ctx, `UPDATE users SET streak_days = 4, last_active_date = $2::date WHERE telegram_id = $1`, tgID, yesterday); err != nil {
		t.Fatal(err)
	}
	txt = statsMsg()
	t.Logf("statistics message:\n%s", txt)
	if !strings.HasPrefix(txt, "🔥 У вас 5-й день огонька! (5 дней подряд)") {
		t.Fatalf("next-day stats:\n%s", txt)
	}
	// More activity the same day (test answers = callbacks) keeps it at 5.
	for i := 0; i < 3; i++ {
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: "c", From: from, Data: cbProgress,
			Message: &bot.Message{MessageID: 1, Chat: bot.Chat{ID: tgID}}}})
	}
	if txt = f.last(); !strings.HasPrefix(txt, "🔥 У вас 5-й день огонька!") {
		t.Fatalf("same-day stats changed:\n%s", txt)
	}

	// A missed day resets the flame to day 1.
	old := models.StreakToday(time.Now()).AddDate(0, 0, -3).Format("2006-01-02")
	if _, err := pool.Exec(ctx, `UPDATE users SET streak_days = 9, last_active_date = $2::date WHERE telegram_id = $1`, tgID, old); err != nil {
		t.Fatal(err)
	}
	if txt = statsMsg(); !strings.HasPrefix(txt, "🔥 У вас 1-й день огонька!") {
		t.Fatalf("after a missed day:\n%s", txt)
	}

	// Per-subject screen shows the flame too.
	sid, err := testutil.CreateSubject(ctx, pool, "Биология "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	h.HandleUpdate(ctx, &bot.Update{CallbackQuery: &bot.CallbackQuery{ID: "c", From: from,
		Data: cbProgSubject + itoa(sid), Message: &bot.Message{MessageID: 1, Chat: bot.Chat{ID: tgID}}}})
	if txt = f.last(); !strings.HasPrefix(txt, "🔥 У вас 1-й день огонька!") || !strings.Contains(txt, "🟢 Закреплено: 0") {
		t.Fatalf("subject stats:\n%s", txt)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestStreakLine(t *testing.T) {
	today := models.StreakToday(time.Now())
	cases := []struct {
		u    *models.User
		want string
	}{
		{nil, "🔥 Огонёк пока не горит"},
		{&models.User{StreakDays: 1, LastActiveDate: today}, "🔥 У вас 1-й день огонька!"},
		{&models.User{StreakDays: 12, LastActiveDate: today}, "🔥 У вас 12-й день огонька! (12 дней подряд)"},
		{&models.User{StreakDays: 22, LastActiveDate: today}, "🔥 У вас 22-й день огонька! (22 дня подряд)"},
		{&models.User{StreakDays: 21, LastActiveDate: today.AddDate(0, 0, -5)}, "🔥 Огонёк пока не горит"},
	}
	for _, c := range cases {
		if got := streakLine(c.u); !strings.HasPrefix(got, c.want) {
			t.Errorf("streakLine = %q, want prefix %q", got, c.want)
		}
	}
}

// #36: after the whole chain is passed (watermark = MaxVisibleTests) the
// subject screen says «Открыто тестов: 200», never 201.
func TestSubjectScreenShowsCappedLevel(t *testing.T) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer srv.Close()

	subjects := repositories.NewSubjectRepository(pool)
	users := repositories.NewUserRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	attempts := repositories.NewAttemptRepository(pool)
	quiz := services.NewQuizService(subjects, attempts, state, gen, nil, users)
	h := New(bot.NewClient("T").WithBaseURL(srv.URL), users, quiz)

	tgID := time.Now().UnixNano()%1_000_000_000 + 6_000_000_000
	u, err := users.Upsert(ctx, &models.User{TelegramID: tgID, FirstName: "Max"})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := testutil.CreateSubject(ctx, pool, "Финал "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO user_subject_state (user_id, subject_id, last_test_number) VALUES ($1, $2, $3)`,
		u.ID, sid, models.MaxVisibleTests); err != nil {
		t.Fatal(err)
	}
	text, _, err := h.renderSubject(ctx, u, sid, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Открыто тестов: "+strconv.Itoa(models.MaxVisibleTests)+" ") {
		t.Fatalf("level not capped:\n%s", text)
	}
	if strings.Contains(text, "Открыто тестов: "+strconv.Itoa(models.MaxVisibleTests+1)) {
		t.Fatalf("shows %d:\n%s", models.MaxVisibleTests+1, text)
	}
}

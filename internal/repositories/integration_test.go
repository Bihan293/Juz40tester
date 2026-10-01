package repositories

// Integration tests against a real PostgreSQL. They are skipped unless
// TEST_DATABASE_URL points to a disposable database, e.g.
//
//	TEST_DATABASE_URL=postgres://postgres:pg@localhost:5433/juz?sslmode=disable go test ./internal/repositories/
//
// The schema is (re)created by the embedded migrations.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func d(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}

func TestStreakAcrossDays(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := NewUserRepository(pool)
	tg := time.Now().UnixNano() % 1_000_000_000
	u := &models.User{TelegramID: tg, FirstName: "Streak"}

	steps := []struct {
		day  string
		want int
	}{
		{"2026-09-01", 1}, // first visit
		{"2026-09-01", 1}, // more taps / answers the same day
		{"2026-09-02", 2}, // next day
		{"2026-09-02", 2},
		{"2026-09-03", 3},
		{"2026-09-05", 1}, // missed 09-04 -> reset
		{"2026-09-06", 2},
		{"2026-09-05", 2}, // a late request with an older date never moves the date back
	}
	for i, st := range steps {
		got, err := repo.upsertAt(ctx, u, d(st.day))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got.StreakDays != st.want {
			t.Fatalf("step %d (%s): streak %d, want %d", i, st.day, got.StreakDays, st.want)
		}
	}
	got, _ := repo.upsertAt(ctx, u, d("2026-09-06"))
	if got.LastActiveDate.Format("2006-01-02") != "2026-09-06" {
		t.Fatalf("last_active_date moved back: %s", got.LastActiveDate)
	}
	// Legacy rows (streak 0 / NULL date from before migration 000007).
	if _, err := pool.Exec(ctx, `UPDATE users SET streak_days = 0, last_active_date = NULL WHERE telegram_id = $1`, tg); err != nil {
		t.Fatal(err)
	}
	got, err := repo.upsertAt(ctx, u, d("2026-09-07"))
	if err != nil || got.StreakDays != 1 {
		t.Fatalf("legacy row: streak %v err %v", got, err)
	}
}

func TestStreakLeaderboardHidesExtinguished(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := NewUserRepository(pool)
	today := models.StreakToday(time.Now())
	base := time.Now().UnixNano()%1_000_000_000 + 1_000_000_000

	live := &models.User{TelegramID: base + 1, FirstName: "Live"}
	dead := &models.User{TelegramID: base + 2, FirstName: "Dead"}
	for i := 9; i >= 0; i-- { // 10 consecutive days up to today
		if _, err := repo.upsertAt(ctx, live, today.AddDate(0, 0, -i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 30; i >= 10; i-- { // 21-day streak that ended 10 days ago
		if _, err := repo.upsertAt(ctx, dead, today.AddDate(0, 0, -i)); err != nil {
			t.Fatal(err)
		}
	}
	board, err := repo.StreakLeaderboard(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sawLive bool
	for _, e := range board {
		if e.FirstName == "Dead" && e.Username == "" && e.StreakDays == 21 {
			t.Fatalf("extinguished streak is still on the board: %+v", e)
		}
		if e.FirstName == "Live" && e.StreakDays == 10 {
			sawLive = true
		}
	}
	if !sawLive {
		t.Fatalf("live 10-day streak missing from the board: %+v", board)
	}
}

func TestQualitySweepRepository(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	subjects := NewSubjectRepository(pool)
	gen := NewGenerationRepository(pool)
	users := NewUserRepository(pool)
	attempts := NewAttemptRepository(pool)

	sid, err := subjects.EnsureSubject(ctx, "Тест качества "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	qs := make([]models.SeedQuestion, 20)
	for i := range qs {
		qs[i] = models.SeedQuestion{
			Text:    "Вопрос номер " + string(rune('A'+i)),
			Options: [4]string{"раз", "два", "три", "четыре"},
			Correct: i % 4, Topic: "Тема", Difficulty: 2,
		}
	}
	qs[0] = models.SeedQuestion{
		Text:    "В каком предложении нужно поставить тире?",
		Options: [4]string{"Наступила зима.", "Книга лежит на столе.", "Я люблю русский язык.", "Москва — столица России."},
		Correct: 3, Topic: "Тире", Difficulty: 1,
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, qs)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := subjects.TestQuestions(ctx, test.ID)
	if err != nil || len(stored) != 20 {
		t.Fatalf("questions: %d %v", len(stored), err)
	}
	bad := stored[0]

	unchecked, err := gen.UncheckedQuestions(ctx, 100000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range unchecked {
		if q.ID == bad.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("new question must be unchecked")
	}

	// A user answers the bad question correctly (D) — progress must survive
	// the in-place rewrite.
	user, err := users.upsertAt(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 2_000_000_000}, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	order := make([][]string, 20)
	ids := make([]int64, 20)
	for i := range stored {
		ids[i] = stored[i].ID
		order[i] = []string{"A", "B", "C", "D"}
	}
	att, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order)
	if err != nil {
		t.Fatal(err)
	}
	// While the bad question is unanswered in an active attempt it must not be rewritten.
	fixed := models.SeedQuestion{
		Text:    "В каком предложении на месте пропуска нужно поставить тире?",
		Options: [4]string{"Москва _ столица России.", "Зимой _ здесь холодно.", "Книга _ лежит на столе.", "Он _ мой друг."},
		Correct: 0, Topic: "Тире", Difficulty: 1,
	}
	if err := gen.ReplaceQuestionContent(ctx, bad.ID, fixed); !errors.Is(err, ErrQuestionBusy) {
		t.Fatalf("expected ErrQuestionBusy, got %v", err)
	}
	res, err := attempts.SubmitAnswer(ctx, user.ID, att.ID, 1, "D")
	if err != nil || !res.Correct {
		t.Fatalf("answer: %+v %v", res, err)
	}
	// Exit the attempt so nothing is on screen any more.
	if _, err := pool.Exec(ctx, `UPDATE test_attempts SET status = 'abandoned' WHERE id = $1`, att.ID); err != nil {
		t.Fatal(err)
	}
	if err := gen.ReplaceQuestionContent(ctx, bad.ID, fixed); err != nil {
		t.Fatalf("replace: %v", err)
	}
	q, err := subjects.GetQuestion(ctx, bad.ID)
	if err != nil {
		t.Fatal(err)
	}
	if q.CorrectAnswer != "A" || q.OptionA != fixed.Options[0] || q.Topic != "Тире" {
		t.Fatalf("rewrite not applied: %+v", q)
	}
	st, err := subjects.QuestionStatuses(ctx, user.ID, []int64{bad.ID})
	if err != nil || st[bad.ID] != models.StatusPartial {
		t.Fatalf("progress lost after rewrite: %v %v", st, err)
	}

	// Mark/attempt bookkeeping.
	other := stored[1].ID
	if err := gen.NoteQualityAttempt(ctx, other, 2); err != nil {
		t.Fatal(err)
	}
	var checked *time.Time
	_ = pool.QueryRow(ctx, `SELECT quality_checked_at FROM questions WHERE id = $1`, other).Scan(&checked)
	if checked != nil {
		t.Fatal("1st failed attempt must keep the question unchecked")
	}
	_ = gen.NoteQualityAttempt(ctx, other, 2)
	_ = pool.QueryRow(ctx, `SELECT quality_checked_at FROM questions WHERE id = $1`, other).Scan(&checked)
	if checked == nil {
		t.Fatal("after max attempts the question must be given up (checked)")
	}
	if err := gen.MarkQuestionsChecked(ctx, ids); err != nil {
		t.Fatal(err)
	}
	left, _ := gen.UncheckedQuestions(ctx, 100000)
	for _, q := range left {
		for _, id := range ids {
			if q.ID == id {
				t.Fatalf("question %d still unchecked", id)
			}
		}
	}
	// Subject boards still work with the live-streak expression.
	if _, err := subjects.UnlockedTestsLeaderboard(ctx, sid, 10); err != nil {
		t.Fatalf("unlocked board: %v", err)
	}
	if _, err := subjects.GreenLeaderboard(ctx, sid, 10); err != nil {
		t.Fatalf("green board: %v", err)
	}
}

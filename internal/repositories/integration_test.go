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
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
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

	sid, err := testutil.CreateSubject(ctx, pool, "Тест качества "+time.Now().Format("150405.000000"))
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
	att, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, false)
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
	var q models.Question
	if err := pool.QueryRow(ctx, `
		SELECT option_a, correct_answer, topic FROM questions WHERE id = $1`, bad.ID).
		Scan(&q.OptionA, &q.CorrectAnswer, &q.Topic); err != nil {
		t.Fatal(err)
	}
	if q.CorrectAnswer != "A" || q.OptionA != fixed.Options[0] || q.Topic != "Тире" {
		t.Fatalf("rewrite not applied: %+v", q)
	}
	// A rewritten question is a NEW question: the old 🟢/🟡 must be reset.
	st := models.StatusNone
	err = pool.QueryRow(ctx, `SELECT status FROM user_question_progress
		WHERE user_id = $1 AND question_id = $2`, user.ID, bad.ID).Scan(&st)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if st != models.StatusNone {
		t.Fatalf("stale progress kept after rewrite: %v", st)
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

// TestAttemptGuards covers: a single in-progress attempt per (user, test),
// replace on retry, out-of-order answers rejected, finishing only after all
// questions are answered, busy/postpone of the quality sweep and the stale
// attempts reaper.
func TestAttemptGuards(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	subjects := NewSubjectRepository(pool)
	gen := NewGenerationRepository(pool)
	users := NewUserRepository(pool)
	attempts := NewAttemptRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Тест попыток "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	qs := make([]models.SeedQuestion, 3)
	for i := range qs {
		qs[i] = models.SeedQuestion{
			Text:    "Вопрос про попытки " + string(rune('A'+i)),
			Options: [4]string{"раз", "два", "три", "четыре"},
			Correct: 0, Topic: "Тема", Difficulty: 2,
		}
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, qs)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := subjects.TestQuestions(ctx, test.ID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := users.upsertAt(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 3_000_000_000}, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(stored))
	order := make([][]string, len(stored))
	for i := range stored {
		ids[i] = stored[i].ID
		order[i] = []string{"A", "B", "C", "D"}
	}

	// Concurrent "double tap": exactly one in-progress attempt.
	var wg sync.WaitGroup
	got := make([]int64, 4)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, false)
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			got[i] = a.ID
		}(i)
	}
	wg.Wait()
	for _, id := range got {
		if id != got[0] {
			t.Fatalf("double tap created several attempts: %v", got)
		}
	}
	var open int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM test_attempts WHERE user_id=$1 AND test_id=$2 AND status='in_progress'`, user.ID, test.ID).Scan(&open); err != nil || open != 1 {
		t.Fatalf("open attempts: %d %v", open, err)
	}

	// The question at position 1 is busy now.
	busy, err := gen.IsQuestionBusy(ctx, ids[0])
	if err != nil || !busy {
		t.Fatalf("question must be busy: %v %v", busy, err)
	}
	if err := gen.PostponeQualityCheck(ctx, ids[0], time.Hour); err != nil {
		t.Fatal(err)
	}
	unchecked, _ := gen.UncheckedQuestions(ctx, 100000)
	for _, q := range unchecked {
		if q.ID == ids[0] {
			t.Fatal("postponed question must be hidden from the sweep")
		}
	}

	// Retry replaces the open attempt.
	a2, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, true)
	if err != nil || a2.ID == got[0] {
		t.Fatalf("retry must create a new attempt: %v %v", a2, err)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM test_attempts WHERE id=$1`, got[0]).Scan(&st)
	if st != models.AttemptAbandoned {
		t.Fatalf("old attempt must be abandoned, got %s", st)
	}

	// Answering the LAST position first is rejected and does not finish.
	if _, err := attempts.SubmitAnswer(ctx, user.ID, a2.ID, 3, "A"); !errors.Is(err, ErrAnswerOutOfOrder) {
		t.Fatalf("out-of-order answer must be rejected, got %v", err)
	}
	for pos := 1; pos <= 3; pos++ {
		res, err := attempts.SubmitAnswer(ctx, user.ID, a2.ID, pos, "A")
		if err != nil {
			t.Fatalf("answer %d: %v", pos, err)
		}
		if res.Finished != (pos == 3) {
			t.Fatalf("finished at pos %d = %v", pos, res.Finished)
		}
	}

	// Stale attempts reaper.
	a3, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE test_attempts SET updated_at = now() - interval '40 days' WHERE id=$1`, a3.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := attempts.AbandonStaleAttempts(ctx, 30*24*time.Hour); err != nil || n < 1 {
		t.Fatalf("reaper: %d %v", n, err)
	}
	_ = pool.QueryRow(ctx, `SELECT status FROM test_attempts WHERE id=$1`, a3.ID).Scan(&st)
	if st != models.AttemptAbandoned {
		t.Fatalf("stale attempt must be abandoned, got %s", st)
	}
}

// TestReviveChainJobOrphanedAndDuplicates covers audit #11/#12: a 'done'
// job whose test is gone must be released and revived; several failed rows
// must be revived one at a time without a unique-index violation.
func TestReviveChainJobOrphanedAndDuplicates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	subjects := NewSubjectRepository(pool)
	gen := NewGenerationRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Revive "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	// #11: orphaned done row (test_id NULL — the test was deleted).
	if _, err := pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, test_number, status)
		VALUES ('chain', $1, 1, 'done')`, sid); err != nil {
		t.Fatal(err)
	}
	revived, err := gen.ReviveChainJob(ctx, sid, 1, 0)
	if err != nil || !revived {
		t.Fatalf("orphaned done job not revived: %v %v", revived, err)
	}
	if ok, _ := gen.HasPendingOrRunningChainJob(ctx, sid, 1); !ok {
		t.Fatal("expected an active job after revive")
	}

	// #12: two failed rows for test 2 — revive must not violate the index.
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO generation_jobs (kind, subject_id, test_number, status)
			VALUES ('chain', $1, 2, 'failed')`, sid); err != nil {
			t.Fatal(err)
		}
	}
	if revived, err := gen.ReviveChainJob(ctx, sid, 2, 0); err != nil || !revived {
		t.Fatalf("revive failed rows: %v %v", revived, err)
	}
	// Second call: an active job exists — nothing revived, no error.
	if revived, err := gen.ReviveChainJob(ctx, sid, 2, 0); err != nil || revived {
		t.Fatalf("second revive must be a no-op: %v %v", revived, err)
	}

	// #15: a user with no answers but a watermark is ranked by level.
	users := NewUserRepository(pool)
	u, err := users.upsertAt(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 3_000_000_000}, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_subject_state (user_id, subject_id, last_test_number)
		VALUES ($1, $2, 3)`, u.ID, sid); err != nil {
		t.Fatal(err)
	}
	board, err := subjects.UnlockedTestsLeaderboard(ctx, sid, 10)
	if err != nil {
		t.Fatalf("unlocked board: %v", err)
	}
	found := false
	for _, e := range board {
		if e.UserID == u.ID {
			found = true
			if e.UnlockedTests != 4 {
				t.Fatalf("level = %d, want 4", e.UnlockedTests)
			}
		}
	}
	if !found {
		t.Fatal("user with watermark missing from the level board")
	}
}

// #34: repeated taps with unchanged profile on the same day must NOT write
// the users row; a profile change or a new day must.
func TestUpsertSkipsNoopWrites(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := NewUserRepository(pool)
	u := &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 4_000_000_000, FirstName: "Noop", Username: "noop"}

	created, err := repo.upsertAt(ctx, u, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.StreakDays != 1 || created.FirstName != "Noop" {
		t.Fatalf("new user not created correctly: %+v", created)
	}
	xmin := func() string {
		var x string
		if err := pool.QueryRow(ctx, `SELECT xmin::text FROM users WHERE id = $1`, created.ID).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	before := xmin()
	for i := 0; i < 5; i++ { // five answers the same day
		got, err := repo.upsertAt(ctx, u, d("2026-10-01"))
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != created.ID || got.StreakDays != 1 || !got.UpdatedAt.Equal(created.UpdatedAt) {
			t.Fatalf("no-op tap changed the row: %+v", got)
		}
	}
	if after := xmin(); after != before {
		t.Fatalf("no-op taps wrote the row (xmin %s → %s)", before, after)
	}

	// Profile change → written.
	u.FirstName = "Renamed"
	got, err := repo.upsertAt(ctx, u, d("2026-10-01"))
	if err != nil {
		t.Fatal(err)
	}
	if got.FirstName != "Renamed" || !got.UpdatedAt.After(created.UpdatedAt) || xmin() == before {
		t.Fatalf("profile change not persisted: %+v", got)
	}
	// Next day → streak written.
	got, err = repo.upsertAt(ctx, u, d("2026-10-02"))
	if err != nil {
		t.Fatal(err)
	}
	if got.StreakDays != 2 {
		t.Fatalf("next-day streak = %d, want 2", got.StreakDays)
	}
	// test_lang (set separately) is returned on the read-only path too.
	if err := repo.SetTestLang(ctx, got.ID, models.TestLangKK); err != nil {
		t.Fatal(err)
	}
	got, err = repo.upsertAt(ctx, u, d("2026-10-02"))
	if err != nil {
		t.Fatal(err)
	}
	if got.TestLang != models.TestLangKK || got.StreakDays != 2 {
		t.Fatalf("read path lost data: %+v", got)
	}
}

// #34: concurrent first contacts of the same user all succeed.
func TestUpsertConcurrentFirstContact(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	repo := NewUserRepository(pool)
	tg := time.Now().UnixNano()%1_000_000_000 + 5_000_000_000
	errs := make(chan error, 8)
	ids := make(chan int64, 8)
	for i := 0; i < 8; i++ {
		go func() {
			u, err := repo.upsertAt(ctx, &models.User{TelegramID: tg, FirstName: "Race"}, d("2026-10-01"))
			if err != nil {
				errs <- err
				return
			}
			ids <- u.ID
			errs <- nil
		}()
	}
	var first int64
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	close(ids)
	for id := range ids {
		if first == 0 {
			first = id
		} else if id != first {
			t.Fatalf("different ids for the same user: %d vs %d", first, id)
		}
	}
}

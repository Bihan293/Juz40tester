package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// A late answer to an attempt that is no longer in progress (duplicate of
// the final answer, or a stale question of an attempt abandoned by a
// restart) is ErrAttemptClosed — the handler tells the user so instead of
// «Не удалось сохранить ответ».
func TestSubmitAnswerClosedAttempt(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	subjects := NewSubjectRepository(pool)
	users := NewUserRepository(pool)
	attempts := NewAttemptRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Closed "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	qs := []models.SeedQuestion{
		{Text: "C вопрос 1", Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "Тема C", Difficulty: 2},
		{Text: "C вопрос 2", Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "Тема C", Difficulty: 2},
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, qs)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := subjects.TestQuestions(ctx, test.ID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := users.Upsert(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 7_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{stored[0].ID, stored[1].ID}
	order := [][]string{{"A", "B", "C", "D"}, {"A", "B", "C", "D"}}

	// Abandoned by «🔄 Пройти ещё раз»: its question 1 is still unanswered.
	old, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, false)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, true)
	if err != nil || fresh.ID == old.ID {
		t.Fatalf("restart: %v (old %d, new %d)", err, old.ID, fresh.ID)
	}
	if _, err := attempts.SubmitAnswer(ctx, user.ID, old.ID, 1, "A"); !errors.Is(err, ErrAttemptClosed) {
		t.Fatalf("answer to an abandoned attempt: %v, want ErrAttemptClosed", err)
	}

	// Completed: a late duplicate of the final answer.
	for pos := 1; pos <= 2; pos++ {
		if _, err := attempts.SubmitAnswer(ctx, user.ID, fresh.ID, pos, "A"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := attempts.SubmitAnswer(ctx, user.ID, fresh.ID, 2, "A"); !errors.Is(err, ErrAttemptClosed) {
		t.Fatalf("late duplicate final answer: %v, want ErrAttemptClosed", err)
	}
}

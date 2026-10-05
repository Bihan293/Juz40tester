package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestSubmitAnswerTransitions (A4): status transitions 🔴→🟡→🟢 and back,
// double tap, out-of-order answer, foreign attempt, topic stats skip for 🟢.
func TestSubmitAnswerTransitions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	subjects := NewSubjectRepository(pool)
	users := NewUserRepository(pool)
	attempts := NewAttemptRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "A4 "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	qs := []models.SeedQuestion{
		{Text: "A4 вопрос 1", Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "Тема A4", Difficulty: 2},
		{Text: "A4 вопрос 2", Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "Тема A4", Difficulty: 2},
	}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, qs)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := subjects.TestQuestions(ctx, test.ID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixNano()%1_000_000_000 + 6_000_000_000
	user, err := users.Upsert(ctx, &models.User{TelegramID: base})
	if err != nil {
		t.Fatal(err)
	}
	other, err := users.Upsert(ctx, &models.User{TelegramID: base + 1})
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{stored[0].ID, stored[1].ID}
	order := [][]string{{"A", "B", "C", "D"}, {"A", "B", "C", "D"}}
	q1 := stored[0].ID

	topicCounts := func() (int, int) {
		var c, w int
		_ = pool.QueryRow(ctx, `SELECT COALESCE(SUM(correct_count),0), COALESCE(SUM(wrong_count),0)
			FROM user_topic_stats WHERE user_id = $1 AND subject_id = $2`, user.ID, sid).Scan(&c, &w)
		return c, w
	}
	progress := func() (int, int, int) {
		var s, c, w int
		_ = pool.QueryRow(ctx, `SELECT status, correct_count, wrong_count FROM user_question_progress
			WHERE user_id = $1 AND question_id = $2`, user.ID, q1).Scan(&s, &c, &w)
		return s, c, w
	}

	// Answer sequence on question 1 (one attempt per answer) and the
	// expected status after each; wrong on 🔴 stays 🔴.
	steps := []struct {
		ans  string
		want int
	}{
		{"B", models.StatusNone}, {"A", models.StatusPartial}, {"A", models.StatusMastered},
		{"A", models.StatusMastered}, {"B", models.StatusPartial}, {"B", models.StatusNone},
	}
	wantTopicC, wantTopicW := 0, 0
	prev := models.StatusNone
	for i, st := range steps {
		a, err := attempts.CreateAttempt(ctx, user.ID, test.ID, ids, order, true)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// Foreign attempt and out-of-order answer are rejected.
			if _, err := attempts.SubmitAnswer(ctx, other.ID, a.ID, 1, "A"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign attempt: %v", err)
			}
			if _, err := attempts.SubmitAnswer(ctx, user.ID, a.ID, 2, "A"); !errors.Is(err, ErrAnswerOutOfOrder) {
				t.Fatalf("out of order: %v", err)
			}
		}
		res, err := attempts.SubmitAnswer(ctx, user.ID, a.ID, 1, st.ans)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if res.NewStatus != st.want || res.Correct != (st.ans == "A") || res.Finished {
			t.Fatalf("step %d: status %d correct %v finished %v, want %d", i, res.NewStatus, res.Correct, res.Finished, st.want)
		}
		if prev != models.StatusMastered {
			if st.ans == "A" {
				wantTopicC++
			} else {
				wantTopicW++
			}
		}
		prev = st.want
		// Double tap: not counted again.
		dup, err := attempts.SubmitAnswer(ctx, user.ID, a.ID, 1, "A")
		if err != nil || !dup.AlreadyAnswered {
			t.Fatalf("step %d double tap: %+v %v", i, dup, err)
		}
		if i == len(steps)-1 {
			last, err := attempts.SubmitAnswer(ctx, user.ID, a.ID, 2, "A")
			if err != nil || !last.Finished || last.Attempt.Status != models.AttemptCompleted ||
				last.Attempt.CorrectCount != 1 || last.Attempt.WrongCount != 1 {
				t.Fatalf("finish: %+v %v", last, err)
			}
		}
	}
	s, c, w := progress()
	if s != models.StatusNone || c != 3 || w != 3 {
		t.Fatalf("progress status=%d correct=%d wrong=%d", s, c, w)
	}
	// Topic stats: the "A" on 🟢 (step 4) and "B" on 🟢 (step 5) are skipped;
	// question 2 adds one correct answer.
	tc, tw := topicCounts()
	if tc != wantTopicC+1 || tw != wantTopicW {
		t.Fatalf("topic stats %d/%d, want %d/%d", tc, tw, wantTopicC+1, wantTopicW)
	}
}

package repositories

import (
	"context"
	"fmt"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// TestDeletePersonalKeepsProgressOnSharedQuestions: finishing a generated
// personal test whose questions are still used by another student's test
// must keep the user's progress on them — otherwise the bank hands the
// already mastered questions out again as «new» in the next weak test.
func TestDeletePersonalKeepsProgressOnSharedQuestions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	subjects := NewSubjectRepository(pool)
	sid := bankFixture(t, ctx, pool, "Shared progress", []string{"тх"}, 0)
	a := bankUser(t, ctx, pool, 11)
	b := bankUser(t, ctx, pool, 12)

	var qs []models.SeedQuestion
	for i := 0; i < 4; i++ {
		qs = append(qs, models.SeedQuestion{Text: fmt.Sprintf("shared %d q%d", sid, i), Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "тх", Difficulty: 2})
	}
	ta, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, Title: "A", Kind: models.TestKindPersonal, OwnerUserID: a.ID, Topics: []string{"тх"}}, qs)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := subjects.TestQuestionIDs(ctx, ta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE questions SET quality_checked_at = now(), topic_key = 'тх' WHERE id = ANY($1)`, ids); err != nil {
		t.Fatal(err)
	}
	// Student B's test links the same question rows.
	if _, err := gen.CreateBankPersonalTest(ctx, sid, b.ID, "B", []string{"тх"}, ids); err != nil {
		t.Fatal(err)
	}
	// A masters the first question.
	mastered := ids[0]
	if _, err := pool.Exec(ctx, `INSERT INTO user_question_progress (user_id, question_id, status) VALUES ($1, $2, 2)`, a.ID, mastered); err != nil {
		t.Fatal(err)
	}
	if err := gen.DeletePersonalTest(ctx, a.ID, ta.ID, sid); err != nil {
		t.Fatal(err)
	}
	var st int
	if err := pool.QueryRow(ctx, `SELECT status FROM user_question_progress WHERE user_id = $1 AND question_id = $2`, a.ID, mastered).Scan(&st); err != nil || st != 2 {
		t.Fatalf("progress on a surviving shared question must stay: status=%d err=%v", st, err)
	}
	got, _, err := gen.BankCandidates(ctx, sid, a.ID, []string{"тх"}, map[string]int{"тх": 4}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range got {
		if id == mastered {
			t.Fatal("the bank re-served a question the user already mastered")
		}
	}
	if len(got) != 3 {
		t.Fatalf("want the 3 unsolved shared questions, got %v", got)
	}
}

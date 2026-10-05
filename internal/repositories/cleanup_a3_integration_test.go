package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestCleanupStaleTemplatesKeepsSharedQuestions (A3): an old template is
// deleted, a question still linked to another test survives.
func TestCleanupStaleTemplatesKeepsSharedQuestions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	cl := NewCleanupRepository(pool)
	sid, err := testutil.CreateSubject(ctx, pool, "Шаблоны "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	qs := []models.SeedQuestion{{Text: "Шаблонный вопрос", Options: [4]string{"a", "b", "c", "d"}, Topic: "Т", Difficulty: 2}}
	tpl, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, Title: "T", Kind: models.TestKindPersonal, TopicsFingerprint: "x"}, qs)
	if err != nil {
		t.Fatal(err)
	}
	other, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "C", Kind: models.TestKindChain}, qs)
	if err != nil {
		t.Fatal(err)
	}
	// Share the template's question with the chain test, age the template.
	if _, err := pool.Exec(ctx, `INSERT INTO test_questions (test_id, question_id, position)
		SELECT $2, question_id, 99 FROM test_questions WHERE test_id = $1`, tpl.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tests SET owner_user_id = NULL, is_active = FALSE,
		created_at = now() - interval '100 days' WHERE id = $1`, tpl.ID); err != nil {
		t.Fatal(err)
	}
	n, err := cl.DeleteStaleTemplates(ctx, 60*24*time.Hour, CleanupBatchSize)
	if err != nil || n < 1 {
		t.Fatalf("deleted %d, err %v", n, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tests WHERE id = $1`, tpl.ID).Scan(&left)
	var shared int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM test_questions WHERE test_id = $1`, other.ID).Scan(&shared)
	if left != 0 || shared != 2 {
		t.Fatalf("template left=%d, shared links=%d (want 0, 2)", left, shared)
	}
	if _, err := cl.DeleteOldTranslationJobs(ctx, 24*time.Hour, CleanupBatchSize); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.DeleteOrphanPersonalDone(ctx, 0, CleanupBatchSize); err != nil {
		t.Fatal(err)
	}
}

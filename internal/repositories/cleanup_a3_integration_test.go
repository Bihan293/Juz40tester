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
}

// TestCleanupOldFinishedAttemptsKeepsBestAndLatest (A3): of four old
// finished attempts only the best and the latest survive.
func TestCleanupOldFinishedAttemptsKeepsBestAndLatest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	cl := NewCleanupRepository(pool)
	u, err := NewUserRepository(pool).Upsert(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 5_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	sid, err := testutil.CreateSubject(ctx, pool, "Попытки "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	qs := []models.SeedQuestion{{Text: "В", Options: [4]string{"a", "b", "c", "d"}, Topic: "Т", Difficulty: 2}}
	tst, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "C", Kind: models.TestKindChain}, qs)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range []int{3, 9, 1, 2} { // best = 9, latest = 2
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO test_attempts (user_id, test_id, status, correct_count, updated_at)
			VALUES ($1, $2, 'completed', $3, now() - interval '400 days') RETURNING id`, u.ID, tst.ID, c).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := cl.DeleteOldFinishedAttempts(ctx, 180*24*time.Hour, CleanupBatchSize); err != nil {
		t.Fatal(err)
	}
	var left []int64
	rows, _ := pool.Query(ctx, `SELECT id FROM test_attempts WHERE user_id = $1 ORDER BY id`, u.ID)
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	if len(left) != 2 || left[0] != ids[1] || left[1] != ids[3] {
		t.Fatalf("left %v, want [%d %d]", left, ids[1], ids[3])
	}
}

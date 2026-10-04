package services

import (
	"context"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// R-6: questions that passed the audit in runJob (toSeed) are stored as
// checked, a clone inherits that, and neither is returned to the sweep.
func TestR6AuditedQuestionsSkipSweep(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	gt := &generatedTest{Questions: validQuestions()}
	src, err := e.gen.CreateGeneratedTest(ctx, &models.Test{
		SubjectID: e.sid, Title: "🎯", Kind: models.TestKindPersonal, OwnerUserID: e.user(t, "A"), TopicsFingerprint: "fp-r6",
	}, gt.toSeed())
	if err != nil {
		t.Fatal(err)
	}
	qs, err := e.gen.PersonalTestQuestions(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := e.gen.ClonePersonalTest(ctx, src, qs, e.user(t, "B"))
	if err != nil {
		t.Fatal(err)
	}
	var unchecked int
	if err := e.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM test_questions tq JOIN questions q ON q.id = tq.question_id
		WHERE tq.test_id IN ($1, $2) AND q.quality_checked_at IS NULL`, src.ID, clone.ID).Scan(&unchecked); err != nil {
		t.Fatal(err)
	}
	if unchecked != 0 {
		t.Fatalf("%d audited/cloned question(s) left for the sweep", unchecked)
	}
}

// R-6: the session-level sweep lock holds no transaction while fn runs.
func TestR6SweepLockNoIdleTransaction(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	_, err := e.gen.TryQualitySweepLock(ctx, func(context.Context) error {
		var n int
		if err := e.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state LIKE 'idle in transaction%'`).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("%d idle-in-transaction session(s) while the sweep lock is held", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// R-8b: a clone of a fully checked test links the SAME question rows; two
// owners' progress on them is independent.
func TestR8bSharedCloneIndependentProgress(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	gt := &generatedTest{Questions: validQuestions()}
	a, b := e.user(t, "A"), e.user(t, "B")
	src, err := e.gen.CreateGeneratedTest(ctx, &models.Test{
		SubjectID: e.sid, Title: "🎯", Kind: models.TestKindPersonal, OwnerUserID: a, TopicsFingerprint: "fp-r8b",
	}, gt.toSeed())
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	qs, err := e.gen.PersonalTestQuestions(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := e.gen.ClonePersonalTest(ctx, src, qs, b)
	if err != nil {
		t.Fatal(err)
	}
	var after, shared int
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("clone created %d new question rows", after-before)
	}
	if err := e.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM test_questions x JOIN test_questions y
		  ON y.question_id = x.question_id AND y.position = x.position
		WHERE x.test_id = $1 AND y.test_id = $2`, src.ID, clone.ID).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if shared != len(qs) {
		t.Fatalf("clone shares %d of %d questions", shared, len(qs))
	}
	var qid int64
	if err := e.pool.QueryRow(ctx, `SELECT question_id FROM test_questions WHERE test_id = $1 ORDER BY position LIMIT 1`, src.ID).Scan(&qid); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		id     int64
		status int
	}{{a, 2}, {b, 1}} {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO user_question_progress (user_id, question_id, status, correct_count, wrong_count)
			VALUES ($1, $2, $3, 1, 0)`, u.id, qid, u.status); err != nil {
			t.Fatal(err)
		}
	}
	// B finishes the clone: A's progress and the shared question survive.
	if err := e.gen.DeletePersonalTest(ctx, b, clone.ID, e.sid); err != nil {
		t.Fatal(err)
	}
	var aStatus int
	if err := e.pool.QueryRow(ctx, `SELECT status FROM user_question_progress WHERE user_id = $1 AND question_id = $2`, a, qid).Scan(&aStatus); err != nil {
		t.Fatalf("A's progress lost: %v", err)
	}
	if aStatus != 2 {
		t.Fatalf("A's status changed to %d", aStatus)
	}
	var bRows int
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_question_progress WHERE user_id = $1 AND question_id = $2`, b, qid).Scan(&bRows); err != nil {
		t.Fatal(err)
	}
	if bRows != 0 {
		t.Fatal("B's progress on the finished clone was kept")
	}
}

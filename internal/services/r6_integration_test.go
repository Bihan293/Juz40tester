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

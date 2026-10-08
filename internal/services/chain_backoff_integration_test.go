package services

// A chain test whose generation fails every time must not start a new round
// of paid AI attempts on every open of the subject screen / every ⏳ tap
// (skipped without TEST_DATABASE_URL).

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// activeChainJob returns the id of the pending/running job of chain test n
// (0 = none).
func (e *flowEnv) activeChainJob(t *testing.T, n int) int64 {
	t.Helper()
	var id int64
	err := e.pool.QueryRow(context.Background(), `
		SELECT COALESCE(MAX(id), 0) FROM generation_jobs
		WHERE kind = 'chain' AND subject_id = $1 AND test_number = $2 AND status IN ('pending','running')`,
		e.sid, n).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// failChainJob simulates a job that spent all its attempts (every provider
// returned an unusable reply): it is parked as 'failed' right now.
func (e *flowEnv) failChainJob(t *testing.T, id int64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `
		UPDATE generation_jobs SET status = 'failed', attempts = $2, last_error = 'invalid reply', updated_at = now()
		WHERE id = $1`, id, maxJobAttempts); err != nil {
		t.Fatal(err)
	}
}

func TestFailingChainTestIsNotRequeuedOnEveryOpen(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Opener")
	t1 := e.firstChainTest(t, 1)
	e.setStatuses(t, u, t1, 20, 0) // Тест 2 is open but missing

	open := func() {
		t.Helper()
		if _, err := e.quiz.GetSubjectScreen(ctx, u, e.sid, 0, false); err != nil {
			t.Fatal(err)
		}
	}

	// First open: the missing openable test is queued (self-healing).
	open()
	first := e.activeChainJob(t, 2)
	if first == 0 {
		t.Fatal("the missing openable Тест 2 must be queued on the first open")
	}
	// The job pays for a few questions, then fails for good.
	if err := e.gen.SaveJobSlots(ctx, first, map[int]models.SeedQuestion{0: {
		Text: "Сохранённый вопрос прошлой попытки", Options: [4]string{"a", "b", "c", "d"}, Topic: "Тема 0", Difficulty: 2,
	}}, "test"); err != nil {
		t.Fatal(err)
	}
	e.failChainJob(t, first)

	// The user keeps opening the subject screen while the generation keeps
	// failing. Count how many paid rounds (queued jobs) that starts.
	rounds := 0
	for i := 0; i < 6; i++ {
		open()
		if id := e.activeChainJob(t, 2); id != 0 {
			rounds++
			e.failChainJob(t, id)
		}
	}
	// One immediate retry is fine (an outage may be over); every further
	// round waits for the backoff — before the fix this was 6 rounds,
	// i.e. 6×3 paid attempts within a minute.
	if rounds != 1 {
		t.Fatalf("subject screen opens started %d generation rounds of a failing test, want 1", rounds)
	}
	var rows, revivals int
	if err := e.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(MAX(revivals), 0) FROM generation_jobs
		WHERE kind = 'chain' AND subject_id = $1 AND test_number = 2`, e.sid).Scan(&rows, &revivals); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || revivals != 1 {
		t.Fatalf("the failed job must be revived in place (rows=%d revivals=%d), not duplicated", rows, revivals)
	}
	// The revived job keeps what its previous attempts already paid for.
	if b, err := e.gen.JobBatches(ctx, first); err != nil || len(b) != 1 {
		t.Fatalf("stored batches of the failed job must survive the revive: %v %v", b, err)
	}

	// ⏳ tap and the next-test unlock path respect the same backoff, and the
	// tap reports when a retry is possible.
	test, retryAt, err := e.quiz.ChainTestOrRevive(ctx, e.sid, 2, u)
	if err != nil || test != nil {
		t.Fatalf("ChainTestOrRevive: test=%v err=%v", test, err)
	}
	if wait := time.Until(retryAt); wait < 25*time.Minute || wait > repositories.ChainReviveBaseBackoff+time.Minute {
		t.Fatalf("retryAt must be ~30 min after the failure, got %v", wait)
	}
	if out := e.quiz.OnTestCompleted(ctx, u, &models.Test{ID: t1, SubjectID: e.sid, TestNumber: 1, Kind: models.TestKindChain}, 20, 0); out.NextGenerating {
		t.Fatal("a retry of Тест 1 must not re-queue the failing Тест 2 during its backoff")
	}
	if e.activeChainJob(t, 2) != 0 {
		t.Fatal("no job may be queued during the backoff")
	}

	// After the pause the next open revives it again; the following pause
	// is twice as long.
	if _, err := e.pool.Exec(ctx, `UPDATE generation_jobs SET updated_at = now() - interval '31 minutes' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	open()
	if id := e.activeChainJob(t, 2); id != first {
		t.Fatalf("after the backoff the failed job must be revived in place, active=%d want %d", id, first)
	}
	if j := e.job(t, first); j.attempts != 0 || !j.urgent {
		t.Fatalf("revived job: attempts=%d urgent=%v, want 0/true", j.attempts, j.urgent)
	}
	e.failChainJob(t, first)
	if _, retryAt, _ = e.quiz.ChainTestOrRevive(ctx, e.sid, 2, u); time.Until(retryAt) < 55*time.Minute {
		t.Fatalf("second backoff must be ~1 h, got %v", time.Until(retryAt))
	}

	// A healthy generation finishes the story: the test appears and the
	// tap opens it.
	if _, err := e.pool.Exec(ctx, `UPDATE generation_jobs SET updated_at = now() - interval '2 hours' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	open()
	e.drain(t)
	if test, _, err := e.quiz.ChainTestOrRevive(ctx, e.sid, 2, u); err != nil || test == nil {
		t.Fatalf("Тест 2 must exist after a successful run: %v %v", test, err)
	}
}

// A done job whose test was deleted is released and revived at once (no
// backoff carried over from earlier failures).
func TestOrphanedDoneChainJobRevivedWithoutBackoff(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Orphan")
	t1 := e.firstChainTest(t, 1)
	e.setStatuses(t, u, t1, 20, 0)
	var id int64
	if err := e.pool.QueryRow(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, test_number, status, revivals, updated_at)
		VALUES ('chain', $1, 2, 'done', 4, now()) RETURNING id`, e.sid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, retryAt, err := e.quiz.ChainTestOrRevive(ctx, e.sid, 2, u); err != nil || !retryAt.IsZero() {
		t.Fatalf("orphaned done job: retryAt=%v err=%v", retryAt, err)
	}
	if e.activeChainJob(t, 2) != id {
		t.Fatal("the orphaned done job must be revived right away")
	}
}

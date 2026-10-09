package services

// Reliability scenarios of audit part 3 (#19, #23, #25–#27) and the
// re-verification of #13, #16, #18 against a real PostgreSQL (skipped
// without TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// jobRow reads the fields of a generation job the tests assert on.
type jobRow struct {
	status    string
	attempts  int
	urgent    bool
	dueInSecs float64 // not_before - now(), seconds
}

func (e *flowEnv) job(t *testing.T, id int64) jobRow {
	t.Helper()
	var j jobRow
	if err := e.pool.QueryRow(context.Background(), `
		SELECT status, attempts, urgent, EXTRACT(EPOCH FROM (not_before - now()))::float8
		FROM generation_jobs WHERE id = $1`, id).Scan(&j.status, &j.attempts, &j.urgent, &j.dueInSecs); err != nil {
		t.Fatal(err)
	}
	return j
}

func (e *flowEnv) insertJob(t *testing.T, testNumber int, status string, attempts int, urgent bool, notBefore, updatedAgo string) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `
		INSERT INTO generation_jobs (kind, subject_id, test_number, status, attempts, urgent, not_before, updated_at)
		VALUES ('chain', $1, $2, $3, $4, $5, now() + $6::interval, now() - $7::interval)
		RETURNING id`, e.sid, testNumber, status, attempts, urgent, notBefore, updatedAgo).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// #19: a job interrupted by shutdown (worker ctx cancelled) goes back to
// 'pending' with not_before = now() and WITHOUT spending an attempt — it is
// not left in 'running' for the stuck-job reaper.
func TestShutdownReleasesRunningJob(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()

	// An AI provider that hangs until the request is cancelled.
	var reached int32
	stopHang := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt32(&reached, 1)
		select {
		case <-r.Context().Done():
		case <-stopHang:
		}
	}))
	defer hang.Close()
	defer close(stopHang)
	state := repositories.NewStateRepository(e.pool)
	svc := NewGeneratorService(deepseek.New("k", hang.URL), &config.Config{}, e.gen, e.subjects, state)

	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	job, err := e.claimOwn(ctx) // running, attempts 0 -> 1
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if j := e.job(t, job.ID); j.status != "running" || j.attempts != 1 {
		t.Fatalf("claimed job: %+v", j)
	}

	wctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.executeJob(wctx, job); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&reached) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel() // SIGTERM
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("executeJob did not return after cancellation")
	}
	j := e.job(t, job.ID)
	if j.status != "pending" {
		t.Fatalf("interrupted job must be pending, got %q", j.status)
	}
	if j.attempts != 0 {
		t.Fatalf("interrupted run must not count as an attempt, attempts = %d", j.attempts)
	}
	if j.dueInSecs > 1 {
		t.Fatalf("released job must be due now, not_before in %.0fs", j.dueInSecs)
	}
}

// #26 + #25: the stuck-job reaper parks jobs that exhausted their attempts
// as 'failed' (no infinite crash loop), re-queues the others WITHOUT
// promoting deferred jobs to urgent, and the make_interval threshold works.
func TestResetStuckRunningJobsRespectsAttempts(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()

	retry := e.insertJob(t, 1, "running", 1, false, "2 hours", "30 minutes")           // stuck, may retry
	exhausted := e.insertJob(t, 2, "running", maxJobAttempts, true, "0", "30 minutes") // stuck, out of attempts
	alive := e.insertJob(t, 3, "running", 1, false, "0", "5 minutes")                  // heartbeat is fresh

	requeued, failed, err := e.gen.ResetStuckRunningJobs(ctx, stuckJobTimeout, maxJobAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if requeued < 1 || failed < 1 {
		t.Fatalf("requeued=%d failed=%d", requeued, failed)
	}
	r := e.job(t, retry)
	if r.status != "pending" || r.urgent || r.attempts != 1 || r.dueInSecs > 1 {
		t.Fatalf("retryable stuck job: %+v (want pending, NOT urgent, attempts 1, due now)", r)
	}
	if x := e.job(t, exhausted); x.status != "failed" {
		t.Fatalf("job with exhausted attempts must be failed, got %+v", x)
	}
	if a := e.job(t, alive); a.status != "running" {
		t.Fatalf("job updated 5 min ago (< %s) must stay running, got %+v", stuckJobTimeout, a)
	}
}

// #25: FailJob's retry backoff via make_interval(secs => …).
func TestFailJobBackoffInterval(t *testing.T) {
	e := newFlowEnv(t)
	id := e.insertJob(t, 1, "running", 1, true, "0", "0")
	if err := e.gen.FailJob(context.Background(), id, errors.New("boom"), retryDelay, maxJobAttempts); err != nil {
		t.Fatal(err)
	}
	j := e.job(t, id)
	want := retryDelay.Seconds()
	if j.status != "pending" || j.dueInSecs < want-5 || j.dueInSecs > want+1 {
		t.Fatalf("FailJob: %+v, want pending due in ~%.0fs", j, want)
	}
}

// #23: the quality sweep runs on one instance at a time — while one holds
// the advisory lock, the other skips (no duplicate paid AI calls).
func TestQualitySweepSingleInstance(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()

	holding := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := e.gen.TryQualitySweepLock(ctx, func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
		first <- err
	}()
	<-holding

	// "Second instance": its own service on the same database.
	calls := atomic.LoadInt32(&e.ai.calls)
	ran, n, err := e.genSvc.runQualitySweepExclusive(ctx)
	if err != nil || ran || n != 0 {
		t.Fatalf("second instance must skip the sweep: ran=%v n=%d err=%v", ran, n, err)
	}
	if atomic.LoadInt32(&e.ai.calls) != calls {
		t.Fatal("a skipped sweep must not call the AI")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// Lock released -> the sweep runs again.
	ran, _, err = e.genSvc.runQualitySweepExclusive(ctx)
	if err != nil || !ran {
		t.Fatalf("after release the sweep must run: ran=%v err=%v", ran, err)
	}
}

// #27: finishing a personal test deletes ONLY its own orphaned questions:
// a question shared with another test stays, and an unrelated orphan of the
// database (that the old full-table DELETE would have wiped) is untouched.
func TestDeletePersonalTestOnlyOwnOrphans(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Orphans")

	q := func(text string) int64 {
		var id int64
		if err := e.pool.QueryRow(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d, correct_answer, topic)
			VALUES ($1, $2, 'a', 'b', 'c', 'd', 'A', 'Тема') RETURNING id`, e.sid, text).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	test := func(num int, owner any) int64 {
		var id int64
		if err := e.pool.QueryRow(ctx, `
			INSERT INTO tests (subject_id, test_number, title, kind, owner_user_id)
			VALUES ($1, $2, 't', 'personal', $3) RETURNING id`, e.sid, num, owner).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	link := func(testID, qid int64, pos int) {
		if _, err := e.pool.Exec(ctx, `INSERT INTO test_questions (test_id, question_id, position) VALUES ($1, $2, $3)`, testID, qid, pos); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(id int64) bool {
		var ok bool
		_ = e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM questions WHERE id = $1)`, id).Scan(&ok)
		return ok
	}

	own := q("own question")
	shared := q("shared question")
	foreignOrphan := q("orphan of someone else")
	mine := test(9501, u)
	other := test(9502, nil)
	link(mine, own, 1)
	link(mine, shared, 2)
	link(other, shared, 1)
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO user_question_progress (user_id, question_id, status) VALUES ($1, $2, 1), ($1, $3, 1)`,
		u, own, shared); err != nil {
		t.Fatal(err)
	}

	if err := e.gen.DeletePersonalTest(ctx, u, mine, e.sid); err != nil {
		t.Fatal(err)
	}
	if exists(own) {
		t.Fatal("the deleted test's own orphaned question must be deleted")
	}
	if !exists(shared) {
		t.Fatal("a question still used by another test must stay")
	}
	if !exists(foreignOrphan) {
		t.Fatal("an unrelated orphan question of the database must NOT be touched")
	}
	var progress int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_question_progress WHERE user_id = $1 AND question_id = $2`, u, own).Scan(&progress)
	if progress != 0 {
		t.Fatal("progress of the deleted question must cascade away")
	}
}

// #13 (re-check): the unlock guard lives in the SERVICE, not only in the
// Telegram handler. Calling QuizService.ReviveChainTest directly for a
// locked test must not revive / create a generation job (no paid call); an
// unlocked test is revived.
func TestReviveChainTestLockedIsRefusedAtServiceLevel(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Reviver")
	t1 := e.firstChainTest(t, 1)

	// Failed jobs of Тест 2 and Тест 5 (e.g. retries exhausted earlier).
	failed2 := e.insertJob(t, 2, "failed", maxJobAttempts, false, "0", "1 hour")
	failed5 := e.insertJob(t, 5, "failed", maxJobAttempts, false, "0", "1 hour")
	active := func(n int) bool {
		ok, err := e.gen.HasPendingOrRunningChainJob(ctx, e.sid, n)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// The user has not passed Тест 1: everything above 1 is locked.
	e.quiz.ReviveChainTest(ctx, e.sid, 2, u)
	e.quiz.ReviveChainTest(ctx, e.sid, 5, u)
	e.quiz.ReviveChainTest(ctx, e.sid, 100, u)
	if active(2) || active(5) || active(100) {
		t.Fatal("a locked test must not get an active generation job")
	}
	if e.job(t, failed2).status != "failed" || e.job(t, failed5).status != "failed" {
		t.Fatal("failed jobs of locked tests must not be revived")
	}

	// Pass Тест 1 → Тест 2 unlocks → the same call now revives its job.
	e.setStatuses(t, u, t1, 20, 0)
	e.quiz.ReviveChainTest(ctx, e.sid, 2, u)
	if !active(2) {
		t.Fatal("an unlocked missing test must be revived")
	}
	e.quiz.ReviveChainTest(ctx, e.sid, 5, u)
	if active(5) {
		t.Fatal("Тест 5 is still locked")
	}
}

// #16 (re-check): ReplaceQuestionContent rewrites the question and drops the
// old progress in ONE transaction. A failure inside the operation (forced by
// a trigger on the progress delete) must leave NEITHER change behind.
func TestReplaceQuestionContentAtomic(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Atomic")
	t1 := e.firstChainTest(t, 1)
	qs, err := e.subjects.TestQuestions(ctx, t1)
	if err != nil {
		t.Fatal(err)
	}
	target := qs[0]
	e.setStatuses(t, u, t1, 20, 0)

	// Failure injected AFTER the question UPDATE, at the progress DELETE.
	if _, err := e.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION juz_test_fail_progress_delete() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected failure'; END $$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS juz_test_fail ON user_question_progress;
		CREATE TRIGGER juz_test_fail BEFORE DELETE ON user_question_progress
		FOR EACH ROW WHEN (OLD.question_id = `+itoa64(target.ID)+`)
		EXECUTE FUNCTION juz_test_fail_progress_delete();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS juz_test_fail ON user_question_progress;
			DROP FUNCTION IF EXISTS juz_test_fail_progress_delete();`)
	})

	sq := models.SeedQuestion{Text: "Полностью новый текст вопроса?", Options: [4]string{"w", "x", "y", "z"}, Correct: 2, Difficulty: 3}
	err = e.gen.ReplaceQuestionContent(ctx, target.ID, sq)
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	var text, correct string
	var progress int
	if err := e.pool.QueryRow(ctx, `SELECT question_text, correct_answer FROM questions WHERE id = $1`, target.ID).Scan(&text, &correct); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_question_progress WHERE question_id = $1`, target.ID).Scan(&progress)
	if text != target.Text || correct != target.CorrectAnswer {
		t.Fatalf("question was rewritten although the operation failed: %q / %s", text, correct)
	}
	if progress != 1 {
		t.Fatalf("old progress must still be there after the rollback, rows = %d", progress)
	}

	// Without the failure both changes land together.
	_, _ = e.pool.Exec(ctx, `DROP TRIGGER IF EXISTS juz_test_fail ON user_question_progress`)
	if err := e.gen.ReplaceQuestionContent(ctx, target.ID, sq); err != nil {
		t.Fatal(err)
	}
	_ = e.pool.QueryRow(ctx, `SELECT question_text, correct_answer FROM questions WHERE id = $1`, target.ID).Scan(&text, &correct)
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_question_progress WHERE question_id = $1`, target.ID).Scan(&progress)
	if text != sq.Text || correct != "C" || progress != 0 {
		t.Fatalf("successful replace: text=%q correct=%s progress=%d", text, correct, progress)
	}
}

// #18 (re-check): EnsurePersonalTest never deletes a stale personal test
// while THIS test has an in-progress attempt; once the attempt is gone the
// stale test is replaced. Topics are compared via models.NormalizeTopic, so
// «ТЕМА  0» (case + double space) counts as the test's «Тема 0».
func TestEnsurePersonalTestKeepsActiveAttemptAndNormalizes(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Personal")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0", "Тема 1"))

	if _, pending, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid); err != nil || !pending {
		t.Fatalf("expected pending generation: pending=%v err=%v", pending, err)
	}
	e.drain(t)
	pt, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || pt == nil {
		t.Fatalf("personal test: %v", err)
	}
	setStat := func(key, display, recent string) {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent)
			VALUES ($1, $2, $3, $4, 0, 10, $5)
			ON CONFLICT (user_id, subject_id, topic_key) DO UPDATE SET topic = EXCLUDED.topic, recent = EXCLUDED.recent`,
			u, e.sid, key, display, recent); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(id int64) bool {
		var ok bool
		_ = e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tests WHERE id = $1)`, id).Scan(&ok)
		return ok
	}

	// (a) NormalizeTopic: the only weak topic is displayed as «ТЕМА  0» —
	// the test covering «Тема 0» still trains it and must be kept.
	setStat(models.NormalizeTopic("Тема 1"), "Тема 1", "1111111111")
	setStat(models.NormalizeTopic("Тема 0"), "ТЕМА  0", "0000000000")
	if w := e.weak(t, u, e.sid); len(w) != 1 || w[0] != "ТЕМА  0" {
		t.Fatalf("weak topics = %v", w)
	}
	got, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || got == nil || got.ID != pt.ID {
		t.Fatalf("«ТЕМА  0» must match «Тема 0» via NormalizeTopic: got %v err %v", got, err)
	}

	// (b) Make the test stale (its topics mastered, a different topic weak)
	// while an attempt of THIS test is in progress → must not be deleted.
	setStat(models.NormalizeTopic("Тема 0"), "Тема 0", "1111111111")
	setStat(models.NormalizeTopic("Тема 4"), "Тема 4", "0000000000")
	a, err := e.quiz.startTest(ctx, u, pt.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _, err = e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || got == nil || got.ID != pt.ID || !exists(pt.ID) {
		t.Fatalf("a personal test with an active attempt must be kept: got %v err %v", got, err)
	}
	var st string
	_ = e.pool.QueryRow(ctx, `SELECT status FROM test_attempts WHERE id = $1`, a.ID).Scan(&st)
	if st != "in_progress" {
		t.Fatalf("the active attempt must survive, status = %q", st)
	}

	// (c) Attempt gone → the stale test is replaced.
	if _, err := e.pool.Exec(ctx, `UPDATE test_attempts SET status = 'abandoned' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _, err = e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil && got.ID == pt.ID {
		t.Fatal("the stale test must be replaced once no attempt is active")
	}
	if tt, _ := e.gen.FindPersonalTest(ctx, e.sid, u); tt != nil && tt.ID == pt.ID {
		t.Fatal("the stale test must no longer be the user's personal test")
	}
}

// #24: the backfill now runs in the background, possibly after live answers
// already wrote statistics. It must OVERWRITE with the full-history
// aggregate — never double-count those live answers.
func TestBackfillAfterLiveAnswersNoDoubleCount(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "BackfillLive")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0"))
	e.play(t, u, t1, notIn("Тема 2"))
	live, _ := e.gen.TopicStats(ctx, u, e.sid)
	// Marker missing, stats present = "backfill not done yet but live
	// answers already recorded".
	if _, err := e.pool.Exec(ctx, `DELETE FROM app_backfills WHERE name = 'topic_stats_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := e.gen.BackfillTopicStats(ctx); err != nil {
		t.Fatal(err)
	}
	re, _ := e.gen.TopicStats(ctx, u, e.sid)
	if len(re) != len(live) {
		t.Fatalf("topics: %d vs %d", len(re), len(live))
	}
	for i := range live {
		if live[i].Correct != re[i].Correct || live[i].Wrong != re[i].Wrong || live[i].Recent != re[i].Recent {
			t.Fatalf("backfill double-counted live answers: live %+v, after %+v", live[i], re[i])
		}
	}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

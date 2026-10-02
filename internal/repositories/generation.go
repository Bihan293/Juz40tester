package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// GenerationRepository manages the AI test-generation job queue and the
// creation of generated tests.
type GenerationRepository struct {
	pool *pgxpool.Pool
}

func NewGenerationRepository(pool *pgxpool.Pool) *GenerationRepository {
	return &GenerationRepository{pool: pool}
}

// TestProgress holds the knowledge-status breakdown of one test for one
// user (used by the unlock rule and by the 🔒/✅ marks in the grid).
type TestProgress struct {
	Green    int
	Yellow   int
	Red      int
	Total    int
	Answered bool // user has at least one completed attempt of this test
}

// TestProgressForUser aggregates the user's CURRENT knowledge statuses over
// the questions of each given test.
func (r *GenerationRepository) TestProgressForUser(ctx context.Context, userID int64, testIDs []int64) (map[int64]*TestProgress, error) {
	out := make(map[int64]*TestProgress, len(testIDs))
	if len(testIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT tq.test_id,
		       COUNT(*)                                                            AS total,
		       COUNT(*) FILTER (WHERE p.status = 2)                                AS green,
		       COUNT(*) FILTER (WHERE p.status = 1)                                AS yellow,
		       COUNT(*) FILTER (WHERE COALESCE(p.status, 0) = 0)                   AS red,
		       EXISTS(
		           SELECT 1 FROM test_attempts ta
		           WHERE ta.test_id = tq.test_id AND ta.user_id = $1
		             AND ta.status = 'completed'
		       )                                                                   AS answered
		FROM test_questions tq
		LEFT JOIN user_question_progress p
		       ON p.question_id = tq.question_id AND p.user_id = $1
		WHERE tq.test_id = ANY($2)
		GROUP BY tq.test_id`, userID, testIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tp TestProgress
		var testID int64
		if err := rows.Scan(&testID, &tp.Total, &tp.Green, &tp.Yellow, &tp.Red, &tp.Answered); err != nil {
			return nil, err
		}
		out[testID] = &tp
	}
	return out, rows.Err()
}

// WeakTopics returns the user's weak (🔴/🟡) topics of the subject, worst
// first, capped at limit. They are derived from the ACCUMULATED per-topic
// statistics (user_topic_stats: every answer of every test of this subject,
// across attempts and restarts), classified by models.TopicStat.Level — NOT
// from the status of individual questions. A topic becomes weak only after
// systematic mistakes (≥ 2 wrong and < 80% correct among the last 10
// answers), and leaves the list again once the user answers it reliably.
func (r *GenerationRepository) WeakTopics(ctx context.Context, userID, subjectID int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}
	stats, err := r.WeakTopicStats(ctx, userID, subjectID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(stats))
	for _, s := range stats {
		out = append(out, s.Topic)
	}
	return out, nil
}

// WeakTopicStats is WeakTopics with the statistics of each topic (for the
// «🎯 Слабые темы» screen). limit <= 0 returns every weak topic.
func (r *GenerationRepository) WeakTopicStats(ctx context.Context, userID, subjectID int64, limit int) ([]models.TopicStat, error) {
	stats, err := r.TopicStats(ctx, userID, subjectID)
	if err != nil {
		return nil, err
	}
	return models.WeakTopicStats(stats, limit), nil
}

// SubjectsWithWeakTopics returns the ids of subjects in which the user has
// at least one weak topic — EXACTLY the same definition as WeakTopics. The
// «🎯 Слабые темы» picker lists only these subjects.
func (r *GenerationRepository) SubjectsWithWeakTopics(ctx context.Context, userID int64) (map[int64]bool, error) {
	stats, err := r.AllTopicStats(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for _, s := range stats {
		if s.IsWeak() {
			out[s.SubjectID] = true
		}
	}
	return out, nil
}

// scanTest scans a tests row (id, subject_id, test_number, title, is_active,
// kind, topics, owner_user_id, topics_fingerprint) into a models.Test.
// Returns (nil, nil) when the row does not exist.
func scanTest(row pgx.Row) (*models.Test, error) {
	var t models.Test
	var topicsJSON []byte
	var owner sql.NullInt64
	var fingerprint sql.NullString
	err := row.Scan(&t.ID, &t.SubjectID, &t.TestNumber, &t.Title, &t.IsActive, &t.Kind, &topicsJSON, &owner, &fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(topicsJSON) > 0 {
		if err := json.Unmarshal(topicsJSON, &t.Topics); err != nil {
			return nil, err
		}
	}
	t.OwnerUserID = owner.Int64
	t.TopicsFingerprint = fingerprint.String
	return &t, nil
}

const testColumns = `id, subject_id, test_number, title, is_active, kind, topics, owner_user_id, topics_fingerprint`

// FindPersonalTest returns the user's own weak-topics test of the subject,
// or nil if it has not been generated yet.
func (r *GenerationRepository) FindPersonalTest(ctx context.Context, subjectID, userID int64) (*models.Test, error) {
	return scanTest(r.pool.QueryRow(ctx, `
		SELECT `+testColumns+`
		FROM tests
		WHERE subject_id = $1 AND kind = 'personal' AND owner_user_id = $2`,
		subjectID, userID))
}

// FindPersonalTestByFingerprint returns any personal test generated for the
// exact same weak-topics set (sha256 fingerprint), regardless of the owner —
// including ownerless archived TEMPLATES left behind by «🏁 Закончить тест»
// (see DeletePersonalTest), so the cache survives the original owner.
// Users with identical weakness profiles share the same questions: the new
// user gets a fresh CLONE of that test (zero AI cost, zero shared progress).
//
// forUserID: tests of a lineage (the root test and all its clones) that this
// user has ALREADY finished are skipped — the same weak topics must be
// trained with NEW questions, not by repeating the questions the user has
// already seen. Other users still get the free clone.
func (r *GenerationRepository) FindPersonalTestByFingerprint(ctx context.Context, subjectID int64, fingerprint string, forUserID int64) (*models.Test, error) {
	if fingerprint == "" {
		return nil, nil
	}
	return scanTest(r.pool.QueryRow(ctx, `
		SELECT `+testColumns+`
		FROM tests t
		WHERE subject_id = $1 AND kind = 'personal' AND topics_fingerprint = $2
		  AND NOT EXISTS (
		      SELECT 1 FROM user_personal_done d
		      WHERE d.user_id = $3 AND d.root_test_id = COALESCE(t.origin_test_id, t.id))
		ORDER BY id
		LIMIT 1`,
		subjectID, fingerprint, forUserID))
}

// PersonalTestQuestions returns the seed-question payload of a personal test
// (used to clone it for another user with the same weak-topics fingerprint).
func (r *GenerationRepository) PersonalTestQuestions(ctx context.Context, testID int64) ([]models.SeedQuestion, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT q.question_text, q.option_a, q.option_b, q.option_c, q.option_d,
		       q.correct_answer, q.topic, q.difficulty
		FROM test_questions tq
		JOIN questions q ON q.id = tq.question_id
		WHERE tq.test_id = $1
		ORDER BY tq.position`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	labels := map[string]int{"A": 0, "B": 1, "C": 2, "D": 3}
	var out []models.SeedQuestion
	for rows.Next() {
		var sq models.SeedQuestion
		var correct string
		if err := rows.Scan(&sq.Text, &sq.Options[0], &sq.Options[1], &sq.Options[2], &sq.Options[3],
			&correct, &sq.Topic, &sq.Difficulty); err != nil {
			return nil, err
		}
		idx, ok := labels[correct]
		if !ok {
			return nil, fmt.Errorf("test %d: bad correct_answer %q", testID, correct)
		}
		sq.Correct = idx
		out = append(out, sq)
	}
	return out, rows.Err()
}

// DeletePersonalTest removes the user's personal weak-topics test ("🏁
// Закончить тест", or a stale test whose topics are all mastered). The next
// weak-topics run then builds a fresh test from the CURRENT weak topics.
//
// Sharing guarantee: the test content is the cache that lets the next
// student with the SAME weak-topics fingerprint get the same test for free
// (a clone, no AI call). Deleting the last copy used to throw that cache
// away — after the first student finished, the next one with identical weak
// topics paid for a brand-new generation. So when no other test of the
// subject carries the same fingerprint, the test is ARCHIVED as an ownerless,
// inactive TEMPLATE instead: the user's attempts and progress on it are
// removed (their weak topics no longer count those questions), the
// questions and their cached Kazakh translations stay for future clones.
// Otherwise (another copy still exists) the test is deleted outright.
//
// subjectID pins the operation to the subject the caller resolved the test
// in, so a stale callback can never touch a test of another subject.
func (r *GenerationRepository) DeletePersonalTest(ctx context.Context, userID, testID, subjectID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ownership + kind + subject guard: only the owner can finish their own
	// personal test of THIS subject.
	var fingerprint sql.NullString
	var rootID int64
	err = tx.QueryRow(ctx, `
		SELECT topics_fingerprint, COALESCE(origin_test_id, id) FROM tests
		WHERE id = $1 AND owner_user_id = $2 AND kind = 'personal' AND subject_id = $3
		FOR UPDATE`, testID, userID, subjectID).Scan(&fingerprint, &rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// The user has seen this content: their next weak-topics test must get
	// NEW questions even when the weak-topics set is the same (see
	// FindPersonalTestByFingerprint). The per-topic statistics of the
	// practice stay in user_topic_stats — they are never deleted here.
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_personal_done (user_id, root_test_id, subject_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, userID, rootID, subjectID); err != nil {
		return err
	}

	keepTemplate := false
	if fingerprint.Valid && fingerprint.String != "" {
		var others bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM tests
				WHERE subject_id = $1 AND kind = 'personal'
				  AND topics_fingerprint = $2 AND id <> $3
			)`, subjectID, fingerprint.String, testID).Scan(&others); err != nil {
			return err
		}
		keepTemplate = !others
	}

	if keepTemplate {
		// The user's own trace of the test goes away (attempts cascade to
		// attempt_questions; per-question progress rows are dropped). The
		// per-TOPIC statistics are kept in user_topic_stats ...
		if _, err := tx.Exec(ctx, `
			DELETE FROM test_attempts WHERE test_id = $1 AND user_id = $2`, testID, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM user_question_progress
			WHERE user_id = $2
			  AND question_id IN (SELECT question_id FROM test_questions WHERE test_id = $1)`,
			testID, userID); err != nil {
			return err
		}
		// ... while the content stays as an ownerless, hidden template.
		if _, err := tx.Exec(ctx, `
			UPDATE tests SET owner_user_id = NULL, is_active = FALSE WHERE id = $1`, testID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM tests WHERE id = $1`, testID); err != nil {
		return err
	}
	// Orphaned questions of the deleted test (not referenced by any other
	// test) go away together with their progress rows.
	if _, err := tx.Exec(ctx, `
		DELETE FROM user_question_progress
		WHERE question_id IN (
		    SELECT q.id FROM questions q
		    WHERE NOT EXISTS (SELECT 1 FROM test_questions tq WHERE tq.question_id = q.id)
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM questions
		WHERE NOT EXISTS (SELECT 1 FROM test_questions tq WHERE tq.question_id = questions.id)`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnqueueChainJob registers a job to generate chain test number testNumber
// of the subject. Idempotent: the partial unique index on
// (subject_id, test_number) for active jobs makes duplicates a no-op.
// urgent jobs bypass the off-peak deferral (the user is waiting for this
// test); ownerUserID carries whose knowledge marks the generator should
// personalise against (0 = none). Returns true when a NEW job row was
// inserted.
//
// When an active (pending/running) job for the same test already exists and
// the new request is urgent while the existing job is NOT (it was deferred
// to the off-peak window), the existing job is upgraded: urgent = TRUE and
// not_before = now(). Without that upgrade a test that became openable
// AFTER its job had been deferred would keep the user waiting for the
// off-peak window in ⏳ «Минуточку...» — hours for a test they can already
// open. A 'done' row is never touched: the conflict update's WHERE clause
// excludes it, so the unique index keeps blocking a paid regeneration.
func (r *GenerationRepository) EnqueueChainJob(ctx context.Context, subjectID int64, testNumber int, notBefore time.Time, urgent bool, ownerUserID ...int64) (bool, error) {
	var owner any
	if len(ownerUserID) > 0 && ownerUserID[0] > 0 {
		owner = ownerUserID[0]
	}
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, test_number, not_before, urgent, owner_user_id)
		VALUES ('chain', $1, $2, $3, $4, $5)
		ON CONFLICT (subject_id, test_number) WHERE kind = 'chain' AND status IN ('pending','running')
		DO UPDATE SET
			urgent     = TRUE,
			not_before = now(),
			updated_at = now()
		WHERE generation_jobs.status = 'pending'
		  AND NOT generation_jobs.urgent
		  AND EXCLUDED.urgent`, subjectID, testNumber, notBefore, urgent, owner)
	return tag.RowsAffected() > 0, err
}

// EnqueueChainJobNow is EnqueueChainJob with not_before = now() and
// urgent = true: the job is due immediately, ignoring the off-peak
// deferral. Used to bootstrap the chain (Тест 1) on a fresh database so the
// user never faces an empty grid.
func (r *GenerationRepository) EnqueueChainJobNow(ctx context.Context, subjectID int64, testNumber int) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, test_number, not_before, urgent)
		VALUES ('chain', $1, $2, now(), TRUE)
		ON CONFLICT DO NOTHING`, subjectID, testNumber)
	return tag.RowsAffected() > 0, err
}

// EnqueuePersonalJob registers an URGENT job to generate the user's personal
// weak-topics test of the subject. The fingerprint pins the exact weak-topics
// set the test is generated for (so identical profiles share one generation).
// Idempotent via the partial unique index.
func (r *GenerationRepository) EnqueuePersonalJob(ctx context.Context, subjectID, userID int64, fingerprint string) error {
	var fp any
	if fingerprint != "" {
		fp = fingerprint
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, owner_user_id, topics_fingerprint, not_before, urgent)
		VALUES ('personal', $1, $2, $3, now(), TRUE)
		ON CONFLICT DO NOTHING`, subjectID, userID, fp)
	return err
}

// ReviveChainJob re-queues a FAILED chain job (a generation that exhausted
// its retries) as a fresh urgent pending job. Triggered when the user taps
// the ⏳ button of a test that never appeared: without this path a failed
// job left the test in «Минуточку...» forever, because nobody ever
// re-enqueued it (the unique index only blocks active rows, so flipping a
// failed row back to pending is safe and race-free). The retry counters are
// reset — the failure cause (e.g. an API outage) may be long gone, and the
// user is actively waiting. Returns true when a failed job was revived.
func (r *GenerationRepository) ReviveChainJob(ctx context.Context, subjectID int64, testNumber int, ownerUserID int64) (bool, error) {
	var owner any
	if ownerUserID > 0 {
		owner = ownerUserID
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs
		SET status = 'pending', urgent = TRUE, not_before = now(),
		    attempts = 0, last_error = '', owner_user_id = COALESCE($3, owner_user_id),
		    updated_at = now()
		WHERE kind = 'chain' AND subject_id = $1 AND test_number = $2
		  AND status = 'failed'`,
		subjectID, testNumber, owner)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// TouchRunningJob refreshes the heartbeat of a running job. The worker pings
// it while a (minutes-long) generation is in flight so the stuck-job reaper
// never mistakes a slow-but-alive job for a dead one and double-generates a
// paid test.
func (r *GenerationRepository) TouchRunningJob(ctx context.Context, jobID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs SET updated_at = now()
		WHERE id = $1 AND status = 'running'`, jobID)
	return err
}

// ResetStuckRunningJobs returns 'running' jobs that have not been updated
// for longer than stuckFor back to 'pending'. A job stays 'running' forever
// when the worker crashes mid-generation (deploy, OOM, restart) — without
// this reaper the affected tests would hang in ⏳ «Минуточку...» for good.
// The re-queued job is marked URGENT (not_before = now()): the worker only
// abandons jobs it was actively processing, so the job is by definition
// already wanted; leaving the old deferral would send a recovered job to
// the BACK of the off-peak queue instead of the head.
func (r *GenerationRepository) ResetStuckRunningJobs(ctx context.Context, stuckFor time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs
		SET status = 'pending', urgent = TRUE, not_before = now(), updated_at = now()
		WHERE status = 'running' AND updated_at < now() - $1::interval`,
		stuckFor.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// HasPendingOrRunningChainJob reports whether an active (pending/running)
// chain job already exists for (subject, test number).
func (r *GenerationRepository) HasPendingOrRunningChainJob(ctx context.Context, subjectID int64, testNumber int) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM generation_jobs
		              WHERE kind = 'chain' AND subject_id = $1 AND test_number = $2
		                AND status IN ('pending','running'))`, subjectID, testNumber).Scan(&exists)
	return exists, err
}

// HasPendingOrRunningPersonalJob reports whether an active personal job
// already exists for (subject, user).
func (r *GenerationRepository) HasPendingOrRunningPersonalJob(ctx context.Context, subjectID, userID int64) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM generation_jobs
		              WHERE kind = 'personal' AND subject_id = $1 AND owner_user_id = $2
		                AND status IN ('pending','running'))`, subjectID, userID).Scan(&exists)
	return exists, err
}

// ClaimNextJob atomically picks a due pending job and marks it running:
// urgent jobs first, then deferred jobs whose time has come (off-peak
// pre-generation). EVERY job honours not_before: urgent jobs are enqueued
// with not_before = now() so they still run right away, while the retry
// backoff set by FailJob (not_before = now() + retryDelay) now applies to
// urgent jobs too — previously an urgent job burned all its retries within
// a minute while a provider was down. Returns nil when nothing is due.
func (r *GenerationRepository) ClaimNextJob(ctx context.Context) (*models.GenerationJob, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var j models.GenerationJob
	var testNumber sql.NullInt64
	var fingerprint sql.NullString
	var owner sql.NullInt64
	err = tx.QueryRow(ctx, `
		SELECT id, kind, subject_id, test_number, topics_fingerprint, owner_user_id, status, attempts, urgent
		FROM generation_jobs
		WHERE status = 'pending' AND not_before <= now()
		ORDER BY urgent DESC, id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).
		Scan(&j.ID, &j.Kind, &j.SubjectID, &testNumber, &fingerprint, &owner, &j.Status, &j.Attempts, &j.Urgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.TestNumber = int(testNumber.Int64)
	j.TopicsFingerprint = fingerprint.String
	j.OwnerUserID = owner.Int64

	if _, err := tx.Exec(ctx, `
		UPDATE generation_jobs
		SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = $1`, j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	j.Attempts++
	return &j, nil
}

// CompleteJob marks the job done and links the produced test.
func (r *GenerationRepository) CompleteJob(ctx context.Context, jobID, testID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs
		SET status = 'done', test_id = $2, last_error = '', updated_at = now()
		WHERE id = $1`, jobID, testID)
	return err
}

// FailJob records the failure. After maxAttempts the job is parked as
// 'failed' (and can be re-queued by a fresh Enqueue because the partial
// unique index ignores failed rows).
func (r *GenerationRepository) FailJob(ctx context.Context, jobID int64, jobErr error, retryDelay time.Duration, maxAttempts int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'pending' END,
		    last_error = $3,
		    not_before = CASE WHEN attempts >= $2 THEN not_before ELSE now() + $4::interval END,
		    updated_at = now()
		WHERE id = $1`,
		jobID, maxAttempts, fmt.Sprintf("%v", jobErr), retryDelay.String())
	return err
}

// CreateGeneratedTest atomically inserts a generated test with its questions
// and links. For 'personal' tests the (subject, owner) unique index
// deduplicates concurrent inserts: the loser of the race re-reads the
// winner's test instead of failing. A personal test with test_number = 0
// gets the next free personal number (9000+), keeping it far above the
// chain. The UNIQUE (subject_id, test_number) constraint also guards chain
// tests against concurrent generation of the same number.
// personalLockNS is the advisory-lock namespace of personal-test creation.
const personalLockNS int32 = 40_001

func (r *GenerationRepository) CreateGeneratedTest(ctx context.Context, test *models.Test, questions []models.SeedQuestion) (*models.Test, error) {
	if len(questions) == 0 {
		return nil, errors.New("generated test has no questions")
	}
	topicsJSON, err := json.Marshal(test.Topics)
	if err != nil {
		return nil, err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if test.Kind == models.TestKindPersonal {
		// Serialise personal-test creation per subject. Without the lock two
		// concurrent creations (two users at once, or a double tap) read the
		// same MAX(test_number)+1; the loser hit ON CONFLICT (subject_id,
		// test_number) below and was handed the WINNER's test — another
		// user's personal test. The lock is released on commit/rollback.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, personalLockNS, int32(test.SubjectID)); err != nil {
			return nil, err
		}
		// One personal test per (subject, owner): a concurrent creation for
		// the same owner already won — return that test instead of failing
		// on the idx_tests_personal_owner unique index.
		if test.OwnerUserID > 0 {
			existing, err := scanTest(tx.QueryRow(ctx, `
				SELECT `+testColumns+`
				FROM tests
				WHERE subject_id = $1 AND kind = 'personal' AND owner_user_id = $2`,
				test.SubjectID, test.OwnerUserID))
			if err != nil {
				return nil, err
			}
			if existing != nil {
				return existing, nil
			}
		}
	}
	if test.Kind == models.TestKindPersonal && test.TestNumber == 0 {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(test_number), 8999) + 1
			FROM tests WHERE subject_id = $1 AND kind IN ('personal','weak')`, test.SubjectID).
			Scan(&test.TestNumber); err != nil {
			return nil, err
		}
	}

	var owner any
	if test.OwnerUserID > 0 {
		owner = test.OwnerUserID
	}
	var fingerprint any
	if test.TopicsFingerprint != "" {
		fingerprint = test.TopicsFingerprint
	}
	var origin any
	if test.OriginTestID > 0 {
		origin = test.OriginTestID
	}
	var testID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO tests (subject_id, test_number, title, is_active, kind, topics, owner_user_id, topics_fingerprint, origin_test_id)
		VALUES ($1, $2, $3, TRUE, $4, $5, $6, $7, $8)
		ON CONFLICT (subject_id, test_number) DO NOTHING
		RETURNING id`,
		test.SubjectID, test.TestNumber, test.Title, test.Kind, topicsJSON, owner, fingerprint, origin).
		Scan(&testID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Conflict — the test already exists (concurrent generation). Re-read
		// and return it instead of writing a duplicate.
		if err := tx.Rollback(ctx); err != nil {
			return nil, err
		}
		existing, err := scanTest(r.pool.QueryRow(ctx, `
			SELECT `+testColumns+`
			FROM tests WHERE subject_id = $1 AND test_number = $2`,
			test.SubjectID, test.TestNumber))
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, errors.New("generated test conflict but no existing row found")
		}
		return existing, nil
	}
	if err != nil {
		return nil, err
	}

	labels := []string{"A", "B", "C", "D"}
	for i, sq := range questions {
		if sq.Correct < 0 || sq.Correct > 3 {
			return nil, fmt.Errorf("question %d: invalid correct index %d", i+1, sq.Correct)
		}
		var qid int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
			                       correct_answer, topic, difficulty)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING id`,
			test.SubjectID, sq.Text, sq.Options[0], sq.Options[1], sq.Options[2], sq.Options[3],
			labels[sq.Correct], sq.Topic, sq.Difficulty).Scan(&qid); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO test_questions (test_id, question_id, position)
			VALUES ($1, $2, $3)`, testID, qid, i+1); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	test.ID = testID
	return test, nil
}

// ClonePersonalTest creates a copy of an existing personal test for another
// owner (same subject, same questions payload, fresh question rows so the
// clone has no shared progress). This is how users with IDENTICAL weak-topic
// fingerprints get the SAME test content without a paid AI generation.
// Returns the new test, or the already-existing one when the owner already
// has a personal test (unique index race).
func (r *GenerationRepository) ClonePersonalTest(ctx context.Context, src *models.Test, questions []models.SeedQuestion, ownerUserID int64) (*models.Test, error) {
	if len(questions) == 0 {
		return nil, errors.New("clone source test has no questions")
	}
	clone := &models.Test{
		SubjectID:         src.SubjectID,
		TestNumber:        0, // next free personal number (assigned inside)
		Title:             src.Title,
		Kind:              models.TestKindPersonal,
		Topics:            src.Topics,
		OwnerUserID:       ownerUserID,
		TopicsFingerprint: src.TopicsFingerprint,
	}
	// Lineage: every clone points at the ROOT (originally generated) test.
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(origin_test_id, id) FROM tests WHERE id = $1`, src.ID).Scan(&clone.OriginTestID); err != nil {
		return nil, err
	}
	return r.CreateGeneratedTest(ctx, clone, questions)
}

// UncheckedQuestions returns questions that have not passed the quality
// audit yet (oldest first). Questions whose repair failed too many times
// are skipped (quality_attempts cap is applied by NoteQualityAttempt).
func (r *GenerationRepository) UncheckedQuestions(ctx context.Context, limit int) ([]models.Question, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, subject_id, question_text, option_a, option_b, option_c,
		       option_d, correct_answer, topic, difficulty
		FROM questions
		WHERE quality_checked_at IS NULL
		  AND (quality_postponed_until IS NULL OR quality_postponed_until < now())
		ORDER BY id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Question
	for rows.Next() {
		var q models.Question
		if err := rows.Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB, &q.OptionC,
			&q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// MarkQuestionsChecked stamps questions as audited and clean.
func (r *GenerationRepository) MarkQuestionsChecked(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE questions SET quality_checked_at = now() WHERE id = ANY($1)`, ids)
	return err
}

// NoteQualityAttempt records a failed repair of a flagged question. After
// maxAttempts failures the question is stamped as checked (left as is) so
// the sweep stops paying for it.
func (r *GenerationRepository) NoteQualityAttempt(ctx context.Context, id int64, maxAttempts int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE questions
		SET quality_attempts = quality_attempts + 1,
		    quality_checked_at = CASE WHEN quality_attempts + 1 >= $2 THEN now() ELSE NULL END
		WHERE id = $1`, id, maxAttempts)
	return err
}

// questionBusySQL: the question is on screen (unanswered) in an
// in-progress attempt.
const questionBusySQL = `
	SELECT EXISTS (
		SELECT 1 FROM attempt_questions aq
		JOIN test_attempts a ON a.id = aq.attempt_id
		WHERE aq.question_id = $1 AND NOT aq.answered AND a.status = 'in_progress'
	)`

// IsQuestionBusy reports whether the question is shown in an unanswered
// position of an in-progress attempt. The quality sweep checks it BEFORE
// paying the model for a rewrite (ReplaceQuestionContent would refuse it).
func (r *GenerationRepository) IsQuestionBusy(ctx context.Context, id int64) (bool, error) {
	var busy bool
	err := r.pool.QueryRow(ctx, questionBusySQL, id).Scan(&busy)
	return busy, err
}

// PostponeQualityCheck hides a busy question from the quality sweep for the
// given delay (no AI call is made for it until then).
func (r *GenerationRepository) PostponeQualityCheck(ctx context.Context, id int64, delay time.Duration) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE questions SET quality_postponed_until = now() + $2::interval
		WHERE id = $1`, id, delay.String())
	return err
}

// ErrQuestionBusy: the question is shown in an unanswered position of an
// in-progress attempt — rewriting it now would change the text/key under a
// student who is looking at it. The sweep retries later.
var ErrQuestionBusy = errors.New("question is in an active attempt")

// ReplaceQuestionContent rewrites a flagged question in place (same id, so
// tests, attempts history and the user's 🔴🟡🟢 progress stay intact) and
// drops its cached translations (they described the old text and are
// produced again on the next Kazakh open). Refuses with ErrQuestionBusy
// while the question is on screen in an unfinished attempt.
func (r *GenerationRepository) ReplaceQuestionContent(ctx context.Context, id int64, sq models.SeedQuestion) error {
	if sq.Correct < 0 || sq.Correct > 3 {
		return fmt.Errorf("question %d: invalid correct index %d", id, sq.Correct)
	}
	labels := []string{"A", "B", "C", "D"}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var busy bool
	if err := tx.QueryRow(ctx, questionBusySQL, id).Scan(&busy); err != nil {
		return err
	}
	if busy {
		return ErrQuestionBusy
	}
	if _, err := tx.Exec(ctx, `
		UPDATE questions
		SET question_text = $2, option_a = $3, option_b = $4, option_c = $5, option_d = $6,
		    correct_answer = $7, difficulty = $8, quality_checked_at = now()
		WHERE id = $1`,
		id, sq.Text, sq.Options[0], sq.Options[1], sq.Options[2], sq.Options[3],
		labels[sq.Correct], sq.Difficulty); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM question_translations WHERE question_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

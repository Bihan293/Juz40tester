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

	"github.com/Bihan293/Juz40-test2/internal/models"
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

// WeakTopics returns the topics the user struggles with in a subject: topics
// of ANSWERED questions whose knowledge status is 🔴/🟡 (not mastered),
// weighted by how badly they perform. The list is sorted by weakness (worst
// first) and capped at limit.
//
// Only questions the user has actually answered are considered (a progress
// row exists only after an answer). Never-answered questions must NOT count
// as weak — otherwise every untouched topic of every freshly generated test
// would look "weak", and the weak-topics set would be huge and identical
// for everyone.
//
// Mastered topics disappear from the result automatically the moment all of
// their questions reach 🟢 — that is the "слабая тема удаляется" behaviour:
// the weak-topics list is always derived from the CURRENT statuses, never
// stored as a static snapshot.
func (r *GenerationRepository) WeakTopics(ctx context.Context, userID, subjectID int64, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := r.pool.Query(ctx, `
		SELECT q.topic,
		       COUNT(*) FILTER (WHERE p.status < 2)                           AS weak_count,
		       COALESCE(SUM(p.wrong_count), 0)                                AS wrongs
		FROM questions q
		JOIN user_question_progress p
		     ON p.question_id = q.id AND p.user_id = $1
		WHERE q.subject_id = $2 AND q.topic <> ''
		GROUP BY q.topic
		HAVING COUNT(*) FILTER (WHERE p.status < 2) > 0
		ORDER BY weak_count DESC, wrongs DESC, q.topic
		LIMIT $3`, userID, subjectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var topic string
		var weakCount, wrongs int
		if err := rows.Scan(&topic, &weakCount, &wrongs); err != nil {
			return nil, err
		}
		out = append(out, topic)
	}
	return out, rows.Err()
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
// exact same weak-topics set (sha256 fingerprint), regardless of the owner.
// Users with identical weakness profiles share the same questions: the new
// user gets a fresh CLONE of that test (zero AI cost, zero shared progress).
func (r *GenerationRepository) FindPersonalTestByFingerprint(ctx context.Context, subjectID int64, fingerprint string) (*models.Test, error) {
	if fingerprint == "" {
		return nil, nil
	}
	return scanTest(r.pool.QueryRow(ctx, `
		SELECT `+testColumns+`
		FROM tests
		WHERE subject_id = $1 AND kind = 'personal' AND topics_fingerprint = $2
		ORDER BY id
		LIMIT 1`,
		subjectID, fingerprint))
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

// DeletePersonalTest removes the user's personal weak-topics test together
// with its questions and the user's progress on them (attempts cascade).
// The questions are deleted only when no other test still references them.
// Used by "Закончить тест": the next weak-topics run then generates a fresh
// test from scratch. subjectID pins the deletion to the subject the caller
// resolved the test in, so a stale callback can never delete a test of
// another subject that reused the id.
func (r *GenerationRepository) DeletePersonalTest(ctx context.Context, userID, testID, subjectID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ownership + kind + subject guard: only the owner can delete their own
	// personal test of THIS subject.
	tag, err := tx.Exec(ctx, `
		DELETE FROM tests
		WHERE id = $1 AND owner_user_id = $2 AND kind = 'personal' AND subject_id = $3`, testID, userID, subjectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
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
// urgent jobs first (they ignore not_before — a user is waiting), then
// deferred jobs whose time has come (off-peak pre-generation). Returns nil
// when nothing is due.
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
		WHERE status = 'pending' AND (urgent OR not_before <= now())
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
	var testID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO tests (subject_id, test_number, title, is_active, kind, topics, owner_user_id, topics_fingerprint)
		VALUES ($1, $2, $3, TRUE, $4, $5, $6, $7)
		ON CONFLICT (subject_id, test_number) DO NOTHING
		RETURNING id`,
		test.SubjectID, test.TestNumber, test.Title, test.Kind, topicsJSON, owner, fingerprint).
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
	return r.CreateGeneratedTest(ctx, clone, questions)
}

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// GenerationRepository manages the AI test-generation job queue and the
// creation of generated tests.
type GenerationRepository struct {
	pool *pgxpool.Pool
	// lockURL: DIRECT connection string for session-level advisory locks
	// (TryExclusive). Empty = a connection hijacked from the pool (fine when
	// DATABASE_URL itself is direct, e.g. tests).
	lockURL string
}

// WithLockURL sets the DIRECT (non-pooler) connection string used for
// session-level advisory locks.
func (r *GenerationRepository) WithLockURL(url string) *GenerationRepository {
	r.lockURL = url
	return r
}

func NewGenerationRepository(pool *pgxpool.Pool) *GenerationRepository {
	return &GenerationRepository{pool: pool}
}

// TestProgress holds the knowledge-status breakdown of one test for one
// user (used by the unlock rule and by the 🔒/✅ marks in the grid).
type TestProgress struct {
	Green  int
	Yellow int
	Red    int
	Total  int
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
		       COUNT(*) FILTER (WHERE COALESCE(p.status, 0) = 0)                   AS red
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
		if err := rows.Scan(&testID, &tp.Total, &tp.Green, &tp.Yellow, &tp.Red); err != nil {
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

// WeakTopicKeys (B2) returns the catalog topic_keys of the user's weak
// topics, worst first (ties broken by topic_key — the set does not jump
// from one answer to the next).
func (r *GenerationRepository) WeakTopicKeys(ctx context.Context, userID, subjectID int64, limit int) ([]string, error) {
	stats, err := r.WeakTopicStats(ctx, userID, subjectID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(stats))
	for _, s := range stats {
		out = append(out, s.Key)
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

// scanTest scans a tests row (id, subject_id, test_number, title, is_active,
// kind, topics, owner_user_id, topics_fingerprint, from_bank) into a models.Test.
// Returns (nil, nil) when the row does not exist.
func scanTest(row pgx.Row) (*models.Test, error) {
	var t models.Test
	var topicsJSON []byte
	var owner sql.NullInt64
	var fingerprint sql.NullString
	err := row.Scan(&t.ID, &t.SubjectID, &t.TestNumber, &t.Title, &t.IsActive, &t.Kind, &topicsJSON, &owner, &fingerprint, &t.FromBank)
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

const testColumns = `id, subject_id, test_number, title, is_active, kind, topics, owner_user_id, topics_fingerprint, from_bank`

// FindPersonalTest returns the user's own weak-topics test of the subject,
// or nil if it has not been generated yet.
func (r *GenerationRepository) FindPersonalTest(ctx context.Context, subjectID, userID int64) (*models.Test, error) {
	return scanTest(r.pool.QueryRow(ctx, `
		SELECT `+testColumns+`
		FROM tests
		WHERE subject_id = $1 AND kind = 'personal' AND owner_user_id = $2`,
		subjectID, userID))
}

// PersonalTestSubjects returns, among subjectIDs, the subjects in which the
// user has a personal weak-topics test (one query for the weak-topics menu).
func (r *GenerationRepository) PersonalTestSubjects(ctx context.Context, userID int64, subjectIDs []int64) (map[int64]bool, error) {
	out := make(map[int64]bool)
	if len(subjectIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT subject_id FROM tests
		WHERE subject_id = ANY($2) AND kind = 'personal' AND owner_user_id = $1`, userID, subjectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PersonalTestQuestions returns the seed-question payload of a personal test
// (used by tests to compare the content of personal tests).
func (r *GenerationRepository) PersonalTestQuestions(ctx context.Context, testID int64) ([]models.SeedQuestion, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT q.question_text, q.option_a, q.option_b, q.option_c, q.option_d,
		       q.correct_answer, q.topic, q.difficulty, q.quality_checked_at IS NOT NULL
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
			&correct, &sq.Topic, &sq.Difficulty, &sq.QualityChecked); err != nil {
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
// B6a: no templates / fingerprint cache any more — weak-topics tests are
// assembled from the shared question bank; "already seen" is a row in
// user_question_progress. A generated (non-bank) personal test is deleted
// with its questions that no other test references.
//
// subjectID pins the operation to the subject the caller resolved the test
// in, so a stale callback can never touch a test of another subject.
func (r *GenerationRepository) DeletePersonalTest(ctx context.Context, userID, testID, subjectID int64) error {
	return deleteOwnedTest(ctx, r.pool, userID, testID, subjectID, models.TestKindPersonal)
}

// deleteOwnedTest deletes a per-user test of the given kind (personal /
// custom) — see DeletePersonalTest.
func deleteOwnedTest(ctx context.Context, pool *pgxpool.Pool, userID, testID, subjectID int64, kind string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ownership + kind + subject guard: only the owner can finish their own
	// test of THIS subject.
	var fromBank bool
	err = tx.QueryRow(ctx, `
		SELECT from_bank FROM tests
		WHERE id = $1 AND owner_user_id = $2 AND kind = $4 AND subject_id = $3
		FOR UPDATE`, testID, userID, subjectID, kind).Scan(&fromBank)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if fromBank {
		// B3: a bank test owns no questions — they are shared bank rows
		// (chain tests, other users' bank tests). Only the test row goes
		// away (attempts cascade); the user's per-question progress stays:
		// it is what keeps solved questions out of the next bank test.
		if _, err := tx.Exec(ctx, `DELETE FROM tests WHERE id = $1`, testID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	// Collect the questions of THIS test before the test row (and with it,
	// via ON DELETE CASCADE, its test_questions links) goes away. Only these
	// questions may become orphans — the old full-table
	// «DELETE FROM questions WHERE NOT EXISTS (test_questions)» scanned every
	// question of the database on each finished personal test.
	var qids []int64
	rows, err := tx.Query(ctx, `SELECT question_id FROM test_questions WHERE test_id = $1`, testID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		qids = append(qids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tests WHERE id = $1`, testID); err != nil {
		return err
	}
	if len(qids) > 0 {
		// The user's progress on questions that SURVIVE the delete (shared
		// with a clone or linked into another student's bank test) is kept,
		// exactly like for a bank test above: it is what keeps the solved
		// questions out of the user's next weak-topics test. Dropping it
		// (the old R-8b behaviour) made the bank hand the same, already
		// mastered questions out again as «new». The progress of questions
		// deleted below goes away with them (ON DELETE CASCADE).
		//
		// Questions of the deleted test that no other test references go
		// away; their progress / attempt rows cascade. Questions still linked
		// to another test are left untouched.
		if _, err := tx.Exec(ctx, `
			DELETE FROM questions q
			WHERE q.id = ANY($1::bigint[])
			  AND NOT EXISTS (SELECT 1 FROM test_questions tq WHERE tq.question_id = q.id)`, qids); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Chain revive backoff: a chain job that spent all its attempts is parked
// as 'failed'. The FIRST re-queue of such a job is allowed at once (the
// cause — a provider outage — may already be gone); every further one waits
// ChainReviveBaseBackoff·2^(revivals-1) after the last failure, capped at
// ChainReviveMaxBackoff (30 min, 1 h, 2 h, 4 h, 8 h, 12 h, 12 h …). Without
// it every open of the subject screen re-queued an urgent generation of a
// missing openable test, so a test whose generation kept failing started a
// new round of paid attempts on every open.
const (
	ChainReviveBaseBackoff = 30 * time.Minute
	ChainReviveMaxBackoff  = 12 * time.Hour
)

// chainReviveCooldownSQL is the pause (seconds) a failed chain job with
// `revivals` earlier re-queues must wait after its last failure. $base and
// $max are the parameter placeholders of the two backoff constants.
func chainReviveCooldownSQL(revivals, base, max string) string {
	return `(CASE WHEN ` + revivals + ` <= 0 THEN 0
	         ELSE LEAST(` + base + `::float8 * power(2, LEAST(` + revivals + ` - 1, 30)), ` + max + `::float8) END)`
}

// reviveFailedChainSQL flips the NEWEST failed chain job of the slot back to
// pending (attempts reset, revivals + 1) when its backoff has elapsed and no
// active or done job holds the slot. The row keeps its id, so the batches a
// previous attempt already paid for (gen_job_batches) are reused.
// $1 subject, $2 test number, $3 urgent, $4 not_before, $5 owner (NULL =
// keep), $6/$7 backoff base/max seconds.
var reviveFailedChainSQL = `
	UPDATE generation_jobs
	SET status = 'pending', urgent = $3, not_before = $4,
	    attempts = 0, last_error = '', revivals = revivals + 1,
	    owner_user_id = COALESCE($5, owner_user_id), updated_at = now()
	WHERE id = (
		SELECT id FROM generation_jobs
		WHERE kind = 'chain' AND subject_id = $1 AND test_number = $2
		  AND status = 'failed'
		ORDER BY id DESC LIMIT 1)
	  AND status = 'failed'
	  AND updated_at <= now() - make_interval(secs => ` + chainReviveCooldownSQL("revivals", "$6", "$7") + `)
	  AND NOT EXISTS (
		SELECT 1 FROM generation_jobs
		WHERE kind = 'chain' AND subject_id = $1 AND test_number = $2
		  AND status IN ('pending','running','done'))`

// ChainRetryAt reports when the newest FAILED chain job of the slot may be
// re-queued again (zero time: no failed job, or it may be re-queued now).
func (r *GenerationRepository) ChainRetryAt(ctx context.Context, subjectID int64, testNumber int) (time.Time, error) {
	var at *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT CASE WHEN until > now() THEN until END
		FROM (
			SELECT updated_at + make_interval(secs => `+chainReviveCooldownSQL("revivals", "$3", "$4")+`) AS until
			FROM generation_jobs
			WHERE kind = 'chain' AND subject_id = $1 AND test_number = $2 AND status = 'failed'
			ORDER BY id DESC LIMIT 1) f`,
		subjectID, testNumber, ChainReviveBaseBackoff.Seconds(), ChainReviveMaxBackoff.Seconds()).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) || at == nil {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return *at, nil
}

// EnqueueChainJob registers a job to generate chain test number testNumber
// of the subject. Idempotent: the partial unique index on
// (subject_id, test_number) for active jobs makes duplicates a no-op.
// urgent jobs bypass the off-peak deferral (the user is waiting for this
// test); ownerUserID carries whose knowledge marks the generator should
// personalise against (0 = none). Returns true when a job was queued
// (a NEW row, or a failed row revived).
//
// When an active (pending/running) job for the same test already exists and
// the new request is urgent while the existing job is NOT (it was deferred
// to the off-peak window), the existing job is upgraded: urgent = TRUE and
// not_before = now(). Without that upgrade a test that became openable
// AFTER its job had been deferred would keep the user waiting for the
// off-peak window in ⏳ «Минуточку...» — hours for a test they can already
// open. A 'done' row is never touched: the conflict update's WHERE clause
// excludes it, so the unique index keeps blocking a paid regeneration.
//
// A slot whose last job FAILED (all attempts spent) is not given a fresh
// row: the failed job itself is revived — keeping the batches it already
// paid for — and only once its backoff has elapsed (ChainReviveBaseBackoff).
// While the backoff runs nothing is queued and false is returned.
func (r *GenerationRepository) EnqueueChainJob(ctx context.Context, subjectID int64, testNumber int, notBefore time.Time, urgent bool, ownerUserID int64) (bool, error) {
	var owner any
	if ownerUserID > 0 {
		owner = ownerUserID
	}
	tag, err := r.pool.Exec(ctx, reviveFailedChainSQL, subjectID, testNumber, urgent, notBefore, owner,
		ChainReviveBaseBackoff.Seconds(), ChainReviveMaxBackoff.Seconds())
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		notifyQueue(ctx, r.pool, ChannelGenJobs)
		return true, nil
	}
	// A failed job still in its backoff: do not start a new paid round.
	if at, err := r.ChainRetryAt(ctx, subjectID, testNumber); err != nil {
		return false, err
	} else if !at.IsZero() {
		return false, nil
	}
	tag, err = r.pool.Exec(ctx, `
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
	if err == nil && tag.RowsAffected() > 0 {
		notifyQueue(ctx, r.pool, ChannelGenJobs)
	}
	return tag.RowsAffected() > 0, err
}

// EnqueueChainJobNow is EnqueueChainJob with not_before = now() and
// urgent = true: the job is due immediately, ignoring the off-peak
// deferral. Used to bootstrap the chain (Тест 1) on a fresh database so the
// user never faces an empty grid. It goes through EnqueueChainJob, so a
// Тест 1 whose generation keeps failing is not re-queued on every restart.
func (r *GenerationRepository) EnqueueChainJobNow(ctx context.Context, subjectID int64, testNumber int) (bool, error) {
	return r.EnqueueChainJob(ctx, subjectID, testNumber, time.Now(), true, 0)
}

// EnqueuePersonalJob registers an URGENT job to generate the user's personal
// weak-topics test of the subject (fallback when the question bank and topic
// batches can not cover the weak topics). Idempotent via the partial unique
// index.
func (r *GenerationRepository) EnqueuePersonalJob(ctx context.Context, subjectID, userID int64) error {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, owner_user_id, not_before, urgent)
		VALUES ('personal', $1, $2, now(), TRUE)
		ON CONFLICT DO NOTHING`, subjectID, userID)
	if err == nil && tag.RowsAffected() > 0 {
		notifyQueue(ctx, r.pool, ChannelGenJobs)
	}
	return err
}

// EnqueueCustomJob registers the URGENT generation job of a paid custom
// order and returns its id. Idempotent: one job per order
// (idx_genjobs_custom_order) — a repeated call returns the existing job.
func (r *GenerationRepository) EnqueueCustomJob(ctx context.Context, subjectID, userID, orderID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, owner_user_id, custom_order_id, not_before, urgent)
		VALUES ('custom', $1, $2, $3, now(), TRUE)
		ON CONFLICT (custom_order_id) WHERE custom_order_id IS NOT NULL DO NOTHING
		RETURNING id`, subjectID, userID, orderID).Scan(&id)
	if err == nil {
		notifyQueue(ctx, r.pool, ChannelGenJobs)
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = r.pool.QueryRow(ctx, `SELECT id FROM generation_jobs WHERE custom_order_id = $1`, orderID).Scan(&id)
	return id, err
}

// CustomJobRequest is what a custom generation job needs from its order.
type CustomJobRequest struct {
	Description string
	Title       string
	Paid        bool // the order is still 'paid' (not refunded meanwhile)
}

// CustomJobRequest loads the order of a custom job (ErrNotFound if gone).
func (r *GenerationRepository) CustomJobRequest(ctx context.Context, orderID int64) (*CustomJobRequest, error) {
	var c CustomJobRequest
	err := r.pool.QueryRow(ctx, `SELECT description, title, status = 'paid' FROM custom_test_orders WHERE id = $1`, orderID).
		Scan(&c.Description, &c.Title, &c.Paid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CustomTestByOrder returns the test already stored for a custom order
// (nil = none).
func (r *GenerationRepository) CustomTestByOrder(ctx context.Context, orderID int64) (*models.Test, error) {
	return scanTest(r.pool.QueryRow(ctx, `SELECT `+testColumns+` FROM tests WHERE custom_order_id = $1`, orderID))
}

// PersonalJobsSince counts the user's personal generation jobs created at
// or after since (any status: every job is a generation request). Used for
// the per-user daily limit (R-9); clones of an existing test never create
// a job and therefore never count.
func (r *GenerationRepository) PersonalJobsSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM generation_jobs
		WHERE kind = 'personal' AND owner_user_id = $1 AND created_at >= $2`, userID, since).Scan(&n)
	return n, err
}

// DeferJob hands a 'running' job back to the queue until `until` without
// counting the run as an attempt (R-9: the daily DeepSeek cap is reached —
// retrying before the next day would only fail again and burn attempts).
func (r *GenerationRepository) DeferJob(ctx context.Context, jobID int64, until time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs
		SET status = 'pending', not_before = $2,
		    attempts = GREATEST(attempts - 1, 0), updated_at = now()
		WHERE id = $1 AND status = 'running'`, jobID, until)
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
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A 'done' row whose test no longer exists (deleted manually / by a
	// cleanup, or deactivated) still occupies the unique slot
	// idx_genjobs_chain_unique — no new job could ever be inserted and the
	// slot would hang in ⏳ forever (a missing Тест 1 blocked the whole
	// subject). Such orphaned rows are released by parking them as 'failed'.
	if _, err := tx.Exec(ctx, `
		UPDATE generation_jobs j
		SET status = 'failed', last_error = 'test missing: released', revivals = 0, updated_at = now()
		WHERE j.kind = 'chain' AND j.subject_id = $1 AND j.test_number = $2
		  AND j.status = 'done'
		  AND (j.test_id IS NULL OR NOT EXISTS (
		        SELECT 1 FROM tests t WHERE t.id = j.test_id AND t.is_active))`,
		subjectID, testNumber); err != nil {
		return false, err
	}

	// Revive exactly ONE failed row (the newest), and only when no active
	// or done job holds the unique slot — reviving several rows (or one
	// next to an active job) violated idx_genjobs_chain_unique — and only
	// when its backoff has elapsed (ChainReviveBaseBackoff).
	tag, err := tx.Exec(ctx, reviveFailedChainSQL, subjectID, testNumber, true, time.Now(), owner,
		ChainReviveBaseBackoff.Seconds(), ChainReviveMaxBackoff.Seconds())
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
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
//
// A job that already used maxAttempts attempts is parked as 'failed'
// instead: if that very job is what kills the process (OOM, panic in a
// dependency), re-queuing it forever would crash-loop the worker. Its
// urgency is kept as is — a deferred (off-peak) job is not promoted to
// urgent just because a worker died; not_before = now() only because the
// job was already due when it was claimed. Returns how many jobs were
// re-queued (pending) and parked (failed).
func (r *GenerationRepository) ResetStuckRunningJobs(ctx context.Context, stuckFor time.Duration, maxAttempts int) (requeued, failed int64, err error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE generation_jobs
		SET status     = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'pending' END,
		    last_error = CASE WHEN attempts >= $2 THEN 'stuck in running: attempts exhausted' ELSE last_error END,
		    not_before = CASE WHEN attempts >= $2 THEN not_before ELSE now() END,
		    updated_at = now()
		WHERE status = 'running' AND updated_at < now() - make_interval(secs => $1)
		RETURNING status`,
		durationSecs(stuckFor), maxAttempts)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return 0, 0, err
		}
		if st == "failed" {
			failed++
		} else {
			requeued++
		}
	}
	return requeued, failed, rows.Err()
}

// ReleaseJob hands a 'running' job back to the queue WITHOUT counting the
// interrupted run as an attempt: the worker is shutting down (deploy,
// SIGTERM), the job did not fail. status = 'pending', not_before = now(),
// attempts is restored to its value before the claim (ClaimNextJob
// increments it). Without this the job sat in 'running' until the
// stuck-job reaper picked it up (up to stuckJobTimeout later).
func (r *GenerationRepository) ReleaseJob(ctx context.Context, jobID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE generation_jobs
		SET status = 'pending', not_before = now(),
		    attempts = GREATEST(attempts - 1, 0), updated_at = now()
		WHERE id = $1 AND status = 'running'`, jobID)
	return err
}

// durationSecs converts a duration to whole seconds for make_interval
// (instead of passing Go's "10m0s" text as ::interval — it happens to parse,
// but is fragile).
func durationSecs(d time.Duration) int {
	return int(d.Seconds())
}

// qualitySweepLockKey is the advisory-lock key that keeps the quality sweep
// on ONE instance at a time (zero-downtime deploys run two instances).
const qualitySweepLockKey int64 = 0x6a757a3430737770 // "juz40swp"

// TryExclusive runs fn only when the cluster-wide advisory lock `key` is
// free; otherwise it returns ran = false immediately (another instance holds
// it). A SESSION-level lock (pg_try_advisory_lock) is taken on a DEDICATED
// connection (a direct one when lockURL is set — session locks are not
// reliable behind a transaction-mode pooler), with pg_advisory_unlock in a
// defer. No transaction is open while fn runs (fn makes slow AI calls), so
// there is no idle-in-transaction session. The connection is closed at the
// end; if the process dies, the server drops the session and the lock with it.
func (r *GenerationRepository) TryExclusive(ctx context.Context, key int64, fn func(context.Context) error) (ran bool, err error) {
	conn, err := r.dedicatedConn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key)
	}()
	return true, fn(ctx)
}

// dedicatedConn opens a connection that is not shared with the pool: a new
// direct connection to lockURL, or one hijacked from the pool (it never
// goes back, so a held session lock cannot leak to other callers).
func (r *GenerationRepository) dedicatedConn(ctx context.Context) (*pgx.Conn, error) {
	if r.lockURL != "" {
		return pgx.Connect(ctx, r.lockURL)
	}
	pc, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return pc.Hijack(), nil
}

// TryQualitySweepLock is TryExclusive with the quality-sweep key.
func (r *GenerationRepository) TryQualitySweepLock(ctx context.Context, fn func(context.Context) error) (bool, error) {
	return r.TryExclusive(ctx, qualitySweepLockKey, fn)
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

// HasUrgentWork reports whether a user-facing (urgent) job is due in the
// queue or currently running. The low-priority quality sweep yields to such
// work: it would otherwise compete with user generations for the Groq quota
// (and spill them over to the paid DeepSeek fallback).
func (r *GenerationRepository) HasUrgentWork(ctx context.Context) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM generation_jobs
		              WHERE urgent AND (status = 'running'
		                    OR (status = 'pending' AND not_before <= now())))`).Scan(&exists)
	return exists, err
}

// HasActiveJobs reports whether ANY generation job is running or due in
// the queue (deferred jobs whose not_before is in the future do not count).
// The quality sweep starts only when this is false.
func (r *GenerationRepository) HasActiveJobs(ctx context.Context) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM generation_jobs
		              WHERE status = 'running'
		                 OR (status = 'pending' AND not_before <= now()))`).Scan(&exists)
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
	var topicKey sql.NullString
	err = tx.QueryRow(ctx, `
		SELECT id, kind, subject_id, test_number, topics_fingerprint, owner_user_id, status, attempts, urgent, topic_key,
		       COALESCE(custom_order_id, 0)
		FROM generation_jobs
		WHERE status = 'pending' AND not_before <= now()
		ORDER BY urgent DESC, id
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).
		Scan(&j.ID, &j.Kind, &j.SubjectID, &testNumber, &fingerprint, &owner, &j.Status, &j.Attempts, &j.Urgent, &topicKey, &j.CustomOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.TestNumber = int(testNumber.Int64)
	j.TopicsFingerprint = fingerprint.String
	j.OwnerUserID = owner.Int64
	j.TopicKey = topicKey.String

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

// CompleteJob marks the job done and links the produced test. The job's
// stored batches (gen_job_batches) are no longer needed and go away in the
// same statement. (A FAILED job keeps them: a revived job reuses them.)
func (r *GenerationRepository) CompleteJob(ctx context.Context, jobID, testID int64) error {
	_, err := r.pool.Exec(ctx, `
		WITH d AS (DELETE FROM gen_job_batches WHERE job_id = $1)
		UPDATE generation_jobs
		SET status = 'done', test_id = NULLIF($2::bigint, 0), last_error = '', updated_at = now()
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
		    not_before = CASE WHEN attempts >= $2 THEN not_before ELSE now() + make_interval(secs => $4) END,
		    updated_at = now()
		WHERE id = $1`,
		jobID, maxAttempts, jobErrorText(jobErr), durationSecs(retryDelay))
	return err
}

// maxJobErrorLen caps generation_jobs.last_error (bytes): provider errors
// may carry a whole raw response body.
const maxJobErrorLen = 2000

// jobErrorText renders a job error for last_error: valid UTF-8 (PostgreSQL
// rejects anything else, and a provider message cut in the middle of a
// Cyrillic letter made FailJob itself fail — the job then stayed 'running'
// until the stuck-job reaper) and at most maxJobErrorLen bytes.
func jobErrorText(err error) string {
	s := strings.ToValidUTF8(fmt.Sprintf("%v", err), "\uFFFD")
	if len(s) > maxJobErrorLen {
		cut := maxJobErrorLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

// CreateGeneratedTest atomically inserts a generated test with its questions
// and links. For 'personal' tests the (subject, owner) unique index
// deduplicates concurrent inserts: the loser of the race re-reads the
// winner's test instead of failing. A personal test with test_number = 0
// gets the next personal number (personal_test_number_seq), far above the
// chain. The UNIQUE (subject_id, test_number) constraint also guards chain
// tests against concurrent generation of the same number.
// personalLockNS is the advisory-lock namespace of personal-test creation.
const personalLockNS int32 = 40_001

func (r *GenerationRepository) CreateGeneratedTest(ctx context.Context, test *models.Test, questions []models.SeedQuestion) (*models.Test, error) {
	return r.createTest(ctx, test, questions, 0)
}

// createTest inserts the test row and its questions. linkFromTestID > 0
// (R-8b shared clone) links the SAME question rows of that test instead of
// inserting new questions.
func (r *GenerationRepository) createTest(ctx context.Context, test *models.Test, questions []models.SeedQuestion, linkFromTestID int64) (*models.Test, error) {
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
	if test.Kind == models.TestKindCustom && test.CustomOrderID > 0 {
		// A retried custom job whose earlier run already stored the test
		// (crash before CompleteJob): never a second test for one order.
		existing, err := scanTest(tx.QueryRow(ctx, `SELECT `+testColumns+` FROM tests WHERE custom_order_id = $1`, test.CustomOrderID))
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return existing, nil
		}
	}
	if models.IsOwnedKind(test.Kind) && test.TestNumber == 0 {
		// B3: numbers come from personal_test_number_seq (shared with bank
		// tests), not MAX(test_number)+1.
		if err := tx.QueryRow(ctx, `SELECT nextval('personal_test_number_seq')`).
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
	var customOrder any
	if test.CustomOrderID > 0 {
		customOrder = test.CustomOrderID
	}
	var testID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO tests (subject_id, test_number, title, is_active, kind, topics, owner_user_id, topics_fingerprint, origin_test_id, custom_order_id)
		VALUES ($1, $2, $3, TRUE, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (subject_id, test_number) DO NOTHING
		RETURNING id`,
		test.SubjectID, test.TestNumber, test.Title, test.Kind, topicsJSON, owner, fingerprint, origin, customOrder).
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

	if linkFromTestID > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO test_questions (test_id, question_id, position)
			SELECT $1, question_id, position FROM test_questions WHERE test_id = $2`,
			testID, linkFromTestID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		test.ID = testID
		return test, nil
	}

	rawTopics := make([]string, len(questions))
	for i, sq := range questions {
		rawTopics[i] = sq.Topic
	}
	// A custom test stays OUT of the topic catalog and the question bank:
	// its topics come from the student's own request (topic_key NULL — the
	// bank selects by topic_key, so these questions are never handed out).
	topicKeys := map[string]string{}
	if test.Kind != models.TestKindCustom {
		topicKeys, err = ensureTopicKeys(ctx, tx, test.SubjectID, rawTopics)
		if err != nil {
			return nil, err
		}
	}

	// One round trip for all questions (pgx.Batch) instead of 2×20
	// sequential statements: every question row + its link is ONE
	// statement (a data-modifying CTE), and all of them are pipelined.
	labels := []string{"A", "B", "C", "D"}
	batch := &pgx.Batch{}
	for i, sq := range questions {
		if sq.Correct < 0 || sq.Correct > 3 {
			return nil, fmt.Errorf("question %d: invalid correct index %d", i+1, sq.Correct)
		}
		batch.Queue(`
			WITH q AS (
				INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
				                       correct_answer, topic, difficulty, quality_checked_at, topic_key)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $10 THEN now() END, NULLIF($11, ''))
				RETURNING id)
			INSERT INTO test_questions (test_id, question_id, position)
			SELECT $12, q.id, $13 FROM q`,
			test.SubjectID, sq.Text, sq.Options[0], sq.Options[1], sq.Options[2], sq.Options[3],
			labels[sq.Correct], sq.Topic, sq.Difficulty, sq.QualityChecked,
			topicKeys[models.NormalizeTopic(sq.Topic)], testID, i+1)
	}
	br := tx.SendBatch(ctx, batch)
	for i := range questions {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return nil, fmt.Errorf("insert question %d: %w", i+1, err)
		}
	}
	if err := br.Close(); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if test.Kind == models.TestKindChain {
		InvalidateChainCache(test.SubjectID)
	}
	test.ID = testID
	return test, nil
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

// GiveUpQualityCheck stamps a flagged question that can not be repaired
// (e.g. shared by several tests) as checked AND as given up
// (quality_attempts = QualityGiveUpAttempts): the sweep stops paying for
// it, and the question bank does not hand it out (it still fails the audit).
func (r *GenerationRepository) GiveUpQualityCheck(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE questions
		SET quality_attempts = GREATEST(quality_attempts, $2), quality_checked_at = now()
		WHERE id = $1`, id, QualityGiveUpAttempts)
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
		UPDATE questions SET quality_postponed_until = now() + make_interval(secs => $2)
		WHERE id = $1`, id, durationSecs(delay))
	return err
}

// ErrQuestionBusy: the question is shown in an unanswered position of an
// in-progress attempt — rewriting it now would change the text/key under a
// student who is looking at it. The sweep retries later.
var ErrQuestionBusy = errors.New("question is in an active attempt")

// ErrQuestionShared: the question is linked to more than one test (R-8b
// shared clone); rewriting it would change other owners' tests.
var ErrQuestionShared = errors.New("question is shared by several tests")

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
	// R-8b guard: a question shared by several tests (checked clone) must
	// not be rewritten — that would reset other owners' progress.
	var shared bool
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) > 1 FROM test_questions WHERE question_id = $1`, id).Scan(&shared); err != nil {
		return err
	}
	if shared {
		return ErrQuestionShared
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
	// The question now has a different text and correct answer — the old
	// 🟢/🟡 marks describe a question that no longer exists and must not
	// count towards the unlock bar. Chain unlocks already earned stay safe
	// thanks to the permanent user_subject_state watermark.
	if _, err := tx.Exec(ctx, `DELETE FROM user_question_progress WHERE question_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ChainTestID returns the id of the active chain test #testNumber of the
// subject, or 0 when it does not exist yet. A light point query used by the
// generation watchers (R-7) instead of listing the whole chain.
func (r *GenerationRepository) ChainTestID(ctx context.Context, subjectID int64, testNumber int) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM tests
		WHERE subject_id = $1 AND kind = 'chain' AND test_number = $2 AND is_active
		LIMIT 1`, subjectID, testNumber).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// ActivePersonalJobID returns the id of the pending/running personal job of
// (subject, user), or 0 when there is none (R-7).
func (r *GenerationRepository) ActivePersonalJobID(ctx context.Context, subjectID, userID int64) (int64, error) {
	var id int64
	err := r.pool.QueryRow(ctx, `
		SELECT id FROM generation_jobs
		WHERE kind = 'personal' AND subject_id = $1 AND owner_user_id = $2
		  AND status IN ('pending','running')
		ORDER BY id DESC LIMIT 1`, subjectID, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// JobState reports the produced test (testID > 0 once done) and whether the
// job is still pending/running. A missing job is neither (R-7).
func (r *GenerationRepository) JobState(ctx context.Context, jobID int64) (testID int64, pending bool, err error) {
	var status string
	err = r.pool.QueryRow(ctx, `
		SELECT status, COALESCE(test_id, 0) FROM generation_jobs WHERE id = $1`, jobID).Scan(&status, &testID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if status != "done" {
		testID = 0
	}
	return testID, status == "pending" || status == "running", nil
}

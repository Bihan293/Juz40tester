package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// TranslationRepository manages the cached per-question translations of test
// content (question_translations table). The Russian question row is always
// the master version; a translation is written ONCE per question and then
// reused by every user — this is the cost ceiling of the whole feature.
type TranslationRepository struct {
	pool *pgxpool.Pool
}

func NewTranslationRepository(pool *pgxpool.Pool) *TranslationRepository {
	return &TranslationRepository{pool: pool}
}

// TranslationsForTest returns the translations of a test's questions into the
// given language, keyed by question id. Only the questions of THIS test are
// loaded (translations are stored per question, shared across tests).
func (r *TranslationRepository) TranslationsForTest(ctx context.Context, testID int64, lang string) (map[int64]*models.QuestionTranslation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT tr.question_id, tr.lang, tr.question_text, tr.option_a, tr.option_b,
		       tr.option_c, tr.option_d, tr.topic, q.correct_answer
		FROM test_questions tq
		JOIN question_translations tr ON tr.question_id = tq.question_id AND tr.lang = $2
		JOIN questions q ON q.id = tr.question_id
		WHERE tq.test_id = $1`, testID, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]*models.QuestionTranslation)
	for rows.Next() {
		var tr models.QuestionTranslation
		if err := rows.Scan(&tr.QuestionID, &tr.Lang, &tr.Text, &tr.OptionA, &tr.OptionB,
			&tr.OptionC, &tr.OptionD, &tr.Topic, &tr.CorrectAnswer); err != nil {
			return nil, err
		}
		out[tr.QuestionID] = &tr
	}
	return out, rows.Err()
}

// SaveTranslations atomically stores translations for a batch of questions.
// The model translates the OPTION TEXTS only — the correct-answer letter
// never leaves the server: the answer key is read back from the master
// questions row at query time (TranslationsForTest / TranslationForQuestion
// JOIN questions), so it can never be lost or altered in translation.
// correctByQuestionID validates that every stored row maps to a known
// master question.
// ON CONFLICT DO NOTHING makes concurrent translation runs safe (the loser
// keeps the winner's row — both came from the same master text anyway).
func (r *TranslationRepository) SaveTranslations(ctx context.Context, trs []models.QuestionTranslation, correctByQuestionID map[int64]string) error {
	if len(trs) == 0 {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, tr := range trs {
		correct := correctByQuestionID[tr.QuestionID]
		if correct == "" {
			return fmt.Errorf("no master correct_answer for question %d", tr.QuestionID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO question_translations
			    (question_id, lang, question_text, option_a, option_b, option_c, option_d, topic)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (question_id, lang) DO NOTHING`,
			tr.QuestionID, tr.Lang, tr.Text, tr.OptionA, tr.OptionB, tr.OptionC, tr.OptionD, tr.Topic); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// TranslatedQuestionCount returns how many of the given questions already
// have a translation in the language (used to decide whether a test needs a
// translation run at all — zero cost when everything is cached).
func (r *TranslationRepository) TranslatedQuestionCount(ctx context.Context, questionIDs []int64, lang string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM question_translations
		WHERE lang = $1 AND question_id = ANY($2)`, lang, questionIDs).Scan(&n)
	return n, err
}

// QuestionIDsForTest returns the question ids of a test in their canonical
// order (test_questions.position). The generator uses it to pair the fresh
// question rows of a CLONED personal test with the cached translations of
// the clone source.
func (r *TranslationRepository) QuestionIDsForTest(ctx context.Context, testID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT question_id FROM test_questions
		WHERE test_id = $1 ORDER BY position`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CopyTranslationsToTest copies the cached translations of a SOURCE test's
// questions onto the given (freshly cloned) question ids, matched by the
// master question TEXT. Personal weak-topics tests are cloned with NEW
// question rows (so the clone has no shared progress), which means the
// cached Kazakh translations — keyed by question id — would otherwise be
// lost and every Kazakh-speaking user would pay for a NEW DeepSeek
// translation of a test that is word-for-word identical to an already
// translated one. Copying the cached rows costs zero API calls.
//
// Idempotent (ON CONFLICT DO NOTHING) and race-safe: a row is written only
// when the source question is an exact text match of the clone's question
// (the option texts then come from the source row whose question matched —
// exactly the right pairing).
func (r *TranslationRepository) CopyTranslationsToTest(ctx context.Context, srcTestID int64, dstQuestionIDs []int64, lang string) (int64, error) {
	if len(dstQuestionIDs) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO question_translations
		    (question_id, lang, question_text, option_a, option_b, option_c, option_d, topic)
		SELECT dq.id, tr.lang, tr.question_text, tr.option_a, tr.option_b, tr.option_c, tr.option_d, tr.topic
		FROM test_questions stq
		JOIN questions sq ON sq.id = stq.question_id
		JOIN question_translations tr ON tr.question_id = sq.id AND tr.lang = $3
		JOIN questions dq ON dq.question_text = sq.question_text AND dq.id = ANY($2)
		WHERE stq.test_id = $1
		ON CONFLICT (question_id, lang) DO NOTHING`, srcTestID, dstQuestionIDs, lang)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Background translation queue (R-4)
// ---------------------------------------------------------------------------

// TranslationJob is one row of translation_jobs: "translate test X into
// language L". UNIQUE (test_id, lang) makes a translation a single job row
// across every instance — the cross-instance replacement of the old
// in-memory per-test mutex.
type TranslationJob struct {
	ID        int64
	TestID    int64
	Lang      string
	Status    string // pending | running | done | failed
	Attempts  int
	LastError string
}

// translationRearmAfterFail: a job that exhausted its retries is re-armed by
// a new request only after this pause, so a provider outage is not hammered
// by every tap of every student.
const translationRearmAfterFail = 10 * time.Minute

// EnqueueTranslationJob queues the translation of a test (idempotent). A new
// row is inserted as 'pending'; an existing 'pending'/'running' row is left
// untouched (that very job will do the work — never translate twice); a
// 'done' row is re-armed (the caller only enqueues when the cached
// translation is INCOMPLETE, e.g. a question was rewritten by the quality
// sweep); a 'failed' row is re-armed once translationRearmAfterFail has
// passed. queued reports whether a job is now pending (newly inserted or
// re-armed).
func (r *TranslationRepository) EnqueueTranslationJob(ctx context.Context, testID int64, lang string) (queued bool, err error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO translation_jobs (test_id, lang)
		VALUES ($1, $2)
		ON CONFLICT (test_id, lang) DO UPDATE
		SET status = 'pending', attempts = 0, last_error = '',
		    not_before = now(), updated_at = now()
		WHERE translation_jobs.status = 'done'
		   OR (translation_jobs.status = 'failed'
		       AND translation_jobs.updated_at < now() - make_interval(secs => $3))`,
		testID, lang, durationSecs(translationRearmAfterFail))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		notifyQueue(ctx, r.pool, ChannelTrJobs)
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimNextTranslationJob atomically picks the oldest due pending job and
// marks it running (FOR UPDATE SKIP LOCKED: concurrent workers on any
// instance never claim the same row). Returns nil when nothing is due.
func (r *TranslationRepository) ClaimNextTranslationJob(ctx context.Context) (*TranslationJob, error) {
	var j TranslationJob
	err := r.pool.QueryRow(ctx, `
		UPDATE translation_jobs
		SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM translation_jobs
			WHERE status = 'pending' AND not_before <= now()
			ORDER BY not_before, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, test_id, lang, status, attempts, last_error`).
		Scan(&j.ID, &j.TestID, &j.Lang, &j.Status, &j.Attempts, &j.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// TouchTranslationJob refreshes the heartbeat of a running job, so the
// reaper never mistakes a slow-but-alive translation for a dead one.
func (r *TranslationRepository) TouchTranslationJob(ctx context.Context, jobID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE translation_jobs SET updated_at = now()
		WHERE id = $1 AND status = 'running'`, jobID)
	return err
}

// CompleteTranslationJob marks the job done.
func (r *TranslationRepository) CompleteTranslationJob(ctx context.Context, jobID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE translation_jobs
		SET status = 'done', last_error = '', updated_at = now()
		WHERE id = $1`, jobID)
	return err
}

// FailTranslationJob records a failed run: retried after retryDelay, parked
// as 'failed' once maxAttempts runs were spent.
func (r *TranslationRepository) FailTranslationJob(ctx context.Context, jobID int64, jobErr error, retryDelay time.Duration, maxAttempts int) error {
	msg := "translation failed"
	if jobErr != nil {
		msg = jobErr.Error()
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE translation_jobs
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'pending' END,
		    last_error = $3,
		    not_before = CASE WHEN attempts >= $2 THEN not_before ELSE now() + make_interval(secs => $4) END,
		    updated_at = now()
		WHERE id = $1`, jobID, maxAttempts, msg, durationSecs(retryDelay))
	return err
}

// ReleaseTranslationJob hands a running job back to the queue without
// counting the interrupted run (shutdown / deploy — the job did not fail).
func (r *TranslationRepository) ReleaseTranslationJob(ctx context.Context, jobID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE translation_jobs
		SET status = 'pending', not_before = now(),
		    attempts = GREATEST(attempts - 1, 0), updated_at = now()
		WHERE id = $1 AND status = 'running'`, jobID)
	return err
}

// ResetStuckTranslationJobs returns running jobs whose heartbeat stopped for
// stuckFor (the worker died: deploy, OOM, restart) to 'pending'; a job that
// already used maxAttempts is parked as 'failed' (a job that kills the
// process must not crash-loop it).
func (r *TranslationRepository) ResetStuckTranslationJobs(ctx context.Context, stuckFor time.Duration, maxAttempts int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE translation_jobs
		SET status     = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'pending' END,
		    last_error = CASE WHEN attempts >= $2 THEN 'stuck in running: attempts exhausted' ELSE last_error END,
		    not_before = now(),
		    updated_at = now()
		WHERE status = 'running' AND updated_at < now() - make_interval(secs => $1)`,
		durationSecs(stuckFor), maxAttempts)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TranslationJobState returns the status and the last error of the job of
// (test, lang); ("", "", nil) when no job exists. One indexed lookup — the
// shared per-test waiter uses it as its cheap fallback check.
func (r *TranslationRepository) TranslationJobState(ctx context.Context, testID int64, lang string) (status, lastError string, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT status, last_error FROM translation_jobs
		WHERE test_id = $1 AND lang = $2`, testID, lang).Scan(&status, &lastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return status, lastError, err
}

// MasterQuestionsForTest returns the Russian master questions of a test in
// their canonical order — the input of a background translation job.
func (r *TranslationRepository) MasterQuestionsForTest(ctx context.Context, testID int64) ([]models.Question, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT q.id, q.subject_id, q.question_text, q.option_a, q.option_b,
		       q.option_c, q.option_d, q.correct_answer, q.topic, q.difficulty
		FROM test_questions tq
		JOIN questions q ON q.id = tq.question_id
		WHERE tq.test_id = $1
		ORDER BY tq.position`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Question
	for rows.Next() {
		var q models.Question
		if err := rows.Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB,
			&q.OptionC, &q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SubjectNameForTest returns the name of the subject a test belongs to (the
// worker re-checks that a language subject is never translated).
func (r *TranslationRepository) SubjectNameForTest(ctx context.Context, testID int64) (string, error) {
	var name string
	err := r.pool.QueryRow(ctx, `
		SELECT s.name FROM tests t JOIN subjects s ON s.id = t.subject_id
		WHERE t.id = $1`, testID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

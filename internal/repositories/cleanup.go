package repositories

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// R-8a: daily cleanup of old data that only grows the database. Rows are
// deleted in small batches (one short statement each) so no long lock or
// huge transaction is held. user_topic_stats and user_question_progress are
// NEVER touched: the knowledge statistics live there, not in the history.

// CleanupBatchSize is the number of rows removed per statement.
const CleanupBatchSize = 1000

// CleanupRepository deletes stale history rows.
type CleanupRepository struct {
	pool *pgxpool.Pool
}

func NewCleanupRepository(pool *pgxpool.Pool) *CleanupRepository {
	return &CleanupRepository{pool: pool}
}

// DeleteOldAttemptQuestions removes up to limit attempt_questions rows of
// finished (completed / abandoned) attempts last touched before olderThan.
// The attempt row itself (score, dates) is kept.
func (r *CleanupRepository) DeleteOldAttemptQuestions(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM attempt_questions
		WHERE id IN (
			SELECT aq.id
			FROM test_attempts a
			JOIN attempt_questions aq ON aq.attempt_id = a.id
			WHERE a.status IN ('completed','abandoned')
			  AND a.updated_at < now() - make_interval(secs => $1)
			LIMIT $2)`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOldFinishedJobs removes up to limit generation_jobs rows in a final
// state (done / failed) last updated before olderThan. Safe: a re-enqueued
// chain job for an existing test is resolved by the worker without an AI
// call, and the per-user daily limit only looks at today's jobs.
func (r *CleanupRepository) DeleteOldFinishedJobs(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM generation_jobs
		WHERE id IN (
			SELECT id FROM generation_jobs
			WHERE status IN ('done','failed')
			  AND updated_at < now() - make_interval(secs => $1)
			LIMIT $2)`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteEmptyAbandonedAttempts removes up to limit abandoned attempts with
// no answer at all, last touched before olderThan (their questions go with
// them via ON DELETE CASCADE).
func (r *CleanupRepository) DeleteEmptyAbandonedAttempts(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM test_attempts
		WHERE id IN (
			SELECT a.id FROM test_attempts a
			WHERE a.status = 'abandoned'
			  AND a.correct_count = 0 AND a.wrong_count = 0
			  AND a.updated_at < now() - make_interval(secs => $1)
			  AND NOT EXISTS (
			        SELECT 1 FROM attempt_questions aq
			        WHERE aq.attempt_id = a.id AND aq.answered)
			LIMIT $2)`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOldTranslationJobs removes up to limit translation_jobs in status
// done last updated before olderThan (A3). Safe: a test that turns out to be
// incompletely translated simply gets a fresh job row on the next request.
func (r *CleanupRepository) DeleteOldTranslationJobs(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM translation_jobs
		WHERE id IN (
			SELECT id FROM translation_jobs
			WHERE status = 'done'
			  AND updated_at < now() - make_interval(secs => $1)
			LIMIT $2)`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteStaleTemplates removes up to limit ownerless hidden personal-test
// templates (left by «🏁 Закончить тест») created before olderThan (A3; the
// schema has no last-access date, so the creation date is used). Their
// questions are deleted with the same guard as DeletePersonalTest: only
// questions no other test references. Returns the number of templates.
func (r *CleanupRepository) DeleteStaleTemplates(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id FROM tests
		WHERE kind = 'personal' AND owner_user_id IS NULL AND NOT is_active
		  AND created_at < now() - make_interval(secs => $1)
		ORDER BY id
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	var qids []int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(array_agg(DISTINCT question_id), '{}')
		FROM test_questions WHERE test_id = ANY($1::bigint[])`, ids).Scan(&qids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM tests WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return 0, err
	}
	if len(qids) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM questions q
			WHERE q.id = ANY($1::bigint[])
			  AND NOT EXISTS (SELECT 1 FROM test_questions tq WHERE tq.question_id = q.id)`, qids); err != nil {
			return 0, err
		}
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// DeleteOrphanPersonalDone removes up to limit user_personal_done rows whose
// lineage is gone entirely: neither the root test nor any clone of it
// exists (A3). olderThan is unused. A row whose ROOT was deleted while
// clones of the lineage remain is kept on purpose — it still stops the user
// from getting those clones (the same questions) again; that is also why no
// ON DELETE CASCADE foreign key to tests(id) is added.
func (r *CleanupRepository) DeleteOrphanPersonalDone(ctx context.Context, _ time.Duration, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM user_personal_done
		WHERE (user_id, root_test_id) IN (
			SELECT d.user_id, d.root_test_id FROM user_personal_done d
			WHERE NOT EXISTS (
				SELECT 1 FROM tests t
				WHERE t.id = d.root_test_id OR t.origin_test_id = d.root_test_id)
			LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteOldFinishedAttempts removes up to limit finished (completed /
// abandoned) attempts last touched before olderThan (A3, ATTEMPT_TTL_DAYS).
// The latest attempt and the best one (max correct_count) of every
// (user, test) pair are always kept. Safe: unlocks read user_subject_state,
// statistics read user_question_progress / user_topic_stats; finished
// attempts are only read when the user opens that attempt's result.
func (r *CleanupRepository) DeleteOldFinishedAttempts(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM test_attempts
		WHERE id IN (
			SELECT a.id FROM test_attempts a
			WHERE a.status IN ('completed','abandoned')
			  AND a.updated_at < now() - make_interval(secs => $1)
			  AND EXISTS (SELECT 1 FROM test_attempts n
			              WHERE n.user_id = a.user_id AND n.test_id = a.test_id AND n.id > a.id)
			  AND a.id <> (SELECT b.id FROM test_attempts b
			               WHERE b.user_id = a.user_id AND b.test_id = a.test_id
			               ORDER BY b.correct_count DESC, b.id DESC LIMIT 1)
			LIMIT $2)`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

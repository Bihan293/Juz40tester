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

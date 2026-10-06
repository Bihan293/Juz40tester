package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// TopicBatchSize is the number of questions one topic_batch job generates.
const TopicBatchSize = 10

// EnqueueTopicBatchJobs (B4) queues one URGENT topic_batch job per catalog
// topic_key of the subject. Idempotent: the partial unique index
// idx_genjobs_topic_batch_unique + ON CONFLICT DO NOTHING keep at most one
// active (pending/running) batch per (subject_id, topic_key), so concurrent
// requests never create duplicate jobs. Returns the number of NEW jobs.
func (r *GenerationRepository) EnqueueTopicBatchJobs(ctx context.Context, subjectID int64, topicKeys []string) (int, error) {
	keys := make([]string, 0, len(topicKeys))
	seen := make(map[string]bool, len(topicKeys))
	for _, k := range topicKeys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, topic_key, not_before, urgent)
		SELECT 'topic_batch', $1, k, now(), TRUE
		FROM unnest($2::text[]) AS k
		ON CONFLICT (subject_id, topic_key) WHERE kind = 'topic_batch' AND status IN ('pending','running')
		DO NOTHING`, subjectID, keys)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() > 0 {
		notifyQueue(ctx, r.pool, ChannelGenJobs)
	}
	return int(tag.RowsAffected()), nil
}

// ActiveTopicBatchKeys returns the topic_keys of the subject that have an
// active (pending/running) topic_batch job.
func (r *GenerationRepository) ActiveTopicBatchKeys(ctx context.Context, subjectID int64) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT topic_key FROM generation_jobs
		WHERE kind = 'topic_batch' AND subject_id = $1 AND status IN ('pending','running')
		ORDER BY topic_key`, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SaveBankQuestions (B4) stores audited questions of a topic_batch job in
// the bank: no test row, topic_key set, quality_checked_at = now(). Runs in
// one transaction (all or nothing). Returns the new question ids.
func (r *GenerationRepository) SaveBankQuestions(ctx context.Context, subjectID int64, topicKey string, questions []models.SeedQuestion) ([]int64, error) {
	if topicKey == "" {
		return nil, errors.New("bank questions without topic_key")
	}
	if len(questions) == 0 {
		return nil, errors.New("no bank questions to save")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM subject_topics WHERE subject_id = $1 AND topic_key = $2)`,
		subjectID, topicKey).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("topic_key %q is not in the catalog of subject %d", topicKey, subjectID)
	}

	labels := []string{"A", "B", "C", "D"}
	ids := make([]int64, 0, len(questions))
	for i, sq := range questions {
		if sq.Correct < 0 || sq.Correct > 3 {
			return nil, fmt.Errorf("question %d: invalid correct index %d", i+1, sq.Correct)
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
			                       correct_answer, topic, difficulty, quality_checked_at, topic_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10)
			RETURNING id`,
			subjectID, sq.Text, sq.Options[0], sq.Options[1], sq.Options[2], sq.Options[3],
			labels[sq.Correct], sq.Topic, sq.Difficulty, topicKey).Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

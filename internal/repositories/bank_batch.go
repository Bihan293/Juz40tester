package repositories

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/jackc/pgx/v5"
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
// one transaction (all or nothing). Questions whose text already exists in
// the bank of the topic are skipped. Returns the new question ids.
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
		// Duplicates by question text within the topic are skipped (B4a):
		// the same question is never stored twice in the bank of a topic.
		var id int64
		err := tx.QueryRow(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d,
			                       correct_answer, topic, difficulty, quality_checked_at, topic_key)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, now(), $10
			WHERE NOT EXISTS (
				SELECT 1 FROM questions
				WHERE subject_id = $1 AND topic_key = $10
				  AND lower(btrim(question_text)) = lower(btrim($2)))
			RETURNING id`,
			subjectID, sq.Text, sq.Options[0], sq.Options[1], sq.Options[2], sq.Options[3],
			labels[sq.Correct], sq.Topic, sq.Difficulty, topicKey).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

// EnqueueTopicBatch (B4a) queues one topic_batch job for a single catalog
// topic; reports whether a NEW job was created (false: one is already
// active — the unique index dedupes concurrent calls).
func (r *GenerationRepository) EnqueueTopicBatch(ctx context.Context, subjectID int64, topicKey string) (bool, error) {
	n, err := r.EnqueueTopicBatchJobs(ctx, subjectID, []string{topicKey})
	return n > 0, err
}

// TopicBankInfo (B4a) returns the catalog title of a topic and the texts of
// the bank questions already stored for it (at most limit, newest first).
func (r *GenerationRepository) TopicBankInfo(ctx context.Context, subjectID int64, topicKey string, limit int) (title string, texts []string, err error) {
	if err := r.pool.QueryRow(ctx, `
		SELECT title FROM subject_topics WHERE subject_id = $1 AND topic_key = $2`,
		subjectID, topicKey).Scan(&title); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, fmt.Errorf("topic_key %q is not in the catalog of subject %d", topicKey, subjectID)
		}
		return "", nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT question_text FROM questions
		WHERE subject_id = $1 AND topic_key = $2
		ORDER BY id DESC LIMIT $3`, subjectID, topicKey, limit)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return "", nil, err
		}
		texts = append(texts, t)
	}
	return title, texts, rows.Err()
}

// TopicBatchCooldown: a topic whose topic_batch finished (done or failed)
// this recently is not batched again for a bank shortage (B4b) — the batch
// did not help (duplicates, failures), the caller falls back to the legacy
// personal generation instead of looping on paid batches.
const TopicBatchCooldown = 15 * time.Minute

// EnqueueShortTopicBatches (B4b) queues URGENT topic_batch jobs for the
// short topics of a bank assembly: only catalog topics, none finished within
// cooldown; duplicates are cut by idx_genjobs_topic_batch_unique. Returns
// the number of NEW jobs and the given keys that now have an active
// (pending/running) batch — the topics worth waiting for.
func (r *GenerationRepository) EnqueueShortTopicBatches(ctx context.Context, subjectID int64, topicKeys []string, cooldown time.Duration) (created int, active []string, err error) {
	if len(topicKeys) == 0 {
		return 0, nil, nil
	}
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO generation_jobs (kind, subject_id, topic_key, not_before, urgent)
		SELECT 'topic_batch', $1, k, now(), TRUE
		FROM (SELECT DISTINCT k FROM unnest($2::text[]) AS k WHERE k <> '') u
		WHERE EXISTS (SELECT 1 FROM subject_topics st WHERE st.subject_id = $1 AND st.topic_key = u.k)
		  AND NOT EXISTS (
			SELECT 1 FROM generation_jobs j
			WHERE j.kind = 'topic_batch' AND j.subject_id = $1 AND j.topic_key = u.k
			  AND j.status IN ('done','failed')
			  AND j.updated_at > now() - make_interval(secs => $3::float8))
		ON CONFLICT (subject_id, topic_key) WHERE kind = 'topic_batch' AND status IN ('pending','running')
		DO NOTHING`, subjectID, topicKeys, cooldown.Seconds())
	if err != nil {
		return 0, nil, err
	}
	if tag.RowsAffected() > 0 {
		notifyQueue(ctx, r.pool, ChannelGenJobs)
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT topic_key FROM generation_jobs
		WHERE kind = 'topic_batch' AND subject_id = $1 AND topic_key = ANY($2::text[])
		  AND status IN ('pending','running')
		ORDER BY topic_key`, subjectID, topicKeys)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return 0, nil, err
		}
		active = append(active, k)
	}
	return int(tag.RowsAffected()), active, rows.Err()
}

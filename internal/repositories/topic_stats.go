package repositories

import (
	"context"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// Per-topic statistics of a user (table user_topic_stats). This is the ONLY
// source of weak topics: they are derived from the accumulated answers of the
// user in a topic of a subject, across every test (chain and personal) and
// every attempt — not from the status of individual questions.

// recordTopicAnswer adds one answer to the user's statistics of the topic.
// recent keeps the last models.TopicWindow answers ('1'/'0', oldest first).
func recordTopicAnswer(ctx context.Context, tx pgx.Tx, userID, subjectID int64, topic string, correct bool) error {
	key := models.NormalizeTopic(topic)
	if key == "" {
		return nil
	}
	c, w, mark := 0, 1, "0"
	if correct {
		c, w, mark = 1, 0, "1"
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, subject_id, topic_key) DO UPDATE SET
			topic         = EXCLUDED.topic,
			correct_count = user_topic_stats.correct_count + EXCLUDED.correct_count,
			wrong_count   = user_topic_stats.wrong_count   + EXCLUDED.wrong_count,
			recent        = right(user_topic_stats.recent || EXCLUDED.recent, $8),
			updated_at    = now()`,
		userID, subjectID, key, topic, c, w, mark, models.TopicWindow)
	return err
}

// countsForTopic reports whether an answer is evidence about the TOPIC.
// Answers to a question that was already 🟢 before it are NOT counted at
// all — neither correct nor wrong. Re-answering a memorised question
// (retaking a test to reach the unlock bar) proves memory of that question,
// not knowledge of the topic; and counting only its occasional misses (while
// ignoring its correct answers) skewed the statistics: a well-learnt topic
// slowly accumulated random slips and drifted into 🔴/🟡. Answers to
// not-yet-mastered questions always count, both ways.
func countsForTopic(prevStatus int) bool {
	return prevStatus < models.StatusMastered
}

// TopicStats returns the user's statistics of every topic of the subject.
func (r *GenerationRepository) TopicStats(ctx context.Context, userID, subjectID int64) ([]models.TopicStat, error) {
	return r.queryTopicStats(ctx, `
		SELECT subject_id, topic_key, topic, correct_count, wrong_count, recent
		FROM user_topic_stats
		WHERE user_id = $1 AND subject_id = $2
		ORDER BY topic_key`, userID, subjectID)
}

// AllTopicStats returns the user's topic statistics of every subject.
func (r *GenerationRepository) AllTopicStats(ctx context.Context, userID int64) ([]models.TopicStat, error) {
	return r.queryTopicStats(ctx, `
		SELECT subject_id, topic_key, topic, correct_count, wrong_count, recent
		FROM user_topic_stats
		WHERE user_id = $1
		ORDER BY subject_id, topic_key`, userID)
}

func (r *GenerationRepository) queryTopicStats(ctx context.Context, q string, args ...any) ([]models.TopicStat, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TopicStat
	for rows.Next() {
		var s models.TopicStat
		if err := rows.Scan(&s.SubjectID, &s.Key, &s.Topic, &s.Correct, &s.Wrong, &s.Recent); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// BackfillTopicStats builds user_topic_stats ONCE from the answer history
// already stored in attempt_questions (answers given before the per-topic
// statistics existed), so existing users keep their weak topics after the
// upgrade. Answers are replayed in order with the same rules as live
// answers (countsForTopic). Safe with several instances: the marker row is
// inserted in the same transaction — a concurrent instance blocks on it and
// then sees the backfill as done.
//
// It runs in the background AFTER the HTTP server is up (audit #24), so live
// answers may arrive meanwhile. To stay exact the transaction takes an
// EXCLUSIVE lock on user_topic_stats before reading the history: a live
// answer that already wrote its statistics is committed first (and is part
// of the history read below); a later one waits and adds on top. The rows
// are then OVERWRITTEN with the aggregate of the full history — never added
// to — so nothing is counted twice. The history is streamed (not loaded into
// memory) and aggregated per (user, subject, topic): one batched upsert per
// topic instead of one INSERT per historical answer.
func (r *GenerationRepository) BackfillTopicStats(ctx context.Context) error {
	const name = "topic_stats_v1"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var marker string
	err = tx.QueryRow(ctx, `
		INSERT INTO app_backfills (name) VALUES ($1)
		ON CONFLICT DO NOTHING RETURNING name`, name).Scan(&marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already done
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE user_topic_stats IN EXCLUSIVE MODE`); err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT ta.user_id, q.subject_id, q.id, q.topic, aq.is_correct
		FROM attempt_questions aq
		JOIN test_attempts ta ON ta.id = aq.attempt_id
		JOIN questions q      ON q.id = aq.question_id
		WHERE aq.answered AND aq.is_correct IS NOT NULL AND q.topic <> ''
		ORDER BY ta.started_at, ta.id, aq.position`)
	if err != nil {
		return err
	}
	type qkey struct{ user, question int64 }
	type tkey struct {
		user, subject int64
		key           string
	}
	type agg struct {
		topic          string
		correct, wrong int
		recent         []byte
	}
	status := map[qkey]int{}
	stats := map[tkey]*agg{}
	var order []tkey
	n := 0
	for rows.Next() {
		var user, subject, question int64
		var topic string
		var correct bool
		if err := rows.Scan(&user, &subject, &question, &topic, &correct); err != nil {
			rows.Close()
			return err
		}
		k := qkey{user, question}
		prev := status[k]
		status[k] = models.NextStatus(prev, correct)
		if !countsForTopic(prev) {
			continue
		}
		key := models.NormalizeTopic(topic)
		if key == "" {
			continue
		}
		tk := tkey{user, subject, key}
		a := stats[tk]
		if a == nil {
			a = &agg{}
			stats[tk] = a
			order = append(order, tk)
		}
		a.topic = topic
		if correct {
			a.correct++
			a.recent = append(a.recent, '1')
		} else {
			a.wrong++
			a.recent = append(a.recent, '0')
		}
		if len(a.recent) > models.TopicWindow {
			a.recent = a.recent[len(a.recent)-models.TopicWindow:]
		}
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	const batchSize = 500
	for i := 0; i < len(order); i += batchSize {
		end := i + batchSize
		if end > len(order) {
			end = len(order)
		}
		b := &pgx.Batch{}
		for _, tk := range order[i:end] {
			a := stats[tk]
			b.Queue(`
				INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (user_id, subject_id, topic_key) DO UPDATE SET
					topic         = EXCLUDED.topic,
					correct_count = EXCLUDED.correct_count,
					wrong_count   = EXCLUDED.wrong_count,
					recent        = EXCLUDED.recent,
					updated_at    = now()`,
				tk.user, tk.subject, tk.key, a.topic, a.correct, a.wrong, string(a.recent))
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("topic stats backfill: %d historical answers replayed into %d topic rows", n, len(order))
	return nil
}

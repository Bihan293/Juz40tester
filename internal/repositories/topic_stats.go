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
// Every wrong answer is. A correct answer is, unless the question was
// already 🟢 before it: re-answering a memorised question (retaking a test
// to reach the unlock bar) proves memory of that question, not knowledge of
// the topic — counting it would wash real weaknesses out of the statistics.
func countsForTopic(prevStatus int, correct bool) bool {
	return !correct || prevStatus < models.StatusMastered
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
	type answer struct {
		user, subject, question int64
		topic                   string
		correct                 bool
	}
	var answers []answer
	for rows.Next() {
		var a answer
		if err := rows.Scan(&a.user, &a.subject, &a.question, &a.topic, &a.correct); err != nil {
			rows.Close()
			return err
		}
		answers = append(answers, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	type qkey struct{ user, question int64 }
	status := map[qkey]int{}
	n := 0
	for _, a := range answers {
		k := qkey{a.user, a.question}
		prev := status[k]
		status[k] = models.NextStatus(prev, a.correct)
		if !countsForTopic(prev, a.correct) {
			continue
		}
		if err := recordTopicAnswer(ctx, tx, a.user, a.subject, a.topic, a.correct); err != nil {
			return err
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("topic stats backfill: %d historical answers replayed", n)
	return nil
}

package repositories

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// Topic catalog (B1): subject_topics + topic_aliases, questions.topic_key.

// topicSeedMinQuestions: the one-off seed puts a topic into the catalog only
// when at least this many questions of the subject use it (one-off typos and
// exotic spellings of the model are not worth a catalog entry).
const topicSeedMinQuestions = 3

// TopicCatalog loads the catalog of a subject (titles most used first).
func (r *GenerationRepository) TopicCatalog(ctx context.Context, subjectID int64) (*models.TopicCatalog, error) {
	c := &models.TopicCatalog{SubjectID: subjectID, Aliases: map[string]string{}}
	rows, err := r.pool.Query(ctx, `
		SELECT st.title
		FROM subject_topics st
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS n FROM questions q
			WHERE q.subject_id = st.subject_id AND q.topic_key = st.topic_key
		) cnt ON TRUE
		WHERE st.subject_id = $1
		ORDER BY cnt.n DESC, st.topic_key`, subjectID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		c.Titles = append(c.Titles, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = r.pool.Query(ctx, `
		SELECT alias_norm, topic_key FROM topic_aliases WHERE subject_id = $1`, subjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, k string
		if err := rows.Scan(&a, &k); err != nil {
			return nil, err
		}
		c.Aliases[a] = k
	}
	return c, rows.Err()
}

// ResolveTopicKey maps a raw topic title of the subject to its topic_key:
// normalisation (models.NormalizeTopic) + alias lookup. ok=false when the
// topic is not in the catalog.
func (r *GenerationRepository) ResolveTopicKey(ctx context.Context, subjectID int64, rawTitle string) (key string, ok bool, err error) {
	n := models.NormalizeTopic(rawTitle)
	if n == "" {
		return "", false, nil
	}
	err = r.pool.QueryRow(ctx, `
		SELECT topic_key FROM topic_aliases WHERE subject_id = $1 AND alias_norm = $2`,
		subjectID, n).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// ensureTopicKeys resolves the topics of new questions to topic_key inside
// the creating transaction. A topic missing from the catalog (validators
// allow only a few, see validateChainTopics) is registered as a new catalog
// topic with itself as the alias, so every stored question gets a key.
// Empty topics stay unmapped.
func ensureTopicKeys(ctx context.Context, tx pgx.Tx, subjectID int64, topics []string) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range topics {
		n := models.NormalizeTopic(raw)
		if n == "" {
			continue
		}
		if _, done := out[n]; done {
			continue
		}
		var key string
		err := tx.QueryRow(ctx, `
			SELECT topic_key FROM topic_aliases WHERE subject_id = $1 AND alias_norm = $2`,
			subjectID, n).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `
				INSERT INTO subject_topics (subject_id, topic_key, title)
				VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
				subjectID, n, strings.Join(strings.Fields(raw), " ")); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO topic_aliases (subject_id, alias_norm, topic_key)
				VALUES ($1, $2, $2) ON CONFLICT DO NOTHING`, subjectID, n); err != nil {
				return nil, err
			}
			err = tx.QueryRow(ctx, `
				SELECT topic_key FROM topic_aliases WHERE subject_id = $1 AND alias_norm = $2`,
				subjectID, n).Scan(&key)
		}
		if err != nil {
			return nil, err
		}
		out[n] = key
	}
	return out, nil
}

// BackfillTopicCatalog (one-off, marker "topic_catalog_v1") seeds the topic
// catalog from the existing questions and sets questions.topic_key:
//   - topics are grouped by models.NormalizeTopic per subject;
//   - a group used by >= topicSeedMinQuestions questions becomes a catalog
//     topic: key = normalised form, title = the most frequent spelling;
//   - the normalised form is its alias (more aliases are added by SQL);
//   - questions are mapped through the aliases; the rest stay NULL (logged).
//
// Normalisation runs in Go (SQL lower() depends on the DB ctype).
func (r *GenerationRepository) BackfillTopicCatalog(ctx context.Context) error {
	const name = "topic_catalog_v1"
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
		return nil
	}
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx, `
		SELECT subject_id, topic, COUNT(*)
		FROM questions
		WHERE topic <> '' AND topic_key IS NULL
		GROUP BY subject_id, topic`)
	if err != nil {
		return err
	}
	type form struct {
		subject int64
		raw     string
		n       int
	}
	type gkey struct {
		subject int64
		norm    string
	}
	var forms []form
	total := map[gkey]int{}
	best := map[gkey]form{}
	for rows.Next() {
		var f form
		if err := rows.Scan(&f.subject, &f.raw, &f.n); err != nil {
			rows.Close()
			return err
		}
		forms = append(forms, f)
		k := gkey{f.subject, models.NormalizeTopic(f.raw)}
		if k.norm == "" {
			continue
		}
		total[k] += f.n
		if b, ok := best[k]; !ok || f.n > b.n || (f.n == b.n && f.raw < b.raw) {
			best[k] = f
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	topics := 0
	for k, n := range total {
		if n < topicSeedMinQuestions {
			continue
		}
		title := strings.Join(strings.Fields(best[k].raw), " ")
		if _, err := tx.Exec(ctx, `
			INSERT INTO subject_topics (subject_id, topic_key, title)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, k.subject, k.norm, title); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO topic_aliases (subject_id, alias_norm, topic_key)
			VALUES ($1, $2, $2) ON CONFLICT DO NOTHING`, k.subject, k.norm); err != nil {
			return err
		}
		topics++
	}

	// Map every spelling through the aliases (also aliases that existed
	// before the seed).
	mapped, unmapped := 0, 0
	for _, f := range forms {
		n := models.NormalizeTopic(f.raw)
		var key string
		err := tx.QueryRow(ctx, `
			SELECT topic_key FROM topic_aliases WHERE subject_id = $1 AND alias_norm = $2`,
			f.subject, n).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			unmapped += f.n
			continue
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE questions SET topic_key = $3
			WHERE subject_id = $1 AND topic = $2 AND topic_key IS NULL`, f.subject, f.raw, key)
		if err != nil {
			return err
		}
		mapped += int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	log.Printf("topic catalog backfill: %d topics seeded, %d questions mapped, %d questions left without topic_key", topics, mapped, unmapped)
	return nil
}

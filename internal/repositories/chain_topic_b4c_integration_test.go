package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestChainQuestionsTopicKey (B4c): an audited chain question with a known
// topic (alias) gets its topic_key and stays quality-checked (visible to the
// bank); the one-off backfill maps old chain questions through the aliases,
// an old question with an unknown topic stays NULL.
func TestChainQuestionsTopicKey(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Темы B4c "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO subject_topics (subject_id, topic_key, title) VALUES ($1, 'генетика', 'Генетика');
	`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO topic_aliases (subject_id, alias_norm, topic_key)
		VALUES ($1, 'генетика', 'генетика'), ($1, 'наследственность', 'генетика')`, sid); err != nil {
		t.Fatal(err)
	}

	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "T1", Kind: models.TestKindChain}, []models.SeedQuestion{
		{Text: "B4c известная тема", Options: [4]string{"a", "b", "c", "d"}, Topic: "Наследственность", Difficulty: 2, QualityChecked: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		SELECT q.topic_key, q.quality_checked_at IS NOT NULL FROM test_questions tq JOIN questions q ON q.id = tq.question_id
		WHERE tq.test_id = $1 ORDER BY tq.position`, test.ID)
	if err != nil {
		t.Fatal(err)
	}
	var keys []*string
	for rows.Next() {
		var k *string
		var checked bool
		if err := rows.Scan(&k, &checked); err != nil {
			t.Fatal(err)
		}
		if !checked {
			t.Fatal("audited chain question must keep quality_checked_at")
		}
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 1 || keys[0] == nil || *keys[0] != "генетика" {
		t.Fatalf("chain topic keys: %v", keys)
	}

	// Backfill: an old chain question without topic_key gets it via alias,
	// an unknown one stays NULL.
	var oldKnown, oldUnknown int64
	for i, topic := range []string{"генетика ", "Неведомое"} {
		var qid int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d, correct_answer, topic, difficulty, quality_checked_at)
			VALUES ($1, $2, 'a','b','c','d','A', $3, 2, now()) RETURNING id`,
			sid, "B4c старый "+topic, topic).Scan(&qid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO test_questions (test_id, question_id, position) VALUES ($1, $2, $3)`, test.ID, qid, 10+i); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldKnown = qid
		} else {
			oldUnknown = qid
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM app_backfills WHERE name = 'chain_topic_keys_v1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := gen.BackfillChainTopicKeys(ctx); err != nil {
		t.Fatal(err)
	}
	var k1, k2 *string
	if err := pool.QueryRow(ctx, `SELECT topic_key FROM questions WHERE id = $1`, oldKnown).Scan(&k1); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT topic_key FROM questions WHERE id = $1`, oldUnknown).Scan(&k2); err != nil {
		t.Fatal(err)
	}
	if k1 == nil || *k1 != "генетика" || k2 != nil {
		t.Fatalf("backfill: known=%v unknown=%v", k1, k2)
	}
}

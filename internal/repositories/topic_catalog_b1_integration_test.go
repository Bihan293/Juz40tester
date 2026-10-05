package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestTopicCatalogBackfillAndResolve (B1): the one-off seed builds the
// catalog from existing questions (topics used by >= 3 questions, most
// frequent spelling as title), maps questions.topic_key through the aliases
// and leaves rare topics unmapped; new questions always get a topic_key.
func TestTopicCatalogBackfillAndResolve(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Темы B1 "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	ins := func(topic string, n int) {
		for i := 0; i < n; i++ {
			if _, err := pool.Exec(ctx, `
				INSERT INTO questions (subject_id, question_text, option_a, option_b, option_c, option_d, correct_answer, topic, difficulty)
				VALUES ($1, $2, 'a','b','c','d','A', $3, 2)`, sid, topic+" вопрос "+time.Now().Format("150405.000000000"), topic); err != nil {
				t.Fatal(err)
			}
		}
	}
	ins("Генетика", 2)
	ins("генетика ", 1)
	ins("ГЕНЕТИКА", 1) // 4 questions -> catalog topic, title "Генетика"
	ins("Редкая тема", 1)

	if _, err := pool.Exec(ctx, `DELETE FROM app_backfills WHERE name = 'topic_catalog_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := gen.BackfillTopicCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM subject_topics WHERE subject_id = $1 AND topic_key = 'генетика'`, sid).Scan(&title); err != nil || title != "Генетика" {
		t.Fatalf("seed: title=%q err=%v", title, err)
	}
	var mapped, unmapped int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE topic_key = 'генетика'), COUNT(*) FILTER (WHERE topic_key IS NULL)
		FROM questions WHERE subject_id = $1`, sid).Scan(&mapped, &unmapped); err != nil {
		t.Fatal(err)
	}
	if mapped != 4 || unmapped != 1 {
		t.Fatalf("mapped=%d unmapped=%d, want 4 / 1", mapped, unmapped)
	}

	// An alias added by SQL resolves to the topic.
	if _, err := pool.Exec(ctx, `INSERT INTO topic_aliases (subject_id, alias_norm, topic_key) VALUES ($1, 'наследственность', 'генетика')`, sid); err != nil {
		t.Fatal(err)
	}
	if k, ok, err := gen.ResolveTopicKey(ctx, sid, "  Наследственность "); err != nil || !ok || k != "генетика" {
		t.Fatalf("resolve alias: %q %v %v", k, ok, err)
	}
	if _, ok, _ := gen.ResolveTopicKey(ctx, sid, "Редкая тема"); ok {
		t.Fatal("rare topic must not be in the catalog")
	}
	cat, err := gen.TopicCatalog(ctx, sid)
	if err != nil || len(cat.Titles) != 1 || cat.Titles[0] != "Генетика" {
		t.Fatalf("catalog: %+v %v", cat, err)
	}

	// New questions: an alias gets the existing key, a new topic is
	// registered in the catalog — every stored question has a topic_key.
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "T1", Kind: models.TestKindChain}, []models.SeedQuestion{
		{Text: "Новый вопрос про наследственность", Options: [4]string{"a", "b", "c", "d"}, Topic: "Наследственность", Difficulty: 2},
		{Text: "Новый вопрос про клетку", Options: [4]string{"a", "b", "c", "d"}, Topic: "Клетка", Difficulty: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		SELECT q.topic_key FROM test_questions tq JOIN questions q ON q.id = tq.question_id
		WHERE tq.test_id = $1 ORDER BY tq.position`, test.ID)
	if err != nil {
		t.Fatal(err)
	}
	var keys []*string
	for rows.Next() {
		var k *string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 2 || keys[0] == nil || *keys[0] != "генетика" || keys[1] == nil || *keys[1] != "клетка" {
		t.Fatalf("topic keys of new questions: %v", keys)
	}
	if k, ok, _ := gen.ResolveTopicKey(ctx, sid, "клетка"); !ok || k != "клетка" {
		t.Fatal("new topic must be registered in the catalog")
	}
}

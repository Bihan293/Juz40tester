package repositories

import (
	"context"
	"sync"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// B4 (part 1): concurrent enqueues create one active batch per topic and a
// claimed batch carries its topic_key; saved bank questions get topic_key
// and quality_checked_at.
func TestTopicBatchEnqueueAndSaveB4(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	keys := []string{"б1", "б2"}
	sid := bankFixture(t, ctx, pool, "Банк B4", keys, 0)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gen.EnqueueTopicBatchJobs(ctx, sid, []string{"б1", "б2", "б1"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	active, err := gen.ActiveTopicBatchKeys(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[0] != "б1" || active[1] != "б2" {
		t.Fatalf("active batches = %v, want [б1 б2]", active)
	}

	var jobKey string
	if err := pool.QueryRow(ctx, `SELECT topic_key FROM generation_jobs WHERE subject_id = $1 AND kind = 'topic_batch' ORDER BY id LIMIT 1`, sid).Scan(&jobKey); err != nil {
		t.Fatal(err)
	}
	if jobKey == "" {
		t.Fatal("topic_batch job without topic_key")
	}

	qs := []models.SeedQuestion{{Text: "q1", Options: [4]string{"a", "b", "c", "d"}, Correct: 1, Topic: "б1", Difficulty: 2}}
	ids, err := gen.SaveBankQuestions(ctx, sid, "б1", qs)
	if err != nil || len(ids) != 1 {
		t.Fatalf("SaveBankQuestions: ids=%v err=%v", ids, err)
	}
	var key string
	var checked bool
	if err := pool.QueryRow(ctx, `SELECT topic_key, quality_checked_at IS NOT NULL FROM questions WHERE id = $1`, ids[0]).Scan(&key, &checked); err != nil {
		t.Fatal(err)
	}
	if key != "б1" || !checked {
		t.Fatalf("bank question key=%q checked=%v", key, checked)
	}
	if _, err := gen.SaveBankQuestions(ctx, sid, "нет-такой", qs); err == nil {
		t.Fatal("unknown topic_key must be rejected")
	}
}

// B4a: two concurrent EnqueueTopicBatch calls on one topic create one job;
// a banked question text is not stored twice in the same topic.
func TestEnqueueTopicBatchConcurrentB4a(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	sid := bankFixture(t, ctx, pool, "Банк B4a", []string{"т1"}, 0)

	var wg sync.WaitGroup
	created := make([]bool, 2)
	for i := range created {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := gen.EnqueueTopicBatch(ctx, sid, "т1")
			if err != nil {
				t.Error(err)
			}
			created[i] = ok
		}(i)
	}
	wg.Wait()
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM generation_jobs WHERE subject_id = $1 AND kind = 'topic_batch'`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || created[0] == created[1] {
		t.Fatalf("jobs=%d created=%v, want exactly one new job", n, created)
	}

	qs := []models.SeedQuestion{
		{Text: "Один вопрос", Options: [4]string{"a", "b", "c", "d"}, Correct: 0, Topic: "т1", Difficulty: 2},
		{Text: "  один ВОПРОС ", Options: [4]string{"a", "b", "c", "d"}, Correct: 1, Topic: "т1", Difficulty: 2},
	}
	ids, err := gen.SaveBankQuestions(ctx, sid, "т1", qs)
	if err != nil || len(ids) != 1 {
		t.Fatalf("SaveBankQuestions dedupe: ids=%v err=%v", ids, err)
	}
	title, texts, err := gen.TopicBankInfo(ctx, sid, "т1", 10)
	if err != nil || title != "т1" || len(texts) != 1 {
		t.Fatalf("TopicBankInfo: %q %v %v", title, texts, err)
	}
}

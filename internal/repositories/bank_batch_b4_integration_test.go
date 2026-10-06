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

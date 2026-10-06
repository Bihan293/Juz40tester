package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// B4a: the worker processes a topic_batch job — TopicBatchSize audited
// questions on the topic land in the bank (topic_key, quality_checked_at),
// the job is done without a test, the prompt names the catalog topic.
func TestTopicBatchWorkerB4a(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	var calls int32
	var sawTopic atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 1 && strings.Contains(req.Messages[1].Content, "«Генетика»") {
			sawTopic.Store(true)
		}
		qs := validQuestions()[:repositories.TopicBatchSize]
		for i := range qs {
			qs[i].Topic = "что-то другое" // pinned to the catalog title
		}
		b, _ := json.Marshal(generatedTest{Questions: qs})
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": string(b)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	subjects := repositories.NewSubjectRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	g := NewGeneratorService(nil, &config.Config{}, gen, subjects, state).WithGroq(groq.New("k", srv.URL))

	sid, err := testutil.CreateSubject(ctx, pool, "Биология B4a "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO subject_topics (subject_id, topic_key, title) VALUES ($1, 'генетика', 'Генетика')`, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE generation_jobs SET status = 'done' WHERE status IN ('pending','running')`); err != nil {
		t.Fatal(err)
	}
	if ok, err := gen.EnqueueTopicBatch(ctx, sid, "генетика"); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}
	job, err := gen.ClaimNextJob(ctx)
	if err != nil || job == nil || job.Kind != models.JobKindTopicBatch || job.TopicKey != "генетика" {
		t.Fatalf("claim: %+v %v", job, err)
	}
	g.executeJob(ctx, job)

	var status string
	var testID *int64
	if err := pool.QueryRow(ctx, `SELECT status, test_id FROM generation_jobs WHERE id = $1`, job.ID).Scan(&status, &testID); err != nil {
		t.Fatal(err)
	}
	if status != "done" || testID != nil {
		t.Fatalf("job status=%s test_id=%v", status, testID)
	}
	var n, bad int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE quality_checked_at IS NULL OR topic <> 'Генетика')
		FROM questions WHERE subject_id = $1 AND topic_key = 'генетика'`, sid).Scan(&n, &bad); err != nil {
		t.Fatal(err)
	}
	if n != repositories.TopicBatchSize || bad != 0 {
		t.Fatalf("bank questions=%d bad=%d", n, bad)
	}
	if !sawTopic.Load() {
		t.Fatal("prompt does not name the catalog topic")
	}

	// A second batch returns the same texts: all duplicates, nothing new.
	if _, err := gen.EnqueueTopicBatch(ctx, sid, "генетика"); err != nil {
		t.Fatal(err)
	}
	job2, err := gen.ClaimNextJob(ctx)
	if err != nil || job2 == nil {
		t.Fatalf("claim 2: %v", err)
	}
	g.executeJob(ctx, job2)
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions WHERE subject_id = $1 AND topic_key = 'генетика'`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != repositories.TopicBatchSize {
		t.Fatalf("duplicates stored: %d questions", n)
	}
}

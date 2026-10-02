package services

// End-to-end test of the generation pipeline with a fake Groq server and a
// real PostgreSQL (skipped without TEST_DATABASE_URL):
//
//	model writes a test with the reported «тире» giveaway question →
//	post-validation flags it → targeted repair call rewrites it →
//	the stored test contains only clean questions;
//	plus the background sweep fixing a legacy question in the database.

import (
	"context"
	"encoding/json"
	"io"
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

var badDash = generatedQuestion{
	Text:    "В каком предложении нужно поставить тире?",
	Options: []string{"Наступила зима.", "Книга лежит на столе.", "Я люблю русский язык.", "Москва — столица России."},
	Correct: 3, Topic: "Тире", Difficulty: 1,
}

var goodDash = generatedQuestion{
	Text:    "В каком предложении на месте пропуска нужно поставить тире?",
	Options: []string{"Зимой _ здесь очень холодно.", "Москва _ столица России.", "Книга _ лежит на столе.", "Он _ мой старый друг."},
	Correct: 1, Topic: "Тире", Difficulty: 1,
}

func fakeGroq(t *testing.T, genCalls, repairCalls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		var content string
		if strings.Contains(req.Messages[0].Content, "редактор тестов") {
			atomic.AddInt32(repairCalls, 1)
			// One rewrite per requested question («N. {json}» lines).
			n := strings.Count(req.Messages[1].Content, "Причины брака:")
			qs := make([]generatedQuestion, n)
			for i := range qs {
				qs[i] = goodDash
				qs[i].Topic = "попытка сменить тему" // must be pinned back
			}
			b, _ := json.Marshal(generatedTest{Questions: qs})
			content = string(b)
		} else {
			atomic.AddInt32(genCalls, 1)
			qs := validQuestions()
			qs[7] = badDash
			b, _ := json.Marshal(generatedTest{Questions: qs})
			content = string(b)
		}
		b, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
		_, _ = w.Write(b)
	}))
}

func TestPipelineRepairsGiveawayQuestion(t *testing.T) {
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
	// Isolate from questions left by other test runs.
	if _, err := pool.Exec(ctx, `UPDATE questions SET quality_checked_at = now() WHERE quality_checked_at IS NULL`); err != nil {
		t.Fatal(err)
	}

	var genCalls, repairCalls int32
	srv := fakeGroq(t, &genCalls, &repairCalls)
	defer srv.Close()

	subjects := repositories.NewSubjectRepository(pool)
	gen := repositories.NewGenerationRepository(pool)
	state := repositories.NewStateRepository(pool)
	g := NewGeneratorService(nil, &config.Config{}, gen, subjects, state).WithGroq(groq.New("k", srv.URL))

	sid, err := testutil.CreateSubject(ctx, pool, "Русский язык "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	testID, err := g.runJob(ctx, &models.GenerationJob{ID: 1, Kind: models.TestKindChain, SubjectID: sid, TestNumber: 1, Attempts: 1})
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if atomic.LoadInt32(&repairCalls) != 1 {
		t.Fatalf("expected exactly one targeted repair call, got %d (gen calls %d)", repairCalls, genCalls)
	}
	qs, err := subjects.TestQuestions(ctx, testID)
	if err != nil || len(qs) != GeneratedQuestionsPerTest {
		t.Fatalf("stored: %d %v", len(qs), err)
	}
	for _, q := range qs {
		if rep := auditQuestion(q.Text, q.Options(), letterIndex(q.CorrectAnswer)); rep.HasHard() {
			t.Fatalf("stored question still flagged: %q — %s", q.Text, rep.Reasons())
		}
		if strings.Contains(q.OptionD, "Москва — столица") {
			t.Fatal("the giveaway question reached the database")
		}
	}
	if qs[7].Text != goodDash.Text || qs[7].Topic != "Тире" || qs[7].CorrectAnswer != "B" {
		t.Fatalf("question 8 not repaired correctly: %+v", qs[7])
	}

	// --- Sweep over legacy stored questions ---------------------------
	legacy := make([]models.SeedQuestion, 0, GeneratedQuestionsPerTest)
	for _, q := range validQuestions() {
		var o [4]string
		copy(o[:], q.Options)
		legacy = append(legacy, models.SeedQuestion{Text: q.Text + " (old)", Options: o, Correct: q.Correct, Topic: q.Topic, Difficulty: 2})
	}
	var bo [4]string
	copy(bo[:], badDash.Options)
	legacy[3] = models.SeedQuestion{Text: badDash.Text, Options: bo, Correct: 3, Topic: "Тире", Difficulty: 1}
	old, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 2, Title: "Тест 2", Kind: models.TestKindChain}, legacy)
	if err != nil {
		t.Fatal(err)
	}
	before := atomic.LoadInt32(&repairCalls)
	n, err := g.RunQualitySweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || atomic.LoadInt32(&repairCalls) != before+1 {
		t.Fatalf("sweep repaired %d (repair calls %d→%d)", n, before, repairCalls)
	}
	oqs, _ := subjects.TestQuestions(ctx, old.ID)
	if oqs[3].Text != goodDash.Text || oqs[3].CorrectAnswer != "B" {
		t.Fatalf("legacy question not rewritten: %+v", oqs[3])
	}
	// Second sweep: nothing left, no AI call.
	before = atomic.LoadInt32(&repairCalls)
	if n, err := g.RunQualitySweep(ctx); err != nil || n != 0 || atomic.LoadInt32(&repairCalls) != before {
		t.Fatalf("second sweep must be a no-op: n=%d err=%v", n, err)
	}
}

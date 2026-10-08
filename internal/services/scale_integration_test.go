package services

// Integration tests of the scale features (skipped without
// TEST_DATABASE_URL): batch generation, batch reuse on retry, fingerprint
// templates (identical tests for identical weak topics — personal and
// chain), deterministic bank selection, subject-wide weak topics, queue
// backpressure and strategy statistics.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

var reSpecLine = regexp.MustCompile(`(?m)^(\d+)\) тема: (?:«([^»]+)»[^\n]*|любая[^\n]*), difficulty (\d)$`)

// batchFakeAI answers batch prompts exactly per spec (topic + difficulty)
// and full prompts with 20 questions. failFrom > 0: every call number ≥
// failFrom returns garbage (simulates a provider outage mid-job).
type batchFakeAI struct {
	calls    atomic.Int32
	seq      atomic.Int32
	failFrom int32
	mu       sync.Mutex
	prompts  []string
}

func (f *batchFakeAI) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		user := req.Messages[len(req.Messages)-1].Content
		n := f.calls.Add(1)
		f.mu.Lock()
		f.prompts = append(f.prompts, user)
		f.mu.Unlock()
		content := `{"questions":[]}`
		if f.failFrom == 0 || n < f.failFrom {
			var qs []generatedQuestion
			keys := []string{"ключ", "ответ", "вариант", "решение"}
			for _, m := range reSpecLine.FindAllStringSubmatch(user, -1) {
				d, _ := strconv.Atoi(m[3])
				topic := m[2]
				if topic == "" {
					topic = "Общая тема"
				}
				s := f.seq.Add(1)
				qs = append(qs, generatedQuestion{
					Text:    fmt.Sprintf("Уникальный вопрос номер %d", s),
					Options: []string{"первый " + keys[s%4], "второй " + keys[s%4], "третий " + keys[s%4], "четвёртый " + keys[s%4]},
					Correct: int(s % 4), Topic: topic, Difficulty: d,
				})
			}
			b, _ := json.Marshal(generatedTest{Questions: qs})
			content = string(b)
		}
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
		_, _ = w.Write(out)
	}))
}

func scaleEnv(t *testing.T, f *batchFakeAI, cfg *config.Config) (*flowEnv, *GeneratorService) {
	t.Helper()
	liftGroqLimits(t)
	e := newFlowEnv(t)
	srv := f.server(t)
	t.Cleanup(srv.Close)
	g := NewGeneratorService(nil, cfg, e.gen, e.subjects, repositories.NewStateRepository(e.pool)).WithGroq(groq.New("k", srv.URL))
	return e, g
}

func batchCfg() *config.Config {
	return &config.Config{GenStrategy: config.GenStrategyBatch, GenBatchSize: 5, GenBatchParallel: 2, GenTemplateReuse: true, GenABBatchPercent: 50}
}

func newChainJob(t *testing.T, e *flowEnv, n int) *models.GenerationJob {
	t.Helper()
	ctx := context.Background()
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, n); err != nil {
		t.Fatal(err)
	}
	job, err := e.claimOwn(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	return job
}

func TestBatchStrategyGeneratesChainTest(t *testing.T) {
	f := &batchFakeAI{}
	e, g := scaleEnv(t, f, batchCfg())
	ctx := context.Background()
	job := newChainJob(t, e, 1)
	g.executeJob(ctx, job)
	var status, strategy string
	var testID *int64
	if err := e.pool.QueryRow(ctx, `SELECT status, strategy, test_id FROM generation_jobs WHERE id = $1`, job.ID).Scan(&status, &strategy, &testID); err != nil {
		t.Fatal(err)
	}
	if status != "done" || strategy != "batch" || testID == nil {
		t.Fatalf("job: %s %s %v", status, strategy, testID)
	}
	if c := f.calls.Load(); c != 4 {
		t.Fatalf("20 questions in batches of 5 must take 4 calls, took %d", c)
	}
	qs, err := e.subjects.TestQuestions(ctx, *testID)
	if err != nil || len(qs) != GeneratedQuestionsPerTest {
		t.Fatalf("stored %d questions: %v", len(qs), err)
	}
	if m := meanDifficulty(qs); m < chainDifficultyTarget(1)-0.2 || m > chainDifficultyTarget(1)+0.2 {
		t.Fatalf("mean difficulty %.2f, target %.1f", m, chainDifficultyTarget(1))
	}
	var batches, stats, tpl int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM gen_job_batches WHERE job_id = $1`, job.ID).Scan(&batches)
	_ = e.pool.QueryRow(ctx, `SELECT COALESCE(SUM(jobs_done),0) FROM gen_strategy_stats WHERE strategy = 'batch' AND kind = 'chain'`).Scan(&stats)
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM test_templates WHERE subject_id = $1 AND kind = 'chain' AND source_test_id = $2`, e.sid, *testID).Scan(&tpl)
	if batches != 0 || stats < 1 || tpl != 1 {
		t.Fatalf("batches left %d (want 0), strategy stats %d, templates %d", batches, stats, tpl)
	}
}

func TestBatchRetryReusesPaidQuestions(t *testing.T) {
	f := &batchFakeAI{failFrom: 3} // calls 1–2 succeed, then the provider breaks
	e, g := scaleEnv(t, f, batchCfg())
	ctx := context.Background()
	g.cfg.GenBatchParallel = 1
	job := newChainJob(t, e, 1)
	if _, err := g.runJob(ctx, job); err == nil {
		t.Fatal("the job must fail while the provider is broken")
	}
	var saved int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM gen_job_batches WHERE job_id = $1`, job.ID).Scan(&saved)
	if saved != 10 {
		t.Fatalf("persisted %d questions of the 2 good batches, want 10", saved)
	}
	f.failFrom = 0
	before := f.calls.Load()
	job.Attempts = 2
	id, err := g.runJob(ctx, job)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if c := f.calls.Load() - before; c != 2 {
		t.Fatalf("the retry must only generate the 2 missing batches, made %d calls", c)
	}
	if qs, _ := e.subjects.TestQuestions(ctx, id); len(qs) != GeneratedQuestionsPerTest {
		t.Fatalf("stored %d", len(qs))
	}
}

func setWeak(t *testing.T, e *flowEnv, user int64, topics ...string) {
	t.Helper()
	for _, tp := range topics {
		if _, err := e.pool.Exec(context.Background(), `
			INSERT INTO user_topic_stats (user_id, subject_id, topic_key, topic, correct_count, wrong_count, recent)
			VALUES ($1, $2, $3, $4, 0, 10, '0000000000')
			ON CONFLICT (user_id, subject_id, topic_key) DO UPDATE SET recent = EXCLUDED.recent`,
			user, e.sid, models.NormalizeTopic(tp), tp); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPersonalTemplateCloneForSameWeakTopics(t *testing.T) {
	f := &batchFakeAI{}
	e, g := scaleEnv(t, f, batchCfg())
	g.noBank = true // the bank is empty anyway; force the generation path
	ctx := context.Background()
	a, b, c := e.user(t, "A"), e.user(t, "B"), e.user(t, "C")
	setWeak(t, e, a, "Генетика", "Клетка")
	setWeak(t, e, b, "клетка", "ГЕНЕТИКА") // same set, other spelling/order
	setWeak(t, e, c, "Экология")

	if _, pending, _, err := g.EnsurePersonalTest(ctx, a, e.sid); err != nil || !pending {
		t.Fatalf("A must wait for a generation: %v %v", pending, err)
	}
	job, err := e.claimOwn(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	g.executeJob(ctx, job)
	ta, _ := e.gen.FindPersonalTest(ctx, e.sid, a)
	if ta == nil || ta.TopicsFingerprint == "" {
		t.Fatalf("A's test: %+v", ta)
	}
	calls := f.calls.Load()

	tb, pending, _, err := g.EnsurePersonalTest(ctx, b, e.sid)
	if err != nil || pending || tb == nil {
		t.Fatalf("B must get a clone at once: test=%v pending=%v err=%v", tb, pending, err)
	}
	if f.calls.Load() != calls {
		t.Fatal("a clone must not call the AI")
	}
	qa, _ := e.gen.PersonalTestQuestions(ctx, ta.ID)
	qb, _ := e.gen.PersonalTestQuestions(ctx, tb.ID)
	if len(qa) != 20 || len(qb) != 20 {
		t.Fatalf("sizes %d %d", len(qa), len(qb))
	}
	for i := range qa {
		if qa[i].Text != qb[i].Text || qa[i].Correct != qb[i].Correct || qa[i].Options != qb[i].Options {
			t.Fatalf("question %d differs between the twins", i)
		}
	}
	idsA, _ := e.subjects.TestQuestionIDs(ctx, ta.ID)
	idsB, _ := e.subjects.TestQuestionIDs(ctx, tb.ID)
	if idsA[0] == idsB[0] {
		t.Fatal("a clone must get NEW question rows")
	}
	if tb.TopicsFingerprint != ta.TopicsFingerprint {
		t.Fatal("fingerprints differ")
	}
	// Different weak topics: no clone, a generation is queued.
	if tc, pending, _, err := g.EnsurePersonalTest(ctx, c, e.sid); err != nil || tc != nil || !pending {
		t.Fatalf("C has other weak topics: test=%v pending=%v err=%v", tc, pending, err)
	}
	// B finishes the clone; same weak topics → the template is not reused for B again.
	if err := e.gen.DeletePersonalTest(ctx, b, tb.ID, e.sid); err != nil {
		t.Fatal(err)
	}
	if t2, _, _, err := g.EnsurePersonalTest(ctx, b, e.sid); err != nil || t2 != nil {
		t.Fatalf("B must not receive the same template twice: %v %v", t2, err)
	}
	_, _ = e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE subject_id = $1 AND status IN ('pending','running')`, e.sid)
}

func TestChainTemplateReusedOnRegeneration(t *testing.T) {
	f := &batchFakeAI{}
	e, g := scaleEnv(t, f, batchCfg())
	ctx := context.Background()
	job := newChainJob(t, e, 1)
	id1, err := g.runJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	_ = e.gen.CompleteJob(ctx, job.ID, id1)
	// The chain test disappears (deleted manually) and is requested again.
	if _, err := e.pool.Exec(ctx, `DELETE FROM tests WHERE id = $1`, id1); err != nil {
		t.Fatal(err)
	}
	repositories.InvalidateChainCache(e.sid)
	calls := f.calls.Load()
	id2, err := g.runJob(ctx, &models.GenerationJob{ID: job.ID, Kind: models.TestKindChain, SubjectID: e.sid, TestNumber: 1, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != calls || id2 == id1 {
		t.Fatalf("regeneration with identical inputs must clone the template (calls %d→%d)", calls, f.calls.Load())
	}
	// GEN_TEMPLATE_REUSE=false: a fresh generation.
	_, _ = e.pool.Exec(ctx, `DELETE FROM tests WHERE id = $1`, id2)
	repositories.InvalidateChainCache(e.sid)
	g.cfg.GenTemplateReuse = false
	if _, err := g.runJob(ctx, &models.GenerationJob{ID: job.ID, Kind: models.TestKindChain, SubjectID: e.sid, TestNumber: 1, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() == calls {
		t.Fatal("with reuse disabled the test must be generated")
	}
}

func TestChainWeakTopicsReachPromptAndAreCovered(t *testing.T) {
	f := &batchFakeAI{}
	e, g := scaleEnv(t, f, batchCfg())
	ctx := context.Background()
	// Subject-wide weak topic: «Экология» is weak for 3 students.
	for i := 0; i < 3; i++ {
		setWeak(t, e, e.user(t, fmt.Sprintf("S%d", i)), "Экология")
	}
	subjectWeakCache.m = map[int64]subjectWeakEntry{}
	rows, err := e.gen.SubjectWeakTopics(ctx, e.sid, subjectWeakWindow, 3, 5)
	if err != nil || len(rows) != 1 || rows[0].Key != "экология" || rows[0].WeakUsers != 3 {
		t.Fatalf("subject weak topics: %+v %v", rows, err)
	}
	job := newChainJob(t, e, 1)
	id, err := g.runJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	p := f.prompts[0]
	f.mu.Unlock()
	if !strings.Contains(p, "СЛАБЫЕ ТЕМЫ") || !strings.Contains(p, "Экология") {
		t.Fatalf("the weak topic must reach the prompt:\n%s", p)
	}
	qs, _ := e.subjects.TestQuestions(ctx, id)
	n := 0
	for _, q := range qs {
		if q.Topic == "Экология" {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("weak topic covered by %d questions, want ≥ 2", n)
	}
}

func TestBankSelectionIdenticalForSameFingerprint(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	var seed []models.SeedQuestion
	for i := 0; i < 30; i++ {
		seed = append(seed, models.SeedQuestion{
			Text: fmt.Sprintf("Банк вопрос %d", i), Options: [4]string{"а1", "б1", "в1", "г1"},
			Correct: i % 4, Topic: []string{"Генетика", "Клетка"}[i%2], Difficulty: 2,
		})
	}
	for _, k := range []string{"Генетика", "Клетка"} {
		var part []models.SeedQuestion
		for _, q := range seed {
			if q.Topic == k {
				part = append(part, q)
			}
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO subject_topics (subject_id, topic_key, title) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, e.sid, models.NormalizeTopic(k), k); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.Exec(ctx, `INSERT INTO topic_aliases (subject_id, alias_norm, topic_key) VALUES ($1, $2, $2) ON CONFLICT DO NOTHING`, e.sid, models.NormalizeTopic(k)); err != nil {
			t.Fatal(err)
		}
		if _, err := e.gen.SaveBankQuestions(ctx, e.sid, models.NormalizeTopic(k), part); err != nil {
			t.Fatal(err)
		}
	}
	keys := []string{"генетика", "клетка"}
	a, b := e.user(t, "A"), e.user(t, "B")
	ta, _, err := e.gen.AssembleBankPersonalTest(ctx, e.sid, a, keys, []string{"Генетика", "Клетка"}, 10, "x")
	if err != nil || ta == nil {
		t.Fatalf("A: %v", err)
	}
	tb, _, err := e.gen.AssembleBankPersonalTest(ctx, e.sid, b, []string{"клетка", "генетика"}, []string{"Клетка", "Генетика"}, 10, "x")
	if err != nil || tb == nil {
		t.Fatalf("B: %v", err)
	}
	ia, _ := e.subjects.TestQuestionIDs(ctx, ta.ID)
	ib, _ := e.subjects.TestQuestionIDs(ctx, tb.ID)
	sa := map[int64]bool{}
	for _, id := range ia {
		sa[id] = true
	}
	for _, id := range ib {
		if !sa[id] {
			t.Fatalf("same weak topics must give the same bank questions: %v vs %v", ia, ib)
		}
	}
	if ta.TopicsFingerprint == "" || ta.TopicsFingerprint != models.WeakTopicsFingerprint(e.sid, keys) {
		t.Fatalf("bank test fingerprint %q", ta.TopicsFingerprint)
	}
}

func TestPersonalQueueBackpressure(t *testing.T) {
	f := &batchFakeAI{}
	cfg := batchCfg()
	cfg.GenMaxActivePersonal = 1
	e, g := scaleEnv(t, f, cfg)
	g.noBank = true
	ctx := context.Background()
	_, _ = e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE status IN ('pending','running')`)
	a, b := e.user(t, "A"), e.user(t, "B")
	setWeak(t, e, a, "Тема X")
	setWeak(t, e, b, "Тема Y")
	if _, pending, _, err := g.EnsurePersonalTest(ctx, a, e.sid); err != nil || !pending {
		t.Fatalf("A queued: %v %v", pending, err)
	}
	if _, _, _, err := g.EnsurePersonalTest(ctx, b, e.sid); err != ErrGenQueueBusy {
		t.Fatalf("B must hit the backpressure limit, got %v", err)
	}
	// A asking again is not a new request.
	if _, pending, _, err := g.EnsurePersonalTest(ctx, a, e.sid); err != nil || !pending {
		t.Fatalf("A again: %v %v", pending, err)
	}
	_, _ = e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE subject_id = $1 AND status IN ('pending','running')`, e.sid)
}

package services

// End-to-end checks of the learning flow against a real PostgreSQL and a
// fake AI provider (skipped without TEST_DATABASE_URL):
//
//   - the first chain test of a subject is generated ONCE and shared by every
//     user (no per-user regeneration);
//   - the next chain test is generated exactly when the previous one reaches
//     the unlock bar 15🟢 + 5🟡 — and ALSO when it is better than the bar
//     (16🟢+4🟡, 20🟢) — never before;
//   - the generator sees the AVERAGE knowledge level of all students;
//   - weak topics are derived from the student's own 🔴/🟡 statuses;
//   - students with the same weak topics get the same test (a clone, zero AI
//     calls) — even after the first student finished and «deleted» it.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// flowFakeAI answers generation requests with a valid test. For weak-topics
// prompts the questions are tagged with the requested topics; for chain
// prompts the difficulty matches the requested mean. Every prompt is kept.
type flowFakeAI struct {
	mu      sync.Mutex
	prompts []string
	calls   int32
	seq     int32
}

func (f *flowFakeAI) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		user := req.Messages[len(req.Messages)-1].Content
		atomic.AddInt32(&f.calls, 1)
		f.mu.Lock()
		f.prompts = append(f.prompts, user)
		f.mu.Unlock()
		seq := atomic.AddInt32(&f.seq, 1)

		qs := validQuestions()
		if strings.Contains(user, "ТОЛЬКО по одной теме") {
			qs = qs[:repositories.TopicBatchSize] // topic_batch (B4)
		}
		var topics []string
		if strings.Contains(user, "ТОЛЬКО по этим слабым темам") {
			for _, line := range strings.Split(user, "\n") {
				var n int
				var rest string
				if _, err := fmt.Sscanf(line, "%d.", &n); err == nil && n >= 1 && n <= 5 {
					rest = strings.TrimSpace(line[strings.Index(line, ".")+1:])
					topics = append(topics, rest)
				}
			}
		}
		diff := 2
		if i := strings.Index(user, "средняя difficulty ≈ "); i >= 0 {
			var d float64
			_, _ = fmt.Sscanf(user[i+len("средняя difficulty ≈ "):], "%f", &d)
			diff = int(d + 0.5)
		}
		for i := range qs {
			qs[i].Text = fmt.Sprintf("%s #%d-%d", qs[i].Text, seq, i)
			qs[i].Difficulty = diff
			if len(topics) > 0 {
				qs[i].Topic = topics[i%len(topics)]
			} else {
				qs[i].Topic = fmt.Sprintf("Тема %d", i%5)
			}
		}
		b, _ := json.Marshal(generatedTest{Questions: qs})
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": string(b)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
		_, _ = w.Write(out)
	}))
}

type flowEnv struct {
	pool     *pgxpool.Pool
	ai       *flowFakeAI
	subjects *repositories.SubjectRepository
	gen      *repositories.GenerationRepository
	attempts *repositories.AttemptRepository
	users    *repositories.UserRepository
	genSvc   *GeneratorService
	quiz     *QuizService
	sid      int64
}

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ai := &flowFakeAI{}
	srv := ai.server(t)
	t.Cleanup(srv.Close)

	e := &flowEnv{pool: pool, ai: ai}
	e.subjects = repositories.NewSubjectRepository(pool)
	e.gen = repositories.NewGenerationRepository(pool)
	e.attempts = repositories.NewAttemptRepository(pool)
	e.users = repositories.NewUserRepository(pool)
	state := repositories.NewStateRepository(pool)
	e.genSvc = NewGeneratorService(deepseek.New("k", srv.URL), &config.Config{}, e.gen, e.subjects, state)
	e.quiz = NewQuizService(e.subjects, e.attempts, state, e.gen, e.genSvc, e.users)
	e.sid, err = testutil.CreateSubject(ctx, pool, "Биология flow "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *flowEnv) user(t *testing.T, name string) int64 {
	t.Helper()
	u, err := e.users.Upsert(context.Background(), &models.User{TelegramID: time.Now().UnixNano() % 1_000_000_000_000, FirstName: name})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// drain runs queued jobs until the queue has nothing due for this subject.
func (e *flowEnv) drain(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM generation_jobs WHERE subject_id = $1 AND status = 'pending'`, e.sid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		// Only this subject's jobs: park the other subjects' jobs meanwhile.
		job, err := e.claimOwn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			return
		}
		id, err := e.genSvc.runJob(ctx, job)
		if err != nil {
			t.Fatalf("job %d: %v", job.ID, err)
		}
		if err := e.gen.CompleteJob(ctx, job.ID, id); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *flowEnv) claimOwn(ctx context.Context) (*models.GenerationJob, error) {
	var j models.GenerationJob
	var tn, owner *int64
	err := e.pool.QueryRow(ctx, `
		UPDATE generation_jobs SET status = 'running', attempts = attempts + 1
		WHERE id = (SELECT id FROM generation_jobs WHERE subject_id = $1 AND status = 'pending' ORDER BY id LIMIT 1)
		RETURNING id, kind, subject_id, test_number, owner_user_id, attempts, COALESCE(topic_key, '')`, e.sid).
		Scan(&j.ID, &j.Kind, &j.SubjectID, &tn, &owner, &j.Attempts, &j.TopicKey)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	if tn != nil {
		j.TestNumber = int(*tn)
	}
	if owner != nil {
		j.OwnerUserID = *owner
	}
	return &j, nil
}

// setStatuses gives the user the knowledge statuses for the test questions:
// the first `green` questions 🟢, the next `yellow` 🟡, the rest 🔴.
func (e *flowEnv) setStatuses(t *testing.T, userID, testID int64, green, yellow int) {
	t.Helper()
	ctx := context.Background()
	qs, err := e.subjects.TestQuestions(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range qs {
		st := 0
		switch {
		case i < green:
			st = 2
		case i < green+yellow:
			st = 1
		}
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO user_question_progress (user_id, question_id, status, wrong_count)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, question_id) DO UPDATE SET status = EXCLUDED.status, wrong_count = EXCLUDED.wrong_count`,
			userID, q.ID, st, 2-st); err != nil {
			t.Fatal(err)
		}
	}
}

// play runs one FULL attempt of the test through the real answer path
// (StartTest + SubmitAnswer): a question is answered correctly when
// right(topic) is true. Returns the number of correct answers.
func (e *flowEnv) play(t *testing.T, userID, testID int64, right func(topic string) bool) int {
	t.Helper()
	ctx := context.Background()
	a, err := e.quiz.startTest(ctx, userID, testID, false)
	if err != nil {
		t.Fatal(err)
	}
	qs, err := e.subjects.TestQuestions(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	n := len(qs)
	correct := 0
	for pos := 1; pos <= n; pos++ {
		row, err := e.attempts.LoadQuestionView(ctx, a.ID, userID, pos, false, "")
		if err != nil || row.Question == nil {
			t.Fatal(err)
		}
		q := row.Question
		ans := q.CorrectAnswer
		if right(q.Topic) {
			correct++
		} else if ans == "A" {
			ans = "B"
		} else {
			ans = "A"
		}
		if _, err := e.quiz.SubmitAnswer(ctx, userID, a.ID, pos, ans); err != nil {
			t.Fatal(err)
		}
	}
	return correct
}

func (e *flowEnv) weak(t *testing.T, userID, subjectID int64) []string {
	t.Helper()
	w, err := e.gen.WeakTopics(context.Background(), userID, subjectID, 5)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func notIn(topics ...string) func(string) bool {
	return func(topic string) bool {
		for _, t := range topics {
			if t == topic {
				return false
			}
		}
		return true
	}
}

func (e *flowEnv) chain(t *testing.T) []models.Test {
	t.Helper()
	c, err := e.subjects.ListChainTests(context.Background(), e.sid)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFlowChainSharedAndUnlock(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	alice, bob, carl := e.user(t, "Alice"), e.user(t, "Bob"), e.user(t, "Carl")

	// Bootstrap (as main.go does): Тест 1 queued once per subject.
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if c := e.chain(t); len(c) != 1 {
		t.Fatalf("expected exactly one Тест 1, got %d", len(c))
	}
	callsAfterT1 := atomic.LoadInt32(&e.ai.calls)

	// Three users open the subject: everyone sees the SAME Тест 1, nothing
	// is generated again.
	var t1 int64
	for _, u := range []int64{alice, bob, carl} {
		scr, err := e.quiz.GetSubjectScreen(ctx, u, e.sid, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if scr.Slots[0].Test == nil {
			t.Fatal("Тест 1 must exist for every user")
		}
		if t1 == 0 {
			t1 = scr.Slots[0].Test.ID
		} else if scr.Slots[0].Test.ID != t1 {
			t.Fatalf("users got different Тест 1: %d vs %d", t1, scr.Slots[0].Test.ID)
		}
		if scr.UnlockedMax != 1 {
			t.Fatalf("fresh user must have only Тест 1 open, got %d", scr.UnlockedMax)
		}
	}
	e.drain(t)
	if got := atomic.LoadInt32(&e.ai.calls); got != callsAfterT1 {
		t.Fatalf("opening the subject must not generate anything: %d → %d calls", callsAfterT1, got)
	}
	test1, _ := e.subjects.GetTest(ctx, t1)

	// Below the bar (14🟢+6🟡): no generation, Тест 2 stays locked.
	e.setStatuses(t, alice, t1, 14, 6)
	e.quiz.OnTestCompleted(ctx, alice, test1, 14, 6)
	e.drain(t)
	if len(e.chain(t)) != 1 {
		t.Fatal("Тест 2 must not be generated below 15🟢 + 5🟡")
	}

	// Better than the bar (20🟢, 0🟡) — must unlock and generate Тест 2.
	// (Before the fix this exact student was locked out forever.)
	e.setStatuses(t, alice, t1, 20, 0)
	e.setStatuses(t, bob, t1, 0, 0) // bob: all 🔴 → averaged marks are 1.0
	e.quiz.OnTestCompleted(ctx, alice, test1, 20, 0)
	e.drain(t)
	c := e.chain(t)
	if len(c) != 2 || c[1].TestNumber != 2 {
		t.Fatalf("Тест 2 must be generated once 20🟢 is reached, chain = %d", len(c))
	}
	ok, reason, err := e.quiz.CanOpenTest(ctx, alice, &c[1])
	if err != nil || !ok {
		t.Fatalf("alice (20🟢) must be able to open Тест 2: %v %s", err, reason)
	}
	// The generator saw the AVERAGE level of all students (alice 2, bob 0 → 1).
	last := e.ai.prompts[len(e.ai.prompts)-1]
	if !strings.Contains(last, "Прошлый Тест №1") || !strings.Contains(last, "— 1\n") {
		t.Fatalf("Тест 2 prompt must carry the averaged marks of Тест 1:\n%s", last)
	}

	// Bob reaches 16🟢+4🟡: Тест 2 is shared — no second generation.
	before := atomic.LoadInt32(&e.ai.calls)
	e.setStatuses(t, bob, t1, 16, 4)
	e.quiz.OnTestCompleted(ctx, bob, test1, 16, 4)
	e.drain(t)
	if len(e.chain(t)) != 2 || atomic.LoadInt32(&e.ai.calls) != before {
		t.Fatal("Тест 2 already exists — it must be reused, not regenerated")
	}
	if ok, _, _ := e.quiz.CanOpenTest(ctx, bob, &c[1]); !ok {
		t.Fatal("bob (16🟢 + 4🟡) must be able to open Тест 2")
	}
	// Carl never trained: Тест 2 is locked for him.
	if ok, _, _ := e.quiz.CanOpenTest(ctx, carl, &c[1]); ok {
		t.Fatal("carl must not open Тест 2")
	}
	// Alice retries Тест 1 with mistakes and drops below the bar — the
	// unlock is permanent, Тест 2 must stay open.
	e.setStatuses(t, alice, t1, 10, 5)
	e.quiz.OnTestCompleted(ctx, alice, test1, 10, 5)
	if ok, reason, _ := e.quiz.CanOpenTest(ctx, alice, &c[1]); !ok {
		t.Fatalf("Тест 2 must stay unlocked after a worse retry of Тест 1: %s", reason)
	}
	// Completing below the bar must not bump carl's watermark.
	e.setStatuses(t, carl, t1, 5, 5)
	e.quiz.OnTestCompleted(ctx, carl, test1, 5, 5)
	if ok, _, _ := e.quiz.CanOpenTest(ctx, carl, &c[1]); ok {
		t.Fatal("a completion below the bar must not unlock Тест 2")
	}
}

func TestFlowConcurrentPersonalCreation(t *testing.T) {
	// Two personal tests created at the same moment for DIFFERENT users must
	// not collide on test_number and hand one user the other's test.
	e := newFlowEnv(t)
	ctx := context.Background()
	u1, u2 := e.user(t, "One"), e.user(t, "Two")
	seed := make([]models.SeedQuestion, 0, GeneratedQuestionsPerTest)
	for _, q := range validQuestions() {
		var o [4]string
		copy(o[:], q.Options)
		seed = append(seed, models.SeedQuestion{Text: q.Text, Options: o, Correct: q.Correct, Topic: q.Topic, Difficulty: 2})
	}
	var wg sync.WaitGroup
	res := make([]*models.Test, 2)
	errs := make([]error, 2)
	for i, u := range []int64{u1, u2} {
		wg.Add(1)
		go func(i int, u int64) {
			defer wg.Done()
			res[i], errs[i] = e.gen.CreateGeneratedTest(ctx, &models.Test{
				SubjectID: e.sid, Title: "🎯", Kind: models.TestKindPersonal, OwnerUserID: u, TopicsFingerprint: "fp",
			}, seed)
		}(i, u)
	}
	wg.Wait()
	for i := range res {
		if errs[i] != nil {
			t.Fatalf("create %d: %v", i, errs[i])
		}
	}
	if res[0].ID == res[1].ID {
		t.Fatal("two users received the same personal test row")
	}
	for i, u := range []int64{u1, u2} {
		got, _ := e.gen.FindPersonalTest(ctx, e.sid, u)
		if got == nil || got.ID != res[i].ID {
			t.Fatalf("user %d does not own the returned test", i)
		}
	}
}

// #37: a chain with holes in its numbering (1,2,3,7,8) must show tests 7
// and 8 — the visible range is bounded by the max TestNumber.
func TestSubjectScreenChainGaps(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Gap")
	for _, n := range []int{1, 2, 3, 7, 8} {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO tests (subject_id, test_number, title, kind)
			VALUES ($1, $2, $3, 'chain')`, e.sid, n, fmt.Sprintf("Тест %d", n)); err != nil {
			t.Fatal(err)
		}
	}
	scr, err := e.quiz.GetSubjectScreen(ctx, u, e.sid, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if scr.MaxVisible != 8 {
		t.Fatalf("MaxVisible = %d, want 8", scr.MaxVisible)
	}
	if len(scr.Slots) != 8 {
		t.Fatalf("got %d slots, want 8", len(scr.Slots))
	}
	for _, n := range []int{1, 2, 3, 7, 8} {
		if scr.Slots[n-1].Test == nil {
			t.Fatalf("Тест %d is hidden", n)
		}
	}
	for _, n := range []int{4, 5, 6} {
		if scr.Slots[n-1].Test != nil {
			t.Fatalf("slot %d must be empty (hole)", n)
		}
	}
	if scr.Slots[6].Unlocked || scr.Slots[7].Unlocked {
		t.Fatal("tests 7/8 must be visible but still locked for a new user")
	}
}

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
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
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
	e.genSvc = NewGeneratorService(nil, &config.Config{}, e.gen, e.subjects, state).WithGroq(groq.New("k", srv.URL))
	e.quiz = NewQuizService(e.subjects, e.attempts, state, e.gen, e.genSvc, e.users)
	e.sid, err = e.subjects.EnsureSubject(ctx, "Биология flow "+time.Now().Format("150405.000000"))
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
		RETURNING id, kind, subject_id, test_number, owner_user_id, attempts`, e.sid).
		Scan(&j.ID, &j.Kind, &j.SubjectID, &tn, &owner, &j.Attempts)
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
}

func TestFlowWeakTopicsSharedClone(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	alice, bob, dan := e.user(t, "Alice"), e.user(t, "Bob"), e.user(t, "Dan")

	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	t1 := e.chain(t)[0].ID

	// Alice and Bob have IDENTICAL statuses → identical weak topics.
	e.setStatuses(t, alice, t1, 10, 5)
	e.setStatuses(t, bob, t1, 10, 5)
	wa, _ := e.gen.WeakTopics(ctx, alice, e.sid, 5)
	wb, _ := e.gen.WeakTopics(ctx, bob, e.sid, 5)
	if len(wa) == 0 || strings.Join(wa, "|") != strings.Join(wb, "|") {
		t.Fatalf("weak topics must be derived from statuses: %v vs %v", wa, wb)
	}

	// Alice: first request generates (one AI call).
	_, pending, _, err := e.quiz.EnsurePersonalTest(ctx, alice, e.sid)
	if err != nil || !pending {
		t.Fatalf("alice: expected pending generation, got pending=%v err=%v", pending, err)
	}
	before := atomic.LoadInt32(&e.ai.calls)
	e.drain(t)
	if atomic.LoadInt32(&e.ai.calls) != before+1 {
		t.Fatal("alice's weak-topics test must cost exactly one AI call")
	}
	ta, _, _, err := e.quiz.EnsurePersonalTest(ctx, alice, e.sid)
	if err != nil || ta == nil {
		t.Fatalf("alice test: %v", err)
	}
	qa, _ := e.gen.PersonalTestQuestions(ctx, ta.ID)

	// Bob: same weak topics → instant clone, NO AI call, same questions.
	before = atomic.LoadInt32(&e.ai.calls)
	tb, pending, _, err := e.quiz.EnsurePersonalTest(ctx, bob, e.sid)
	if err != nil || tb == nil || pending {
		t.Fatalf("bob must get a ready clone: test=%v pending=%v err=%v", tb, pending, err)
	}
	if tb.ID == ta.ID || tb.OwnerUserID != bob {
		t.Fatal("bob must own his own copy")
	}
	qb, _ := e.gen.PersonalTestQuestions(ctx, tb.ID)
	if atomic.LoadInt32(&e.ai.calls) != before || len(qa) != len(qb) || qa[0].Text != qb[0].Text {
		t.Fatal("bob must receive the same questions without an AI call")
	}

	// Both finish their tests («🏁 Закончить тест»).
	if err := e.quiz.FinishPersonalTest(ctx, alice, ta.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.quiz.FinishPersonalTest(ctx, bob, tb.ID); err != nil {
		t.Fatal(err)
	}
	if tt, _ := e.gen.FindPersonalTest(ctx, e.sid, bob); tt != nil {
		t.Fatal("a finished personal test must be gone for its owner")
	}

	// Dan comes later with the SAME weak topics: before the fix the cache
	// was deleted together with the last copy and Dan paid for a new
	// generation. Now he gets a clone of the archived template.
	e.setStatuses(t, dan, t1, 10, 5)
	before = atomic.LoadInt32(&e.ai.calls)
	td, pending, _, err := e.quiz.EnsurePersonalTest(ctx, dan, e.sid)
	if err != nil || td == nil || pending {
		t.Fatalf("dan must get a ready clone after the others finished: test=%v pending=%v err=%v", td, pending, err)
	}
	qd, _ := e.gen.PersonalTestQuestions(ctx, td.ID)
	if atomic.LoadInt32(&e.ai.calls) != before || qd[0].Text != qa[0].Text {
		t.Fatal("dan must get the same test without an AI call")
	}
	// The template is hidden and ownerless: it never shows up as anyone's test.
	var templates int
	if err := e.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM tests
		WHERE subject_id = $1 AND kind = 'personal' AND owner_user_id IS NULL AND NOT is_active`, e.sid).Scan(&templates); err != nil {
		t.Fatal(err)
	}
	if templates != 1 {
		t.Fatalf("expected exactly one archived template, got %d", templates)
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

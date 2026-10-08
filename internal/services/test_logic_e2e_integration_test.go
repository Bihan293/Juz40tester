package services

// End-to-end walk through the test logic in the PRODUCTION generation
// configuration (batch strategy) with a fake AI that counts its calls
// (skipped without TEST_DATABASE_URL):
//
//  1. Тест 1 is generated once and shared; tests are taken through the real
//     answer path and finished; the next test is generated only at the
//     unlock bar and is shared by everybody after that;
//  2. AI calls per generated test (4 batch calls of 5 questions) and zero
//     calls for every reuse;
//  3. the next test sees the previous one, is harder and repeats none of
//     the earlier questions;
//  4. users with the same weak topics get the same personal test content
//     with zero further AI calls.

import (
	"context"
	"strings"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

func TestTestLogicEndToEnd(t *testing.T) {
	f := &batchFakeAI{}
	e, g := scaleEnv(t, f, batchCfg())
	e.genSvc, e.quiz.genSvc = g, g
	ctx := context.Background()
	alice, bob := e.user(t, "Alice"), e.user(t, "Bob")

	chainTest := func(n int) *models.Test {
		t.Helper()
		for _, c := range e.chain(t) {
			if c.TestNumber == n {
				c := c
				return &c
			}
		}
		return nil
	}
	// finish plays one full attempt with every answer right and reports it
	// like the bot's result screen does.
	finish := func(u int64, test *models.Test) CompletionOutcome {
		t.Helper()
		e.play(t, u, test.ID, func(string) bool { return true })
		p, err := e.gen.TestProgressForUser(ctx, u, []int64{test.ID})
		if err != nil {
			t.Fatal(err)
		}
		return e.quiz.OnTestCompleted(ctx, u, test, p[test.ID].Green, p[test.ID].Yellow)
	}

	// --- Тест 1: bootstrap, one generation, shared.
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	t1 := chainTest(1)
	if t1 == nil {
		t.Fatal("Тест 1 not generated")
	}
	perTest := f.calls.Load()
	if perTest != 4 {
		t.Fatalf("a 20-question test must take 4 AI calls (batches of 5), took %d", perTest)
	}

	// First pass: every answer right → 20🟡, below the bar: nothing unlocks,
	// nothing is generated.
	if out := finish(alice, t1); out.NewUnlock || out.NextGenerating {
		t.Fatalf("first pass (20🟡) must not unlock: %+v", out)
	}
	e.drain(t)
	if f.calls.Load() != perTest || chainTest(2) != nil {
		t.Fatal("no generation below the unlock bar")
	}
	// Second pass: 20🟢 → Тест 2 is unlocked and generated right away.
	if out := finish(alice, t1); !out.NewUnlock || !out.NextGenerating {
		t.Fatalf("second pass (20🟢) must unlock and generate Тест 2: %+v", out)
	}
	e.drain(t)
	t2 := chainTest(2)
	if t2 == nil || f.calls.Load() != 2*perTest {
		t.Fatalf("Тест 2 must be generated with %d calls (total %d)", perTest, f.calls.Load())
	}
	f.mu.Lock()
	prompt2 := f.prompts[len(f.prompts)-1]
	f.mu.Unlock()
	for _, want := range []string{"Прошлый Тест №1", "немного сложнее прошлого", "НЕ ПОВТОРЯТЬ"} {
		if !strings.Contains(prompt2, want) {
			t.Fatalf("Тест 2 prompt must contain %q:\n%s", want, prompt2)
		}
	}

	// Bob passes Тест 1 too: Тест 2 already exists — shared, zero calls.
	finish(bob, t1)
	finish(bob, t1)
	e.drain(t)
	if f.calls.Load() != 2*perTest || len(e.chain(t)) != 2 {
		t.Fatal("Тест 2 is shared: a second student must not trigger a generation")
	}

	// --- Тест 3: harder again, no repeats of Тест 1 or Тест 2.
	finish(alice, t2)
	finish(alice, t2)
	e.drain(t)
	t3 := chainTest(3)
	if t3 == nil || f.calls.Load() != 3*perTest {
		t.Fatalf("Тест 3 must be generated with %d calls", perTest)
	}
	var stems []string
	means := make([]float64, 0, 3)
	for _, tt := range []*models.Test{t1, t2, t3} {
		qs, err := e.subjects.TestQuestions(ctx, tt.ID)
		if err != nil || len(qs) != GeneratedQuestionsPerTest {
			t.Fatalf("test %d: %d questions, %v", tt.TestNumber, len(qs), err)
		}
		for _, q := range qs {
			if repeatsAny(q.Text, stems) >= 0 {
				t.Fatalf("Тест %d repeats an earlier question: %q", tt.TestNumber, q.Text)
			}
		}
		stems = append(stems, questionStems(qs)...)
		means = append(means, meanDifficulty(qs))
	}
	if !(means[0] < means[1] && means[1] < means[2]) {
		t.Fatalf("difficulty must grow from test to test, got %v", means)
	}

	// --- Personal weak-topics tests: same weak topics → same content, the
	// second student costs no AI call.
	setWeak(t, e, alice, "Генетика", "Клетка")
	setWeak(t, e, bob, "клетка", "ГЕНЕТИКА")
	if _, pending, _, err := e.quiz.EnsurePersonalTest(ctx, alice, e.sid); err != nil || !pending {
		t.Fatalf("Alice must wait for her first personal test: pending=%v err=%v", pending, err)
	}
	e.drain(t)
	pa, _, _, err := e.quiz.EnsurePersonalTest(ctx, alice, e.sid)
	if err != nil || pa == nil {
		t.Fatalf("Alice's personal test: %v", err)
	}
	afterAlice := f.calls.Load()
	t.Logf("AI calls: %d per chain test, %d for the first personal test, difficulty means %v", perTest, afterAlice-3*perTest, means)
	pb, pending, _, err := e.quiz.EnsurePersonalTest(ctx, bob, e.sid)
	if err != nil || pending || pb == nil {
		t.Fatalf("Bob must get the test at once: test=%v pending=%v err=%v", pb, pending, err)
	}
	if f.calls.Load() != afterAlice {
		t.Fatal("the same weak topics must not trigger a second generation")
	}
	qa, _ := e.subjects.TestQuestions(ctx, pa.ID)
	qb, _ := e.subjects.TestQuestions(ctx, pb.ID)
	texts := map[string]bool{}
	for _, q := range qa {
		texts[q.Text] = true
	}
	for _, q := range qb {
		if !texts[q.Text] {
			t.Fatalf("Bob's test differs from Alice's: %q", q.Text)
		}
		if q.Topic != "Генетика" && q.Topic != "Клетка" {
			t.Fatalf("a personal test may only train the weak topics, got %q", q.Topic)
		}
	}
	if len(qa) != GeneratedQuestionsPerTest || len(qb) != GeneratedQuestionsPerTest {
		t.Fatalf("personal tests: %d and %d questions", len(qa), len(qb))
	}
}

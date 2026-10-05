package services

// Scenarios of the weak-topics system against a real PostgreSQL (skipped
// without TEST_DATABASE_URL). Every answer goes through the real answer path
// (StartTest + SubmitAnswer), weak topics come from the accumulated per-topic
// statistics (user_topic_stats).

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

func (e *flowEnv) firstChainTest(t *testing.T, number int) int64 {
	t.Helper()
	if _, err := e.gen.EnqueueChainJobNow(context.Background(), e.sid, number); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	for _, c := range e.chain(t) {
		if c.TestNumber == number {
			return c.ID
		}
	}
	t.Fatalf("chain test %d not generated", number)
	return 0
}

func (e *flowEnv) level(t *testing.T, userID int64, topic string) int {
	t.Helper()
	stats, err := e.gen.TopicStats(context.Background(), userID, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stats {
		if s.Key == models.NormalizeTopic(topic) {
			return s.Level()
		}
	}
	return models.TopicUnknown
}

// Systematic mistakes in ONE topic → only that topic is weak (🔴).
func TestWeakTopicsOneTopicSystematic(t *testing.T) {
	e := newFlowEnv(t)
	u := e.user(t, "One")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 3"))
	if w := e.weak(t, u, e.sid); len(w) != 1 || w[0] != "Тема 3" {
		t.Fatalf("expected only «Тема 3» weak, got %v", w)
	}
	if e.level(t, u, "Тема 3") != models.TopicRed {
		t.Fatal("4/4 wrong must be 🔴")
	}
	// A clean first run is NOT weak (the old logic marked every 🟡
	// question — i.e. every first correct answer — as a weak topic).
	clean := e.user(t, "Clean")
	e.play(t, clean, t1, func(string) bool { return true })
	if w := e.weak(t, clean, e.sid); len(w) != 0 {
		t.Fatalf("20/20 must give no weak topics, got %v", w)
	}
}

// The user fixes the topic → 🔴 → 🟡 → leaves the weak list.
func TestWeakTopicsFixingErrors(t *testing.T) {
	e := newFlowEnv(t)
	u := e.user(t, "Fixer")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0"))
	if e.level(t, u, "Тема 0") != models.TopicRed {
		t.Fatal("expected 🔴 after 4 mistakes")
	}
	e.play(t, u, t1, func(string) bool { return true }) // 0000 1111 → 50%
	if e.level(t, u, "Тема 0") != models.TopicYellow {
		t.Fatal("expected 🟡 after fixing half of the window")
	}
	e.play(t, u, t1, func(string) bool { return true }) // last 10: 0011111111 → 80%
	if w := e.weak(t, u, e.sid); len(w) != 0 {
		t.Fatalf("fixed topic must leave the weak list, got %v", w)
	}
	// Retakes of already-🟢 questions are memory, not topic evidence: they
	// must not inflate the statistics.
	before, _ := e.gen.TopicStats(context.Background(), u, e.sid)
	e.play(t, u, t1, func(string) bool { return true })
	after, _ := e.gen.TopicStats(context.Background(), u, e.sid)
	for i := range before {
		if before[i].Correct != after[i].Correct {
			t.Fatalf("correct answers to 🟢 questions must not be counted: %+v → %+v", before[i], after[i])
		}
	}
}

// Mistakes in different topics → all of them weak, worst first.
func TestWeakTopicsDifferentTopicsOrdered(t *testing.T) {
	e := newFlowEnv(t)
	u := e.user(t, "Multi")
	t1 := e.firstChainTest(t, 1)
	seen := map[string]int{}
	e.play(t, u, t1, func(topic string) bool {
		seen[topic]++
		switch topic {
		case "Тема 1":
			return false // 0/4 → 🔴
		case "Тема 4":
			return seen[topic] > 2 // 2/4 → 🟡
		}
		return true
	})
	w := e.weak(t, u, e.sid)
	if strings.Join(w, "|") != "Тема 1|Тема 4" {
		t.Fatalf("expected [Тема 1 Тема 4] (worst first), got %v", w)
	}
}

// Statistics accumulate over several tests: a single slip per test is not a
// weakness, the same slip in two tests is.
func TestWeakTopicsAccumulateAcrossTests(t *testing.T) {
	e := newFlowEnv(t)
	u := e.user(t, "Many")
	t1 := e.firstChainTest(t, 1)
	t2 := e.firstChainTest(t, 2)
	oneSlip := func() func(string) bool {
		seen := map[string]int{}
		return func(topic string) bool {
			seen[topic]++
			return !(topic == "Тема 2" && seen[topic] == 1)
		}
	}
	e.play(t, u, t1, oneSlip())
	if w := e.weak(t, u, e.sid); len(w) != 0 {
		t.Fatalf("one slip must not make a topic weak, got %v", w)
	}
	e.play(t, u, t2, oneSlip())
	if w := e.weak(t, u, e.sid); len(w) != 1 || w[0] != "Тема 2" {
		t.Fatalf("repeated slips across tests must make the topic weak, got %v", w)
	}
	// Survives a "restart": a fresh repository reads the same statistics.
	e2 := *e
	if w := e2.weak(t, u, e.sid); len(w) != 1 {
		t.Fatal("statistics must be persistent")
	}
}

// Subjects never mix: the same topic name in two subjects is two topics.
func TestWeakTopicsSubjectsIsolated(t *testing.T) {
	a := newFlowEnv(t)
	b := newFlowEnv(t) // another subject, same database
	u := a.user(t, "Iso")
	ta := a.firstChainTest(t, 1)
	tb := b.firstChainTest(t, 1)
	a.play(t, u, ta, notIn("Тема 0"))
	b.play(t, u, tb, func(string) bool { return true })
	if w := a.weak(t, u, a.sid); len(w) != 1 {
		t.Fatalf("subject A: expected 1 weak topic, got %v", w)
	}
	if w := b.weak(t, u, b.sid); len(w) != 0 {
		t.Fatalf("subject B must not inherit subject A's mistakes, got %v", w)
	}
	stats, err := a.gen.AllTopicStats(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	subs := models.WeakTopicStatsBySubject(stats, 0)
	if len(subs[a.sid]) == 0 || len(subs[b.sid]) != 0 {
		t.Fatalf("weak subjects = %v, want only %d", subs, a.sid)
	}
}

// Personal practice: its answers stay in the statistics after «🏁 Закончить
// тест», and the next weak-topics test has NEW questions (fresh generation),
// not a clone of what the user already finished — other users still get the
// free clone.
func TestWeakTopicsPracticeNewQuestions(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u, other := e.user(t, "Practice"), e.user(t, "Other")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0", "Тема 1"))
	e.play(t, other, t1, notIn("Тема 0", "Тема 1"))

	if _, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	p1, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || p1 == nil {
		t.Fatalf("personal test: %v", err)
	}
	q1, _ := e.gen.PersonalTestQuestions(ctx, p1.ID)

	// Practice: still wrong everywhere → topic answers accumulate.
	e.play(t, u, p1.ID, func(string) bool { return false })
	before := e.statsTotal(t, u)
	if err := e.quiz.FinishPersonalTest(ctx, u, p1.ID); err != nil {
		t.Fatal(err)
	}
	if after := e.statsTotal(t, u); after != before {
		t.Fatalf("finishing must keep the topic statistics: %d → %d", before, after)
	}
	if w := e.weak(t, u, e.sid); len(w) != 2 {
		t.Fatalf("topics are still weak, got %v", w)
	}

	// Same weak topics again → a NEW generation, not the finished questions.
	calls := atomic.LoadInt32(&e.ai.calls)
	_, pending, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || !pending {
		t.Fatalf("expected a fresh generation, pending=%v err=%v", pending, err)
	}
	e.drain(t)
	if atomic.LoadInt32(&e.ai.calls) != calls+1 {
		t.Fatal("the next weak-topics test must be generated anew")
	}
	p2, _, _, _ := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if p2 == nil {
		t.Fatal("the new test must exist")
	}
	q2, _ := e.gen.PersonalTestQuestions(ctx, p2.ID)
	if len(q2) == 0 || q2[0].Text == q1[0].Text {
		t.Fatal("the new test must have new questions")
	}

	// Another user with the same profile still gets a free clone.
	calls = atomic.LoadInt32(&e.ai.calls)
	po, pending, _, err := e.quiz.EnsurePersonalTest(ctx, other, e.sid)
	if err != nil || po == nil || pending || atomic.LoadInt32(&e.ai.calls) != calls {
		t.Fatalf("other user must get a clone: test=%v pending=%v err=%v", po, pending, err)
	}
}

// The one-off backfill replays the stored answer history with exactly the
// same rules as live answers.
func TestWeakTopicsBackfillMatchesLive(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Backfill")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0"))
	e.play(t, u, t1, notIn("Тема 1"))
	e.play(t, u, t1, func(string) bool { return true })
	live, _ := e.gen.TopicStats(ctx, u, e.sid)

	if _, err := e.pool.Exec(ctx, `DELETE FROM user_topic_stats`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM app_backfills WHERE name = 'topic_stats_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := e.gen.BackfillTopicStats(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.gen.BackfillTopicStats(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	re, _ := e.gen.TopicStats(ctx, u, e.sid)
	if len(re) != len(live) {
		t.Fatalf("backfill: %d topics, live: %d", len(re), len(live))
	}
	for i := range live {
		if live[i].Recent != re[i].Recent || live[i].Correct != re[i].Correct || live[i].Wrong != re[i].Wrong {
			t.Fatalf("backfill mismatch: live %+v, backfill %+v", live[i], re[i])
		}
	}
}

func (e *flowEnv) statsTotal(t *testing.T, userID int64) int {
	t.Helper()
	stats, err := e.gen.TopicStats(context.Background(), userID, e.sid)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range stats {
		n += s.Correct + s.Wrong
	}
	return n
}

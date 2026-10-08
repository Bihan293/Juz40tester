package services

import (
	"context"
	"testing"
)

// Answering the personal test improves its topics; once they leave the weak
// list the (maybe half-done, maybe paid) personal test must stay reachable:
// the weak-topics menu still lists its subject and EnsurePersonalTest
// returns it instead of «no weak topics».
func TestPersonalTestReachableAfterTopicsRecovered(t *testing.T) {
	e := newFlowEnv(t)
	e.genSvc.noBank = true // personal-generation path
	ctx := context.Background()
	u := e.user(t, "Recovered")
	t1 := e.firstChainTest(t, 1)
	e.play(t, u, t1, notIn("Тема 0", "Тема 1"))

	if _, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	p1, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || p1 == nil {
		t.Fatalf("personal test: %v", err)
	}
	// The user answers the personal test correctly — the topics recover.
	e.play(t, u, p1.ID, func(string) bool { return true })
	if w := e.weak(t, u, e.sid); len(w) != 0 {
		t.Fatalf("precondition: topics must have recovered, still weak: %v", w)
	}

	subs, weak, withTest, err := e.quiz.WeakMenu(ctx, u, 5)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, s := range subs {
		listed = listed || s.ID == e.sid
	}
	if !listed || !withTest[e.sid] || len(weak[e.sid]) != 0 {
		t.Fatalf("subject with a personal test must stay in the menu: listed=%v withTest=%v weak=%v", listed, withTest, weak[e.sid])
	}
	got, pending, topics, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid)
	if err != nil || got == nil || got.ID != p1.ID || pending || len(topics) == 0 {
		t.Fatalf("existing personal test must be returned: test=%v pending=%v topics=%v err=%v", got, pending, topics, err)
	}

	// Once finished, nothing is listed any more.
	if err := e.quiz.FinishPersonalTest(ctx, u, p1.ID); err != nil {
		t.Fatal(err)
	}
	if subs, _, _, err := e.quiz.WeakMenu(ctx, u, 5); err != nil || len(subs) != 0 {
		t.Fatalf("after finishing: %v %v", subs, err)
	}
	if got, _, _, err := e.quiz.EnsurePersonalTest(ctx, u, e.sid); err != nil || got != nil {
		t.Fatalf("after finishing nothing must be returned: %v %v", got, err)
	}
}

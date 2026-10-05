package services

// #33/#36 regression checks against a real PostgreSQL (skipped without
// TEST_DATABASE_URL): the batched queries return EXACTLY what the old
// per-item queries returned, in the same order.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// AllSubjectsProgress (one batch for the statistics screen) == SubjectProgress
// called per subject (the old N+1 path), for several subjects and users.
func TestAllSubjectsProgressMatchesPerSubject(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	alice, bob := e.user(t, "Alice"), e.user(t, "Bob")

	// Subject 1: Тест 1 + Тест 2. Subject 2: Тест 1 only. Subject 3: empty.
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	t1 := e.chain(t)[0]
	e.setStatuses(t, alice, t1.ID, 15, 5)
	e.quiz.OnTestCompleted(ctx, alice, &t1, 15, 5) // generates Тест 2
	e.drain(t)
	if len(e.chain(t)) != 2 {
		t.Fatalf("want 2 chain tests, got %d", len(e.chain(t)))
	}
	e.setStatuses(t, alice, e.chain(t)[1].ID, 3, 2)
	e.setStatuses(t, bob, t1.ID, 4, 1)

	sid1 := e.sid
	sid2, err := testutil.CreateSubject(ctx, e.pool, "Химия batch "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	e.sid = sid2
	if _, err := e.gen.EnqueueChainJobNow(ctx, sid2, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	e.setStatuses(t, alice, e.chain(t)[0].ID, 10, 3)
	sid3, err := testutil.CreateSubject(ctx, e.pool, "Пусто batch "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{sid1, sid2, sid3}

	for _, u := range []int64{alice, bob} {
		batch, err := e.quiz.AllSubjectsProgress(ctx, u, ids)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != len(ids) {
			t.Fatalf("batch has %d subjects, want %d", len(batch), len(ids))
		}
		for _, sid := range ids {
			one, err := e.quiz.SubjectProgress(ctx, u, sid)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(one, batch[sid]) {
				t.Fatalf("user %d subject %d: per-subject %+v != batch %+v", u, sid, *one, *batch[sid])
			}
		}
	}
	// Sanity: Alice has both tests of subject 1 open (40 questions), Bob one.
	a, _ := e.quiz.AllSubjectsProgress(ctx, alice, ids)
	b, _ := e.quiz.AllSubjectsProgress(ctx, bob, ids)
	if a[sid1].TotalQuestions != 40 || b[sid1].TotalQuestions != 20 {
		t.Fatalf("open-test scoping broken: alice %d, bob %d", a[sid1].TotalQuestions, b[sid1].TotalQuestions)
	}
	if a[sid3].TotalQuestions != 0 || a[sid3].SubjectName == "" {
		t.Fatalf("empty subject must still be present: %+v", *a[sid3])
	}
}

// GetSubjectScreen: the batched resume lookup marks exactly the tests with an
// in-progress attempt, the cell order is unchanged, and the shown level is
// capped at MaxVisibleTests (#36).
func TestSubjectScreenBatchedResumeAndClamp(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	u := e.user(t, "Resume")
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	t1 := e.chain(t)[0]
	e.setStatuses(t, u, t1.ID, 15, 5)
	e.quiz.OnTestCompleted(ctx, u, &t1, 15, 5)
	e.drain(t)
	chain := e.chain(t)
	if len(chain) != 2 {
		t.Fatalf("want 2 tests, got %d", len(chain))
	}
	// In-progress attempt only on Тест 2.
	if _, err := e.quiz.startTest(ctx, u, chain[1].ID, false); err != nil {
		t.Fatal(err)
	}
	scr, err := e.quiz.GetSubjectScreen(ctx, u, e.sid, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range scr.Slots {
		if s.Number != i+1 {
			t.Fatalf("slot order broken: slot %d has number %d", i, s.Number)
		}
	}
	if scr.Slots[0].Resume || !scr.Slots[0].Completed || !scr.Slots[0].Unlocked {
		t.Fatalf("Тест 1: %+v", scr.Slots[0])
	}
	if !scr.Slots[1].Resume || scr.Slots[1].Completed || !scr.Slots[1].Unlocked {
		t.Fatalf("Тест 2: %+v", scr.Slots[1])
	}
	if scr.UnlockedMax != 2 || scr.UnlockedShown != 2 {
		t.Fatalf("level: max %d shown %d", scr.UnlockedMax, scr.UnlockedShown)
	}

	// #36: the whole chain passed → watermark 200 → access level 201, but
	// the UI shows 200.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO user_subject_state (user_id, subject_id, last_test_number) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, subject_id) DO UPDATE SET last_test_number = EXCLUDED.last_test_number`,
		u, e.sid, models.MaxVisibleTests); err != nil {
		t.Fatal(err)
	}
	e.genSvc = nil // no self-healing generation of 200 tests in a test
	e.quiz.genSvc = nil
	scr, err = e.quiz.GetSubjectScreen(ctx, u, e.sid, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if scr.UnlockedMax != models.MaxVisibleTests+1 {
		t.Fatalf("access level must stay raw (%d), got %d", models.MaxVisibleTests+1, scr.UnlockedMax)
	}
	if scr.UnlockedShown != models.MaxVisibleTests {
		t.Fatalf("shown level = %d, want %d", scr.UnlockedShown, models.MaxVisibleTests)
	}
	if scr.MaxVisible != models.MaxVisibleTests || scr.TotalPages*models.TestsPerPage < models.MaxVisibleTests {
		t.Fatalf("grid: maxVisible %d pages %d", scr.MaxVisible, scr.TotalPages)
	}
}

// buildView (one TestViewMeta query per question) keeps the Russian/Kazakh
// selection rules: Total is correct, a complete translation is used for kk
// users, a partial one never is, ru users always get the master.
func TestQuestionViewMetaTranslationRules(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	uid := e.user(t, "Kk")
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	t1 := e.chain(t)[0]
	a, err := e.quiz.startTest(ctx, uid, t1.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	qs, err := e.subjects.TestQuestions(ctx, t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.quiz.attempts.LoadQuestionView(ctx, a.ID, uid, 1, true, models.TestLangKK)
	if err != nil {
		t.Fatal(err)
	}
	meta := row.Meta
	if meta.Total != len(qs) || meta.Translated != 0 {
		t.Fatalf("meta %+v, want total %d translated 0", *meta, len(qs))
	}
	// A translator backed by the fake AI server — only its DB lookups are
	// used here (translations are inserted directly below).
	e.quiz.translator = NewTranslatorService(nil, repositories.NewTranslationRepository(e.pool)).WithGroq(groq.New("k", "http://127.0.0.1:1"))
	if !e.quiz.translator.Enabled() {
		t.Fatal("translator must be enabled")
	}
	kk := &models.User{ID: uid, TestLang: models.TestLangKK}
	ru := &models.User{ID: uid, TestLang: models.TestLangRU}

	insert := func(n int) {
		for _, q := range qs[:n] {
			if _, err := e.pool.Exec(ctx, `
				INSERT INTO question_translations (question_id, lang, question_text, option_a, option_b, option_c, option_d)
				VALUES ($1, 'kk', $2, 'a', 'b', 'c', 'd') ON CONFLICT DO NOTHING`, q.ID, "KK "+itoa64(q.ID)); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(len(qs) - 1) // partial
	v, err := e.quiz.QuestionAtPosition(ctx, a.ID, kk, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Total != len(qs) || v.Text != v.Question.Text {
		t.Fatalf("partial translation must show the Russian master: %q (total %d)", v.Text, v.Total)
	}
	insert(len(qs)) // complete
	v, err = e.quiz.QuestionAtPosition(ctx, a.ID, kk, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Text == v.Question.Text || v.Text[:3] != "KK " {
		t.Fatalf("complete translation must be used for kk: %q", v.Text)
	}
	v, err = e.quiz.QuestionAtPosition(ctx, a.ID, ru, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Text != v.Question.Text {
		t.Fatalf("ru user must get the master: %q", v.Text)
	}
}

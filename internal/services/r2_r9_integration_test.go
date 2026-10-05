package services

// Integration tests (PostgreSQL, skipped without TEST_DATABASE_URL) for:
//   R-2 — batched CreateAttempt, JOINed question loading, cached TestViewMeta,
//         1-query BuildSummary give the SAME results as before;
//   R-2/R-6 — parallel answers keep the atomic answer transaction intact;
//   R-9 — per-user daily personal-generation limit, DeepSeek spending cap.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// readyTest generates Тест 1 of the flow subject and returns it.
func (e *flowEnv) readyTest(t *testing.T) models.Test {
	t.Helper()
	if _, err := e.gen.EnqueueChainJobNow(context.Background(), e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	return e.chain(t)[0]
}

// TestCreateAttemptBatchInsert (R-2): one batch INSERT stores every question
// with its 1-based position and its own option order — identical to the
// old per-question INSERT loop.
func TestCreateAttemptBatchInsert(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	uid := e.user(t, "Batch")
	t1 := e.readyTest(t)
	qs, err := e.subjects.TestQuestions(ctx, t1.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, len(qs))
	orders := make([][]string, len(qs))
	perms := [][]string{{"A", "B", "C", "D"}, {"D", "C", "B", "A"}, {"B", "D", "A", "C"}}
	for i := range qs {
		ids[len(qs)-1-i] = qs[i].ID // reversed order
		orders[len(qs)-1-i] = perms[i%3]
	}
	a, err := e.attempts.CreateAttempt(ctx, uid, t1.ID, ids, orders, false)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(ctx, `SELECT question_id, position, option_order, answered FROM attempt_questions WHERE attempt_id = $1 ORDER BY position`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var qid int64
		var pos int
		var raw []byte
		var answered bool
		if err := rows.Scan(&qid, &pos, &raw, &answered); err != nil {
			t.Fatal(err)
		}
		ord, _ := repositories.DecodeOptionOrder(raw)
		if string(raw) != `"`+strings.Join(orders[n], "")+`"` {
			t.Fatalf("row %d: option_order stored as %s, want compact string", n, raw)
		}
		if pos != n+1 || qid != ids[n] || answered || len(ord) != 4 || ord[0] != orders[n][0] || ord[3] != orders[n][3] {
			t.Fatalf("row %d: qid=%d pos=%d ord=%v, want qid=%d pos=%d ord=%v", n, qid, pos, ord, ids[n], n+1, orders[n])
		}
		n++
	}
	if n != len(qs) {
		t.Fatalf("stored %d questions, want %d", n, len(qs))
	}
	// The JOINed loaders agree with the stored rows.
	cur, err := e.attempts.LoadQuestionView(ctx, a.ID, uid, 0, false, "")
	if err != nil || cur.AQ == nil || cur.AQ.Position != 1 || cur.Question.ID != ids[0] || cur.AQ.OptionOrder[0] != orders[0][0] {
		t.Fatalf("current question: %+v %v", cur, err)
	}
	at5, err := e.attempts.LoadQuestionView(ctx, a.ID, uid, 5, false, "")
	if err != nil || at5.AQ == nil || at5.AQ.Position != 5 || at5.Question.ID != ids[4] {
		t.Fatalf("question at position: %+v %v", at5, err)
	}
	if _, err := e.quiz.QuestionAtPosition(ctx, a.ID, &models.User{ID: uid}, 999); !errors.Is(err, repositories.ErrNotFound) {
		t.Fatalf("missing position: %v", err)
	}
}

// TestQuestionViewOwnershipAndMetaCache (R-2): the 1-query view keeps the
// ownership check, the meta is served from the cache after the first
// question (Total/subject unchanged), and BuildSummary (1 query) returns
// the same counts as the old 5-query computation.
func TestQuestionViewOwnershipAndMetaCache(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	uid := e.user(t, "Owner")
	other := e.user(t, "Stranger")
	t1 := e.readyTest(t)
	a, err := e.quiz.startTest(ctx, uid, t1.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	me := &models.User{ID: uid, TestLang: models.TestLangRU}
	if _, err := e.quiz.CurrentQuestion(ctx, a.ID, &models.User{ID: other}); !errors.Is(err, repositories.ErrNotFound) {
		t.Fatalf("foreign attempt must be ErrNotFound, got %v", err)
	}
	if _, err := e.quiz.QuestionAtPosition(ctx, a.ID, &models.User{ID: other}, 1); !errors.Is(err, repositories.ErrNotFound) {
		t.Fatalf("foreign attempt (position) must be ErrNotFound, got %v", err)
	}
	v, err := e.quiz.CurrentQuestion(ctx, a.ID, me)
	if err != nil || v == nil {
		t.Fatalf("current: %v", err)
	}
	total := v.Total
	if total != 20 || v.AttemptQ.Position != 1 || len(v.DisplayTexts) != 4 {
		t.Fatalf("view: total %d pos %d", total, v.AttemptQ.Position)
	}
	if _, ok := e.quiz.metas.get(a.ID); !ok {
		t.Fatal("meta must be cached after the first question")
	}
	// Answer everything; every next view uses the cached meta.
	for pos := 1; pos <= total; pos++ {
		v, err := e.quiz.QuestionAtPosition(ctx, a.ID, me, pos)
		if err != nil || v.Total != total {
			t.Fatalf("pos %d: %v (total %d)", pos, err, v.Total)
		}
		ans := v.Question.CorrectAnswer
		if pos%3 == 0 {
			ans = map[string]string{"A": "B", "B": "C", "C": "D", "D": "A"}[ans]
		}
		if _, err := e.quiz.SubmitAnswer(ctx, uid, a.ID, pos, ans); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := e.quiz.CurrentQuestion(ctx, a.ID, me); err != nil || v != nil {
		t.Fatalf("finished attempt: view %v err %v", v, err)
	}
	sum, err := e.quiz.BuildSummary(ctx, a.ID, uid)
	if err != nil {
		t.Fatal(err)
	}
	// Reference: the previous 5-query computation.
	qs, _ := e.subjects.TestQuestions(ctx, t1.ID)
	ids := make([]int64, len(qs))
	for i := range qs {
		ids[i] = qs[i].ID
	}
	st := map[int64]int{}
	rows, _ := e.pool.Query(ctx, `SELECT question_id, status FROM user_question_progress
		WHERE user_id = $1 AND question_id = ANY($2)`, uid, ids)
	for rows.Next() {
		var qid int64
		var s int
		_ = rows.Scan(&qid, &s)
		st[qid] = s
	}
	rows.Close()
	want := map[int]int{0: 0, 1: 0, 2: 0}
	for _, id := range ids {
		want[st[id]]++
	}
	if sum.Total != len(qs) || sum.Test.ID != t1.ID || sum.Subject.ID != e.sid || sum.Attempt.Status != models.AttemptCompleted ||
		sum.StatusCounts[0] != want[0] || sum.StatusCounts[1] != want[1] || sum.StatusCounts[2] != want[2] {
		t.Fatalf("summary %+v counts %v, want %v", sum, sum.StatusCounts, want)
	}
	if want[1] != 14 || want[0] != 6 {
		t.Fatalf("unexpected reference counts %v", want)
	}
	if _, ok := e.quiz.metas.get(a.ID); ok {
		t.Fatal("meta of a finished attempt must be dropped")
	}
	if _, err := e.quiz.BuildSummary(ctx, a.ID, other); !errors.Is(err, repositories.ErrNotFound) {
		t.Fatalf("foreign summary: %v", err)
	}
}

// TestParallelAnswersAtomic (R-2 must not weaken the answer transaction):
// many concurrent submissions of the same and of later positions count
// every question exactly once, never skip, and the attempt finishes once
// with consistent counters.
func TestParallelAnswersAtomic(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	uid := e.user(t, "Parallel")
	other := e.user(t, "Intruder")
	t1 := e.readyTest(t)
	a, err := e.quiz.startTest(ctx, uid, t1.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	const total = 20
	var finished, counted int32
	for pos := 1; pos <= total; pos++ {
		row, err := e.attempts.LoadQuestionView(ctx, a.ID, uid, pos, false, "")
		if err != nil || row.Question == nil {
			t.Fatal(err)
		}
		q := row.Question
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				p, uidX := pos, uid
				switch i {
				case 6:
					p = pos + 2 // out of order: never the current question
				case 7:
					uidX = other // foreign user
				}
				res, err := e.quiz.SubmitAnswer(ctx, uidX, a.ID, p, q.CorrectAnswer)
				if err != nil {
					if errors.Is(err, repositories.ErrAnswerOutOfOrder) || errors.Is(err, repositories.ErrNotFound) ||
						err.Error() == "attempt is not in progress" { // late duplicate after the last answer
						return
					}
					t.Errorf("submit: %v", err)
					return
				}
				if uidX == other {
					t.Error("foreign user answered an attempt")
				}
				if !res.AlreadyAnswered {
					atomic.AddInt32(&counted, 1)
					if res.Finished {
						atomic.AddInt32(&finished, 1)
					}
				}
			}(i)
		}
		wg.Wait()
	}
	if counted != total || finished != 1 {
		t.Fatalf("counted %d answers (want %d), finished %d times (want 1)", counted, total, finished)
	}
	var cc, wc, answered int
	var status string
	if err := e.pool.QueryRow(ctx, `
		SELECT a.correct_count, a.wrong_count, a.status,
		       (SELECT COUNT(*) FROM attempt_questions WHERE attempt_id = a.id AND answered)
		FROM test_attempts a WHERE a.id = $1`, a.ID).Scan(&cc, &wc, &status, &answered); err != nil {
		t.Fatal(err)
	}
	if cc != total || wc != 0 || status != models.AttemptCompleted || answered != total {
		t.Fatalf("attempt: correct %d wrong %d status %s answered %d", cc, wc, status, answered)
	}
	var progress int
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(correct_count), 0) FROM user_question_progress p
		JOIN attempt_questions aq ON aq.question_id = p.question_id AND aq.attempt_id = $2
		WHERE p.user_id = $1`, uid, a.ID).Scan(&progress); err != nil || progress != total {
		t.Fatalf("progress correct_count sum %d (want %d): %v", progress, total, err)
	}
}

// TestPersonalGenerationDailyLimit (R-9): new personal generations per user
// per day are capped; the cap is counted in the DB.
func TestPersonalGenerationDailyLimit(t *testing.T) {
	e := newFlowEnv(t)
	e.genSvc.noBank = true // legacy clone/limit path (removed in B6)
	ctx := context.Background()
	uid := e.user(t, "Limit")
	e.genSvc.cfg = &config.Config{PersonalGenPerUserDay: 2}
	// Two jobs already requested today.
	for i := 0; i < 2; i++ {
		if _, err := e.pool.Exec(ctx, `
			INSERT INTO generation_jobs (kind, subject_id, owner_user_id, topics_fingerprint, status, urgent)
			VALUES ('personal', $1, $2, $3, 'failed', TRUE)`, e.sid, uid, "fp-limit-"+time.Now().Format("150405.000000")+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.gen.PersonalJobsSince(ctx, uid, dayStart(time.Now()))
	if err != nil || n != 2 {
		t.Fatalf("PersonalJobsSince = %d, %v", n, err)
	}
	// The user has weak topics: play the chain test badly.
	t1 := e.readyTest(t)
	e.play(t, uid, t1.ID, func(string) bool { return false })
	if len(e.weak(t, uid, e.sid)) == 0 {
		t.Fatal("setup: no weak topics")
	}
	_, _, _, err = e.genSvc.EnsurePersonalTest(ctx, uid, e.sid)
	if !errors.Is(err, ErrPersonalGenLimit) {
		t.Fatalf("3rd generation of the day: want ErrPersonalGenLimit, got %v", err)
	}
	var queued int
	_ = e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM generation_jobs WHERE owner_user_id = $1 AND status = 'pending'`, uid).Scan(&queued)
	if queued != 0 {
		t.Fatal("no job may be queued over the limit")
	}
	// A higher limit lets it through (free scenarios unaffected).
	e.genSvc.cfg = &config.Config{PersonalGenPerUserDay: 3}
	if _, pending, _, err := e.genSvc.EnsurePersonalTest(ctx, uid, e.sid); err != nil || !pending {
		t.Fatalf("under the limit: pending=%v err=%v", pending, err)
	}
}

// TestDeepSeekSpendingCap (R-9): the DB-backed daily cap is fed by the
// DeepSeek client's own cost estimate; once reached, NO further paid call
// is sent (the HTTP server is never hit), concurrently too.
func TestDeepSeekSpendingCap(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":1000}}`))
	}))
	defer srv.Close()
	repo := repositories.NewSpendRepository(e.pool)
	// A fresh "day" per test run: shift the budget clock far away.
	day := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%100000) * 24 * time.Hour)
	b := NewDailyBudget(repo, 0.01)
	b.now = func() time.Time { return day.Add(time.Hour) }
	ds := deepseek.New("k", "deepseek-v4-pro", "deepseek-flash", srv.URL).WithBudget(b, nil)

	// worst case of one call: max_tokens 1000 out at peak flash ≈ $0.0012 +
	// input → ~8 calls fit under $0.01 before settling; after settling at
	// the real (peak) cost ≈ $0.0015 each, ~6 calls fit in total.
	var ok, capped int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ds.GenerateJSON(ctx, []deepseek.Message{{Role: "user", Content: "x"}}, 1000, deepseek.ThinkingEffortLow)
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, deepseek.ErrBudgetExceeded):
				atomic.AddInt32(&capped, 1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	cost, reserved, err := repo.Spent(ctx, SpendProviderDeepSeek, b.day())
	if err != nil {
		t.Fatal(err)
	}
	if ok == 0 || capped == 0 || ok+capped != 30 {
		t.Fatalf("ok %d capped %d", ok, capped)
	}
	if int32(hits) != ok {
		t.Fatalf("HTTP hits %d != successful calls %d — a capped call reached the API", hits, ok)
	}
	if cost > 0.01+1e-9 || reserved > 1e-9 {
		t.Fatalf("ledger cost $%.5f reserved $%.5f — cap $0.01 exceeded or reservations leaked", cost, reserved)
	}
	// Sequential follow-ups: each is either refused or keeps the ledger
	// under the cap, and the cap stops them within a few calls. (A fixed
	// "worst case ≈ $0.0013" constant was flaky: the client's real worst
	// case also counts the request body, so a remaining $0.0015 may
	// already be refused correctly.)
	stopped := false
	for i := 0; i < 10 && !stopped; i++ {
		before := atomic.LoadInt32(&hits)
		_, err := ds.GenerateJSON(ctx, []deepseek.Message{{Role: "user", Content: "x"}}, 1000, deepseek.ThinkingEffortLow)
		switch {
		case errors.Is(err, deepseek.ErrBudgetExceeded):
			if atomic.LoadInt32(&hits) != before {
				t.Fatal("a capped call reached the API")
			}
			stopped = true
		case err != nil:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if cost, _, err = repo.Spent(ctx, SpendProviderDeepSeek, b.day()); err != nil || cost > 0.01+1e-9 || !stopped {
		t.Fatalf("cap not enforced: cost $%.5f stopped %v err %v", cost, stopped, err)
	}
	// Next day: budget is fresh again.
	b.now = func() time.Time { return day.Add(25 * time.Hour) }
	if _, err := ds.GenerateJSON(ctx, []deepseek.Message{{Role: "user", Content: "x"}}, 1000, deepseek.ThinkingEffortLow); err != nil {
		t.Fatalf("next day must be allowed: %v", err)
	}
}

// TestCappedJobIsDeferredNotFailed (R-9): when only the capped DeepSeek is
// left, the generation job is parked until the budget resets — its attempt
// is not burned, and no paid call happens.
func TestCappedJobIsDeferredNotFailed(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	repo := repositories.NewSpendRepository(e.pool)
	day := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%100000) * 24 * time.Hour)
	b := NewDailyBudget(repo, 0.000001) // nothing fits
	b.now = func() time.Time { return day }
	ds := deepseek.New("k", "deepseek-v4-pro", "deepseek-flash", srv.URL).WithBudget(b, nil)
	state := repositories.NewStateRepository(e.pool)
	g := NewGeneratorService(ds, &config.Config{}, e.gen, e.subjects, state).WithBudget(b) // DeepSeek only, no Groq
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	job, err := e.claimOwn(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	g.executeJob(ctx, job)
	var status string
	var attempts int
	var notBefore time.Time
	if err := e.pool.QueryRow(ctx, `SELECT status, attempts, not_before FROM generation_jobs WHERE id = $1`, job.ID).Scan(&status, &attempts, &notBefore); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 0 || !notBefore.Equal(b.NextDay()) {
		t.Fatalf("job: status %s attempts %d not_before %v (want pending/0/%v)", status, attempts, notBefore, b.NextDay())
	}
	if hits != 0 {
		t.Fatalf("a capped generation hit the paid API %d times", hits)
	}
	_, _ = e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE id = $1`, job.ID)
}

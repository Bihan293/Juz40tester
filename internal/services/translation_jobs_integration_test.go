package services

// R-4 integration checks (real PostgreSQL via TEST_DATABASE_URL, fake AI):
//   - opening an untranslated test never waits for the model: the request
//     only queues ONE job per (test, lang), however many users open it;
//   - the background worker translates the test once and every waiting
//     user is notified through the shared waiter;
//   - a chain test is queued for translation right after its generation;
//   - a failing translation never blocks the test (outcome Err, Russian
//     master stays usable) and the answer key is never sent to the model.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// fakeKKServer answers translation requests by prefixing every text with
// "KK ". fail=true makes every call fail with HTTP 500.
type fakeKKServer struct {
	calls    int32
	fail     atomic.Bool
	delay    time.Duration
	sawKeyMu sync.Mutex
	sawKey   bool
}

func (f *fakeKKServer) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls, 1)
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if f.fail.Load() {
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		user := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(user, "correct") {
			f.sawKeyMu.Lock()
			f.sawKey = true
			f.sawKeyMu.Unlock()
		}
		var in []translationQuestion
		_ = json.Unmarshal([]byte(user[strings.LastIndex(user, "\n")+1:]), &in)
		out := translationResponse{}
		for _, q := range in {
			opts := make([]string, len(q.Options))
			for i, o := range q.Options {
				opts[i] = "KK " + o
			}
			out.Translations = append(out.Translations, translatedQuestion{ID: q.ID, Question: "KK " + q.Question, Options: opts, Topic: "KK " + q.Topic})
		}
		b, _ := json.Marshal(out)
		resp, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": string(b)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// kkEnv is a flow env with one generated chain test and a translator wired
// to a fake Kazakh server.
func kkEnv(t *testing.T) (*flowEnv, *fakeKKServer, *TranslatorService, *models.Test) {
	t.Helper()
	e := newFlowEnv(t)
	ctx := context.Background()
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 1); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	test := e.chain(t)[0]
	fk := &fakeKKServer{}
	srv := fk.server(t)
	tr := NewTranslatorService(nil, repositories.NewTranslationRepository(e.pool)).WithGroq(groq.New("k", srv.URL))
	tr.hub.every = 50 * time.Millisecond
	e.quiz.WithTranslator(tr)
	return e, fk, tr, &test
}

func trJobRows(t *testing.T, e *flowEnv, testID int64) (n int, status string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `
		SELECT COUNT(*), COALESCE(MAX(status), '') FROM translation_jobs WHERE test_id = $1 AND lang = 'kk'`, testID).
		Scan(&n, &status); err != nil {
		t.Fatal(err)
	}
	return n, status
}

func TestTranslationQueuedNotSynchronous(t *testing.T) {
	e, fk, tr, test := kkEnv(t)
	ctx := context.Background()
	uid := e.user(t, "Kk")
	kk := &models.User{ID: uid, TestLang: models.TestLangKK}

	// 32 Kazakh students open the same untranslated test at once: no
	// handler waits for the model, and there is exactly ONE job row.
	const users = 32
	waits := make([]<-chan TranslationOutcome, users)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready, wait, err := e.quiz.PrepareTranslation(ctx, kk, test)
			if err != nil || ready || wait == nil {
				t.Errorf("prepare: ready=%v wait=%v err=%v", ready, wait != nil, err)
				return
			}
			waits[i] = wait
		}(i)
	}
	wg.Wait()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("PrepareTranslation took %v — the handler must not wait for the translation", d)
	}
	if c := atomic.LoadInt32(&fk.calls); c != 0 {
		t.Fatalf("model called %d times from the handler path", c)
	}
	if n, st := trJobRows(t, e, test.ID); n != 1 || st != "pending" {
		t.Fatalf("translation jobs = %d (%s), want exactly 1 pending", n, st)
	}

	// Isolation: the shared test DB may hold pending translation jobs left
	// by other tests — the worker would translate them too and inflate the
	// model-call count (flaky "model calls = 7").
	if _, err := e.pool.Exec(ctx, `
		DELETE FROM translation_jobs WHERE test_id <> $1 AND status IN ('pending','running')`, test.ID); err != nil {
		t.Fatal(err)
	}
	// The background worker translates once; every waiter is notified.
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { tr.RunWorker(wctx); close(done) }()
	for i, w := range waits {
		select {
		case out := <-w:
			if !out.Ready {
				t.Fatalf("waiter %d: %+v", i, out)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("waiter %d never notified", i)
		}
	}
	calls := atomic.LoadInt32(&fk.calls)
	if calls == 0 || calls > 3 {
		t.Fatalf("model calls = %d, want one per chunk (1..3) for the whole test", calls)
	}
	if n, st := trJobRows(t, e, test.ID); n != 1 || st != "done" {
		t.Fatalf("job after run: %d rows, status %s", n, st)
	}
	fk.sawKeyMu.Lock()
	if fk.sawKey {
		t.Error("answer key leaked to the translation model")
	}
	fk.sawKeyMu.Unlock()

	// Now the cached translation is complete: the next open is instant and
	// queues nothing; the question is rendered in Kazakh with the master key.
	ready, wait, err := e.quiz.PrepareTranslation(ctx, kk, test)
	if err != nil || !ready || wait != nil {
		t.Fatalf("after translation: ready=%v wait=%v err=%v", ready, wait != nil, err)
	}
	a, err := e.quiz.startTest(ctx, uid, test.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.quiz.QuestionAtPosition(ctx, a.ID, kk, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.Text, "KK ") {
		t.Fatalf("question not shown in Kazakh: %q", v.Text)
	}
	var key string
	if err := e.pool.QueryRow(ctx, `SELECT correct_answer FROM questions WHERE id = $1`, v.Question.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != v.Question.CorrectAnswer {
		t.Fatalf("answer key changed: %q vs %q", key, v.Question.CorrectAnswer)
	}
	if atomic.LoadInt32(&fk.calls) != calls {
		t.Fatal("cached test must not call the model again")
	}
	stop()
	<-done
}

func TestTranslationFailureFallsBack(t *testing.T) {
	e, fk, tr, test := kkEnv(t)
	ctx := context.Background()
	fk.fail.Store(true)
	kk := &models.User{ID: e.user(t, "KkFail"), TestLang: models.TestLangKK}
	_, wait, err := e.quiz.PrepareTranslation(ctx, kk, test)
	if err != nil || wait == nil {
		t.Fatalf("prepare: %v", err)
	}
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { tr.RunWorker(wctx); close(done) }()
	select {
	case out := <-wait:
		if out.Ready || out.Err == "" {
			t.Fatalf("failed translation reported as %+v", out)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("failure never reported")
	}
	stop()
	<-done
	// The test still opens — in the Russian master.
	a, err := e.quiz.startTest(ctx, kk.ID, test.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.quiz.QuestionAtPosition(ctx, a.ID, kk, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Text != v.Question.Text {
		t.Fatalf("untranslated test must show the master: %q", v.Text)
	}
	// The job is retried later (pending with backoff), never duplicated.
	if n, st := trJobRows(t, e, test.ID); n != 1 || st != "pending" {
		t.Fatalf("job after failure: %d rows, status %s", n, st)
	}
}

func TestChainTestTranslationQueuedAfterGeneration(t *testing.T) {
	e, _, tr, _ := kkEnv(t)
	ctx := context.Background()
	e.genSvc.WithTranslator(tr)
	if _, err := e.gen.EnqueueChainJobNow(ctx, e.sid, 2); err != nil {
		t.Fatal(err)
	}
	job, err := e.claimOwn(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: %v %v", job, err)
	}
	e.genSvc.executeJob(ctx, job)
	var testID int64
	if err := e.pool.QueryRow(ctx, `SELECT id FROM tests WHERE subject_id = $1 AND test_number = 2`, e.sid).Scan(&testID); err != nil {
		t.Fatal(err)
	}
	if n, st := trJobRows(t, e, testID); n != 1 || st != "pending" {
		t.Fatalf("chain test %d: translation jobs %d (%s), want 1 pending right after generation", testID, n, st)
	}
}

// The stuck-job reaper returns a job whose worker died to the queue.
func TestTranslationJobReaper(t *testing.T) {
	e, _, _, test := kkEnv(t)
	ctx := context.Background()
	repo := repositories.NewTranslationRepository(e.pool)
	if _, err := repo.EnqueueTranslationJob(ctx, test.ID, models.TestLangKK); err != nil {
		t.Fatal(err)
	}
	// Claim jobs until ours is running (other tests may have left jobs).
	var mine *repositories.TranslationJob
	for i := 0; i < 50 && mine == nil; i++ {
		j, err := repo.ClaimNextTranslationJob(ctx)
		if err != nil || j == nil {
			t.Fatalf("claim: %v %v", j, err)
		}
		if j.TestID == test.ID {
			mine = j
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE translation_jobs SET updated_at = now() - interval '1 hour' WHERE id = $1`, mine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ResetStuckTranslationJobs(ctx, translationStuckTimeout, translationMaxAttempts); err != nil {
		t.Fatal(err)
	}
	if _, st := trJobRows(t, e, test.ID); st != "pending" {
		t.Fatalf("stuck job status %s, want pending", st)
	}
	// Enqueue while pending/running is a no-op (never translated twice).
	if q, err := repo.EnqueueTranslationJob(ctx, test.ID, models.TestLangKK); err != nil || q {
		t.Fatalf("re-enqueue of a pending job: queued=%v err=%v", q, err)
	}
}

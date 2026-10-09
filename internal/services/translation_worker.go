package services

// R-4: background Kazakh translation.
//
// The Telegram update handler never translates anything itself anymore: it
// enqueues a translation job (RequestTranslation — one row per test and
// language, UNIQUE in the DB, so it works across instances), tells the
// user «⏳ Перевод готовится…» and frees its webhook slot within
// milliseconds. The worker below claims the job (FOR UPDATE SKIP LOCKED),
// translates it with a heartbeat, and publishes the outcome to the shared
// per-test waiter (translationHub): every user waiting for that test is
// notified at once, with no per-user DB polling.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

const (
	// translationWorkers is the size of the translation worker pool. A
	// translation is short (2–3 calls) — two workers drain a burst of
	// freshly opened tests without competing with generation for quota.
	translationWorkers = 2
	// translationJobTimeout bounds one translation run (all chunks, every
	// provider of the route).
	translationJobTimeout = 4 * time.Minute
	// translationStuckTimeout: a running job whose heartbeat stopped for
	// this long is returned to the queue (the worker died).
	translationStuckTimeout = 3 * time.Minute
	// translationHeartbeat is how often a running job refreshes updated_at.
	translationHeartbeat = 30 * time.Second
	// translationMaxAttempts caps retries of a failing translation job.
	translationMaxAttempts = 3
	// translationRetryDelay is the backoff between job retries.
	translationRetryDelay = 2 * time.Minute
	// translationPollEvery is the FALLBACK poll of an idle worker: jobs
	// enqueued in this instance wake a worker immediately.
	translationPollEvery = 3 * time.Minute
	// translationReapEvery: how often the stuck-job reaper runs.
	translationReapEvery = 5 * time.Minute

	// translationWaitPoll is the interval of the ONE shared fallback check
	// per awaited test (jobs finished by another instance). Jobs finished
	// in this instance are published instantly.
	translationWaitPoll = 10 * time.Second
	// TranslationWaitTimeout caps how long users wait for a translation;
	// after it they get the test in the Russian master version.
	TranslationWaitTimeout = 4 * time.Minute
)

// TranslationOutcome is the result a waiting user gets.
type TranslationOutcome struct {
	Ready    bool   // the Kazakh version is complete
	TimedOut bool   // no result within TranslationWaitTimeout
	Err      string // why the translation failed (Ready=false, TimedOut=false)
}

// ---------------------------------------------------------------------------
// Shared per-test waiter
// ---------------------------------------------------------------------------

// translationHub keeps ONE waiter per awaited test, however many users wait
// for it: subscribers get a buffered channel that receives exactly one
// outcome. The outcome comes either from this instance's worker (publish —
// instant) or from the single fallback poll of the job row (a job finished
// by another instance), which runs at most every `every` per test and
// stops as soon as the test is resolved or the wait times out.
type translationHub struct {
	mu      sync.Mutex
	waits   map[int64]*hubWait
	poll    func(ctx context.Context, testID int64) (TranslationOutcome, bool, error)
	every   time.Duration
	timeout time.Duration
}

// hubWait is one wait for one test: its subscribers and its own poller. A
// fresh wait (after the previous one was resolved) gets a fresh entry, so
// an old poller can never resolve or time out a newer wait.
type hubWait struct {
	subs []chan TranslationOutcome
}

func newTranslationHub(poll func(context.Context, int64) (TranslationOutcome, bool, error), every time.Duration) *translationHub {
	return &translationHub{
		waits: map[int64]*hubWait{}, poll: poll,
		every: every, timeout: TranslationWaitTimeout,
	}
}

// subscribe registers a waiter for the test. The first subscriber of a wait
// starts its (only) fallback poller.
func (h *translationHub) subscribe(testID int64) <-chan TranslationOutcome {
	ch := make(chan TranslationOutcome, 1)
	h.mu.Lock()
	w := h.waits[testID]
	fresh := w == nil
	if fresh {
		w = &hubWait{}
		h.waits[testID] = w
	}
	w.subs = append(w.subs, ch)
	h.mu.Unlock()
	if fresh {
		go h.watch(testID, w)
	}
	return ch
}

// publish delivers the outcome to every current subscriber of the test and
// forgets them (a later subscriber starts a fresh wait).
func (h *translationHub) publish(testID int64, out TranslationOutcome) {
	h.mu.Lock()
	w := h.waits[testID]
	delete(h.waits, testID)
	h.mu.Unlock()
	h.deliver(w, out)
}

// resolve is publish restricted to the given wait (used by its poller).
func (h *translationHub) resolve(testID int64, w *hubWait, out TranslationOutcome) {
	h.mu.Lock()
	if h.waits[testID] != w {
		h.mu.Unlock()
		return
	}
	delete(h.waits, testID)
	h.mu.Unlock()
	h.deliver(w, out)
}

func (h *translationHub) deliver(w *hubWait, out TranslationOutcome) {
	if w == nil {
		return
	}
	for _, ch := range w.subs {
		ch <- out // buffered (1) and sent exactly once — never blocks
	}
}

// current reports whether w is still the active wait of the test.
func (h *translationHub) current(testID int64, w *hubWait) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waits[testID] == w
}

// unsubscribe drops one subscriber (the caller will not read its channel).
func (h *translationHub) unsubscribe(testID int64, ch <-chan TranslationOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.waits[testID]
	if w == nil {
		return
	}
	for i, c := range w.subs {
		if c == ch {
			w.subs = append(w.subs[:i], w.subs[i+1:]...)
			break
		}
	}
	if len(w.subs) == 0 {
		delete(h.waits, testID) // its poller notices and stops
	}
}

// waiting reports the number of subscribers of a test (for tests).
func (h *translationHub) waiting(testID int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if w := h.waits[testID]; w != nil {
		return len(w.subs)
	}
	return 0
}

// watch is the single fallback poller of one wait.
func (h *translationHub) watch(testID int64, w *hubWait) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("translation waiter %d panicked: %v", testID, r)
			h.resolve(testID, w, TranslationOutcome{Err: "internal error"})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()
	ticker := time.NewTicker(h.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.resolve(testID, w, TranslationOutcome{TimedOut: true})
			return
		case <-ticker.C:
		}
		if !h.current(testID, w) {
			return // already published by the in-process worker
		}
		if h.poll == nil {
			continue
		}
		out, final, err := h.poll(ctx, testID)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("translation waiter %d: %v", testID, err)
			}
			continue
		}
		if final {
			h.resolve(testID, w, out)
			return
		}
	}
}

// pollJobOutcome is the cheap fallback check of the hub: ONE indexed lookup
// of the job row. final = false while the job is still pending/running.
func (t *TranslatorService) pollJobOutcome(ctx context.Context, testID int64) (TranslationOutcome, bool, error) {
	status, lastErr, err := t.repo.TranslationJobState(ctx, testID, models.TestLangKK)
	if err != nil {
		return TranslationOutcome{}, false, err
	}
	switch {
	case status == "done":
		return TranslationOutcome{Ready: true}, true, nil
	case status == "failed":
		return TranslationOutcome{Err: lastErr}, true, nil
	case status == "pending" && lastErr != "":
		// The last run failed and the job waits for its retry backoff — the
		// users waiting NOW get the Russian master instead of a long wait.
		return TranslationOutcome{Err: lastErr}, true, nil
	case status == "":
		return TranslationOutcome{Err: "translation job disappeared"}, true, nil
	}
	return TranslationOutcome{}, false, nil
}

// ---------------------------------------------------------------------------
// Handler-facing API (fast: never calls a model)
// ---------------------------------------------------------------------------

// ErrTranslationUnavailable: the translation of the test failed recently
// (all retries spent) — the test is served in the Russian master version
// right away instead of making the user wait for nothing.
var ErrTranslationUnavailable = errors.New("translation failed recently")

// RequestTranslation enqueues the Kazakh translation of a test (idempotent:
// UNIQUE (test_id, lang) — a test is never translated twice, on any
// instance) and wakes a local worker. Returns ErrTranslationUnavailable
// when the job failed recently and is not re-armed yet. Never calls a
// model: one or two short DB round trips.
func (t *TranslatorService) RequestTranslation(ctx context.Context, testID int64) error {
	if !t.Enabled() {
		return nil
	}
	queued, err := t.repo.EnqueueTranslationJob(ctx, testID, models.TestLangKK)
	if err != nil {
		return err
	}
	if !queued {
		// Already pending/running (fine — that job does the work) or failed
		// within the re-arm pause.
		status, _, err := t.repo.TranslationJobState(ctx, testID, models.TestLangKK)
		if err != nil {
			return err
		}
		if status == "failed" {
			return ErrTranslationUnavailable
		}
	}
	t.notifyWorkers()
	return nil
}

// AwaitTranslation subscribes to the outcome of the test's translation. The
// channel receives exactly one TranslationOutcome (at the latest after
// TranslationWaitTimeout). Many users waiting for the same test share ONE
// waiter — no per-user DB polling.
func (t *TranslatorService) AwaitTranslation(testID int64) <-chan TranslationOutcome {
	return t.hub.subscribe(testID)
}

// CancelAwait drops a subscription made by AwaitTranslation that will not
// be read (the translation request failed).
func (t *TranslatorService) CancelAwait(testID int64, ch <-chan TranslationOutcome) {
	t.hub.unsubscribe(testID, ch)
}

// Wake wakes an idle worker; called by the LISTEN tr_jobs listener (R-5b).
func (t *TranslatorService) Wake() { t.notifyWorkers() }

func (t *TranslatorService) notifyWorkers() {
	if t == nil || t.wake == nil {
		return
	}
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

// RunWorker processes the translation queue until ctx is cancelled. No-op
// when translation is not configured.
func (t *TranslatorService) RunWorker(ctx context.Context) {
	if !t.Enabled() {
		return
	}
	log.Printf("translator: starting %d background translation worker(s)", translationWorkers)
	var wg sync.WaitGroup
	for i := 1; i <= translationWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				panicked := t.runWorkerLoop(ctx, id)
				if ctx.Err() != nil {
					return
				}
				if panicked {
					select {
					case <-ctx.Done():
						return
					case <-time.After(5 * time.Second):
					}
				}
			}
		}(i)
	}
	wg.Wait()
	log.Println("translator: workers stopped")
}

func (t *TranslatorService) runWorkerLoop(ctx context.Context, id int) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("translation worker %d: PANIC recovered: %v", id, r)
			panicked = true
		}
	}()
	// Only worker 1 runs the fallback timer (one idle poll per
	// translationPollEvery for the whole pool).
	var tick <-chan time.Time
	if id == 1 {
		poll := time.NewTicker(translationPollEvery)
		defer poll.Stop()
		tick = poll.C
	}
	var lastReap time.Time
	for {
		if id == 1 && time.Since(lastReap) >= translationReapEvery {
			lastReap = time.Now()
			if n, err := t.repo.ResetStuckTranslationJobs(ctx, translationStuckTimeout, translationMaxAttempts); err != nil {
				if ctx.Err() == nil {
					log.Printf("translator: reap stuck jobs: %v", err)
				}
			} else if n > 0 {
				log.Printf("translator: re-queued %d stuck translation job(s)", n)
			}
		}
		t.drainQueue(ctx)
		select {
		case <-ctx.Done():
			return false
		case <-t.wake:
		case <-tick:
		}
	}
}

// drainQueue claims and runs due jobs back to back until none is due.
func (t *TranslatorService) drainQueue(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := t.repo.ClaimNextTranslationJob(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("translator: claim job: %v", err)
			}
			return
		}
		if job == nil {
			return
		}
		t.notifyWorkers() // let another idle worker look at the queue too
		t.executeJob(ctx, job)
	}
}

// executeJob runs one claimed translation job and records/publishes the
// outcome. A shutdown hands the job back to the queue (attempt not counted).
func (t *TranslatorService) executeJob(ctx context.Context, job *repositories.TranslationJob) {
	hbDone := make(chan struct{})
	go func() {
		tk := time.NewTicker(translationHeartbeat)
		defer tk.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-ctx.Done():
				return
			case <-tk.C:
				if err := t.repo.TouchTranslationJob(ctx, job.ID); err != nil && ctx.Err() == nil {
					log.Printf("translator: heartbeat job %d: %v", job.ID, err)
				}
			}
		}
	}()
	runErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("translation job %d panicked: %v", job.ID, r)
			}
		}()
		jctx, cancel := context.WithTimeout(ctx, translationJobTimeout)
		defer cancel()
		return t.runJob(jctx, job)
	}()
	close(hbDone)

	if runErr != nil && ctx.Err() != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := t.repo.ReleaseTranslationJob(rctx, job.ID); err != nil {
			log.Printf("translator: release job %d on shutdown: %v", job.ID, err)
		}
		return
	}
	// Detached from the worker context: a shutdown right after a finished
	// run must still record it (otherwise the job stays 'running' until the
	// reaper and is translated — and paid for — again).
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bcancel()
	if errors.Is(runErr, errNotTranslatable) {
		// Never retried: park the job as failed right away (maxAttempts 0).
		if err := t.repo.FailTranslationJob(bctx, job.ID, runErr, 0, 0); err != nil {
			log.Printf("translator: fail job %d: %v", job.ID, err)
		}
		t.publishOutcome(job.TestID, TranslationOutcome{Err: runErr.Error()})
		return
	}
	if runErr != nil {
		// The test still opens — in the Russian master version; the error is
		// logged and the job is retried with backoff.
		log.Printf("translator: job %d (test %d) failed: %v", job.ID, job.TestID, runErr)
		if err := t.repo.FailTranslationJob(bctx, job.ID, runErr, translationRetryDelay, translationMaxAttempts); err != nil {
			log.Printf("translator: fail job %d: %v", job.ID, err)
		}
		t.publishOutcome(job.TestID, TranslationOutcome{Err: runErr.Error()})
		return
	}
	if err := t.repo.CompleteTranslationJob(bctx, job.ID); err != nil {
		log.Printf("translator: complete job %d: %v", job.ID, err)
	}
	t.publishOutcome(job.TestID, TranslationOutcome{Ready: true})
}

// errNotTranslatable marks a job for a test that must never be translated.
var errNotTranslatable = errors.New("test belongs to a language subject — never translated")

// runJob translates the whole test. The answer key never leaves the server
// (TranslateTest sends only the texts; the key is read from the master row).
func (t *TranslatorService) runJob(ctx context.Context, job *repositories.TranslationJob) error {
	if job.Lang != models.TestLangKK {
		return fmt.Errorf("unsupported translation language %q", job.Lang)
	}
	name, err := t.repo.SubjectNameForTest(ctx, job.TestID)
	if err != nil {
		return err
	}
	if models.IsLanguageSubject(name) {
		return errNotTranslatable
	}
	questions, err := t.repo.MasterQuestionsForTest(ctx, job.TestID)
	if err != nil {
		return err
	}
	if len(questions) == 0 {
		return fmt.Errorf("test %d has no questions", job.TestID)
	}
	tr, err := t.TranslateTest(ctx, job.TestID, questions)
	if err != nil {
		return err
	}
	for _, q := range questions {
		if tr[q.ID] == nil {
			return fmt.Errorf("question %d still untranslated", q.ID)
		}
	}
	return nil
}

// publishOutcome delivers a finished translation to the users waiting in
// THIS process and, in cluster mode, to the other instances (hook).
func (t *TranslatorService) publishOutcome(testID int64, out TranslationOutcome) {
	t.hub.publish(testID, out)
	if t.onOutcome != nil {
		t.onOutcome(testID, out)
	}
}

// WithOutcomeHook installs a callback for every finished translation job
// (cluster mode: the outcome is broadcast to the other instances).
func (t *TranslatorService) WithOutcomeHook(f func(testID int64, out TranslationOutcome)) *TranslatorService {
	t.onOutcome = f
	return t
}

// DeliverRemote hands an outcome broadcast by another instance to the
// users waiting for that test in this process (no-op when nobody waits).
func (t *TranslatorService) DeliverRemote(testID int64, out TranslationOutcome) {
	t.hub.publish(testID, out)
}

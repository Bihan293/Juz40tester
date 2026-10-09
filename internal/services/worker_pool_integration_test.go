package services

// P0-2: the generation worker pool. Several workers drain the queue
// concurrently, each job is executed exactly once (FOR UPDATE SKIP LOCKED),
// and a burst is processed without waiting for the 20s fallback ticker.
// Skipped without TEST_DATABASE_URL.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// liftGroqLimits raises the local Groq quota for the fake provider: on the
// free tier (TPM 8000) a second concurrent generation correctly waits for
// the per-minute window, which is not what these tests measure.
func liftGroqLimits(t *testing.T) {
	t.Helper()
	t.Setenv("GROQ_TPM", "10000000")
	t.Setenv("GROQ_RPM", "100000")
}

func TestWorkerPoolDrainsQueueWithoutDuplicates(t *testing.T) {
	liftGroqLimits(t)
	e := newFlowEnv(t)
	ctx := context.Background()
	// Isolate from jobs of other tests/subjects in the shared database.
	if _, err := e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE status IN ('pending','running')`); err != nil {
		t.Fatal(err)
	}
	const jobs = 6
	for n := 1; n <= jobs; n++ {
		e.insertJob(t, n, "pending", 0, true, "0 seconds", "0 seconds")
	}

	srv := e.ai.server(t)
	defer srv.Close()
	state := repositories.NewStateRepository(e.pool)
	svc := NewGeneratorService(nil, &config.Config{GenWorkers: 4}, e.gen, e.subjects, state).WithGroq(groq.New("k", srv.URL))

	// Count how often every job id is claimed — must be exactly once.
	var mu sync.Mutex
	claims := map[int64]int{}
	var maxParallel, inFlight int32

	wctx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wctx.Err() == nil {
				job, err := svc.claimDue(wctx)
				if err != nil || job == nil {
					return
				}
				mu.Lock()
				claims[job.ID]++
				mu.Unlock()
				cur := atomic.AddInt32(&inFlight, 1)
				for {
					m := atomic.LoadInt32(&maxParallel)
					if cur <= m || atomic.CompareAndSwapInt32(&maxParallel, m, cur) {
						break
					}
				}
				time.Sleep(50 * time.Millisecond) // overlap the workers
				svc.executeJob(wctx, job)
				atomic.AddInt32(&inFlight, -1)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("pool did not drain the queue")
	}
	if len(claims) != jobs {
		t.Fatalf("claimed %d distinct jobs, want %d (%v)", len(claims), jobs, claims)
	}
	for id, c := range claims {
		if c != 1 {
			t.Fatalf("job %d claimed %d times", id, c)
		}
	}
	if maxParallel < 2 {
		t.Fatalf("jobs never ran in parallel (max %d)", maxParallel)
	}
	var left int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM generation_jobs WHERE subject_id = $1 AND status <> 'done'`, e.sid).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d job(s) not done", left)
	}
}

// RunWorker drains a burst immediately (well under the 20s fallback poll)
// and stops cleanly on cancellation.
func TestRunWorkerProcessesBurstImmediately(t *testing.T) {
	liftGroqLimits(t)
	e := newFlowEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE status IN ('pending','running')`); err != nil {
		t.Fatal(err)
	}
	srv := e.ai.server(t)
	defer srv.Close()
	state := repositories.NewStateRepository(e.pool)
	svc := NewGeneratorService(nil, &config.Config{GenWorkers: 3}, e.gen, e.subjects, state).WithGroq(groq.New("k", srv.URL))

	wctx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() { svc.RunWorker(wctx); close(stopped) }()

	time.Sleep(300 * time.Millisecond) // workers are idle, waiting
	for n := 1; n <= 4; n++ {
		if _, err := svc.EnsureChainTest(ctx, e.sid, n, true, 0); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var left int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM generation_jobs WHERE subject_id = $1 AND status <> 'done'`, e.sid).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d job(s) still not done — workers waited for the ticker", left)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("RunWorker did not stop on cancellation")
	}
}

func TestHasUrgentWork(t *testing.T) {
	e := newFlowEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE generation_jobs SET status = 'failed' WHERE status IN ('pending','running')`); err != nil {
		t.Fatal(err)
	}
	if busy, err := e.gen.HasUrgentWork(ctx); err != nil || busy {
		t.Fatalf("empty queue: busy=%v err=%v", busy, err)
	}
	e.insertJob(t, 1, "pending", 0, false, "0 seconds", "0 seconds") // deferred, not urgent
	e.insertJob(t, 2, "pending", 0, true, "1 hour", "0 seconds")     // urgent but backing off
	if busy, err := e.gen.HasUrgentWork(ctx); err != nil || busy {
		t.Fatalf("no due urgent job: busy=%v err=%v", busy, err)
	}
	e.insertJob(t, 3, "running", 1, true, "0 seconds", "0 seconds")
	if busy, err := e.gen.HasUrgentWork(ctx); err != nil || !busy {
		t.Fatalf("running urgent job: busy=%v err=%v", busy, err)
	}
}

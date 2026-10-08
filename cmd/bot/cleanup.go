package main

import (
	"context"
	"log"
	"time"

	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// emptyAbandonedAge: abandoned attempts without a single answer are deleted
// after this long.
const emptyAbandonedAge = 3 * 24 * time.Hour

// testTemplateTTL: a fingerprint template neither created nor used for this
// long is deleted (bounded growth of test_templates).
const testTemplateTTL = 90 * 24 * time.Hour

// cleanupStore is the subset of CleanupRepository used by runCleanup.
type cleanupStore interface {
	DeleteOldAttemptQuestions(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	DeleteOldFinishedJobs(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	DeleteEmptyAbandonedAttempts(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	DeleteOldTranslationJobs(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	DeleteStaleTemplates(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	DeleteOldFinishedAttempts(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

type cleanupSettings struct {
	attemptAge, jobAge, emptyAge time.Duration
	templateAge, trJobAge        time.Duration
	finishedAttemptAge           time.Duration
	firstDelay, interval, pause  time.Duration
}

// runCleanup runs cleanupOnce after firstDelay and then every interval
// until ctx is cancelled (R-8a).
func runCleanup(ctx context.Context, store cleanupStore, s cleanupSettings) {
	timer := time.NewTimer(s.firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			cleanupOnce(ctx, store, s)
			timer.Reset(s.interval)
		}
	}
}

// cleanupOnce deletes every kind of stale row in batches of
// CleanupBatchSize with a pause between batches, and logs the totals.
func cleanupOnce(ctx context.Context, store cleanupStore, s cleanupSettings) {
	steps := []struct {
		name string
		age  time.Duration
		del  func(context.Context, time.Duration, int) (int64, error)
	}{
		{"attempt_questions", s.attemptAge, store.DeleteOldAttemptQuestions},
		{"generation_jobs", s.jobAge, store.DeleteOldFinishedJobs},
		{"empty abandoned attempts", s.emptyAge, store.DeleteEmptyAbandonedAttempts},
		{"done translation_jobs", s.trJobAge, store.DeleteOldTranslationJobs},
		{"stale personal-test templates", s.templateAge, store.DeleteStaleTemplates},
		{"old finished attempts", s.finishedAttemptAge, store.DeleteOldFinishedAttempts},
	}
	// Fingerprint test templates unused for testTemplateTTL (optional
	// method: test fakes of cleanupStore need not implement it).
	if ts, ok := store.(interface {
		DeleteUnusedTestTemplates(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
	}); ok {
		steps = append(steps, struct {
			name string
			age  time.Duration
			del  func(context.Context, time.Duration, int) (int64, error)
		}{"unused test templates", testTemplateTTL, ts.DeleteUnusedTestTemplates})
	}
	for _, st := range steps {
		total := deleteInBatches(ctx, st.age, s.pause, st.del)
		if total > 0 {
			log.Printf("cleanup: deleted %d %s", total, st.name)
		}
	}
}

// deleteInBatches calls del until a batch removes fewer than
// CleanupBatchSize rows, an error occurs, or ctx is cancelled.
func deleteInBatches(ctx context.Context, age, pause time.Duration, del func(context.Context, time.Duration, int) (int64, error)) int64 {
	var total int64
	for ctx.Err() == nil {
		qctx, cancel := context.WithTimeout(ctx, time.Minute)
		n, err := del(qctx, age, repositories.CleanupBatchSize)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("cleanup: %v", err)
			}
			break
		}
		total += n
		if n < repositories.CleanupBatchSize {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(pause):
		}
	}
	return total
}

package main

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// fakeCleanup reports `left` rows remaining and removes at most limit per call.
type fakeCleanup struct{ left, calls int }

func (f *fakeCleanup) del(_ context.Context, _ time.Duration, limit int) (int64, error) {
	f.calls++
	n := min(f.left, limit)
	f.left -= n
	return int64(n), nil
}

func TestDeleteInBatchesStopsOnShortBatch(t *testing.T) {
	f := &fakeCleanup{left: 2*repositories.CleanupBatchSize + 5}
	total := deleteInBatches(context.Background(), time.Hour, 0, f.del)
	if total != int64(2*repositories.CleanupBatchSize+5) || f.calls != 3 || f.left != 0 {
		t.Fatalf("total=%d calls=%d left=%d", total, f.calls, f.left)
	}
}

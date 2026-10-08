package repositories

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestFailJobInvalidUTF8: a provider error cut in the middle of a
// multi-byte letter (or an endless raw body) must still be recorded —
// PostgreSQL rejects invalid UTF-8, and a failed FailJob left the job
// 'running' until the stuck-job reaper.
func TestFailJobInvalidUTF8(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	sid, err := testutil.CreateSubject(ctx, pool, "FailJob utf8 "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.EnqueueChainJobNow(ctx, sid, 1); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM generation_jobs WHERE subject_id = $1`, sid).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE generation_jobs SET status = 'running', attempts = 1 WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	broken := "groq: HTTP 400: ответ модели " + string([]byte("щ")[:1]) + strings.Repeat("x", 20000)
	if err := gen.FailJob(ctx, jobID, errors.New(broken), time.Minute, 3); err != nil {
		t.Fatalf("FailJob with invalid UTF-8: %v", err)
	}
	var status, last string
	if err := pool.QueryRow(ctx, `SELECT status, last_error FROM generation_jobs WHERE id = $1`, jobID).Scan(&status, &last); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || !utf8.ValidString(last) || len(last) > maxJobErrorLen+8 || !strings.HasPrefix(last, "groq: HTTP 400") {
		t.Fatalf("status=%q len=%d valid=%v", status, len(last), utf8.ValidString(last))
	}
}

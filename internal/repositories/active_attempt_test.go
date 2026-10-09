package repositories

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// Test mode state (users.active_attempt_id, migration 000031).
func TestActiveAttemptLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	users := NewUserRepository(pool)
	gen := NewGenerationRepository(pool)

	stamp := time.Now().Format("150405.000000")
	sid, err := testutil.CreateSubject(ctx, pool, "Активный тест "+stamp)
	if err != nil {
		t.Fatal(err)
	}
	seed := []models.SeedQuestion{{Text: "Вопрос " + stamp + "?", Options: [4]string{"a", "b", "c", "d"}, Topic: "Тема", Difficulty: 1}}
	test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: 1, Title: "Тест 1", Kind: models.TestKindChain}, seed)
	if err != nil {
		t.Fatal(err)
	}
	u, err := users.Upsert(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 5_500_000_000, FirstName: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ActiveAttemptID != 0 {
		t.Fatalf("new user active = %d", u.ActiveAttemptID)
	}
	// One in-progress attempt per (user, test) — every attempt gets its
	// own test of the chain.
	num := 1
	newAttempt := func() int64 {
		num++
		tt, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: num, Title: fmt.Sprintf("Тест %d", num),
			Kind: models.TestKindChain}, []models.SeedQuestion{{Text: fmt.Sprintf("Вопрос %s %d?", stamp, num),
			Options: [4]string{"a", "b", "c", "d"}, Topic: "Тема", Difficulty: 1}})
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO test_attempts (user_id, test_id) VALUES ($1, $2) RETURNING id`, u.ID, tt.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var a1 int64
	if err := pool.QueryRow(ctx, `INSERT INTO test_attempts (user_id, test_id) VALUES ($1, $2) RETURNING id`, u.ID, test.ID).Scan(&a1); err != nil {
		t.Fatal(err)
	}
	a2 := newAttempt()

	if at, err := users.ActiveTestOf(ctx, u.ID); err != nil || at != nil {
		t.Fatalf("no test: %+v %v", at, err)
	}
	if changed, err := users.EnterAttempt(ctx, u.ID, a1); err != nil || !changed {
		t.Fatalf("enter a1: %v %v", changed, err)
	}
	// Upsert carries the pointer (zero-query router check).
	if again, _ := users.Upsert(ctx, &models.User{TelegramID: u.TelegramID, FirstName: "A"}); again.ActiveAttemptID != a1 {
		t.Fatalf("upsert active = %d, want %d", again.ActiveAttemptID, a1)
	}
	if changed, err := users.EnterAttempt(ctx, u.ID, a1); err != nil || changed {
		t.Fatalf("re-enter a1 (no write): %v %v", changed, err)
	}
	if _, err := users.EnterAttempt(ctx, u.ID, a2); !errors.Is(err, ErrOtherTestActive) {
		t.Fatalf("enter a2 during a1: %v", err)
	}
	at, err := users.ActiveTestOf(ctx, u.ID)
	if err != nil || at == nil || at.AttemptID != a1 || at.TestID != test.ID || at.TestKind != models.TestKindChain ||
		at.Title != "Тест 1" || at.SubjectName != "Активный тест "+stamp {
		t.Fatalf("active test: %+v %v", at, err)
	}
	// Leaving another attempt changes nothing; leaving a1 clears it.
	if cleared, _ := users.LeaveAttempt(ctx, u.ID, a2); cleared {
		t.Fatal("leave a2 cleared a1")
	}
	if cleared, _ := users.LeaveAttempt(ctx, u.ID, a1); !cleared {
		t.Fatal("leave a1")
	}
	if cleared, _ := users.LeaveAttempt(ctx, u.ID, a1); cleared {
		t.Fatal("double leave")
	}

	// A closed attempt can not be entered; a pointer to a closed attempt
	// does not block and is cleared lazily.
	if _, err := users.EnterAttempt(ctx, u.ID, a2); err != nil {
		t.Fatal(err)
	}
	_, _ = pool.Exec(ctx, `UPDATE test_attempts SET status = 'abandoned' WHERE id = $1`, a2)
	if changed, err := users.EnterAttempt(ctx, u.ID, a2); err != nil || changed {
		t.Fatalf("enter closed: %v %v", changed, err)
	}
	if changed, err := users.EnterAttempt(ctx, u.ID, a1); err != nil || !changed {
		t.Fatalf("enter over a stale pointer: %v %v", changed, err)
	}
	_, _ = pool.Exec(ctx, `UPDATE test_attempts SET status = 'completed', completed_at = now() WHERE id = $1`, a1)
	if at, err := users.ActiveTestOf(ctx, u.ID); err != nil || at != nil {
		t.Fatalf("stale pointer: %+v %v", at, err)
	}
	var ptr *int64
	_ = pool.QueryRow(ctx, `SELECT active_attempt_id FROM users WHERE id = $1`, u.ID).Scan(&ptr)
	if ptr != nil {
		t.Fatalf("stale pointer not cleared: %d", *ptr)
	}
	// Another user's attempt is never entered.
	other, _ := users.Upsert(ctx, &models.User{TelegramID: u.TelegramID + 1, FirstName: "B"})
	a3 := newAttempt()
	if changed, err := users.EnterAttempt(ctx, other.ID, a3); err != nil || changed {
		t.Fatalf("foreign attempt: %v %v", changed, err)
	}

	// Deleting the attempt (finished personal / custom test, cleanup)
	// clears the pointer (ON DELETE SET NULL).
	if _, err := users.EnterAttempt(ctx, u.ID, a3); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM test_attempts WHERE id = $1`, a3); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT active_attempt_id FROM users WHERE id = $1`, u.ID).Scan(&ptr)
	if ptr != nil {
		t.Fatal("pointer survived the attempt delete")
	}

	// Concurrent entries into different attempts: exactly one wins.
	for round := 0; round < 5; round++ {
		ids := []int64{newAttempt(), newAttempt(), newAttempt(), newAttempt()}
		var wg sync.WaitGroup
		var mu sync.Mutex
		won, blocked := 0, 0
		for _, id := range ids {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()
				changed, err := users.EnterAttempt(ctx, u.ID, id)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case errors.Is(err, ErrOtherTestActive):
					blocked++
				case err != nil:
					t.Errorf("enter: %v", err)
				case changed:
					won++
				}
			}(id)
		}
		wg.Wait()
		if won != 1 || blocked != len(ids)-1 {
			t.Fatalf("round %d: won %d, blocked %d", round, won, blocked)
		}
		at, _ := users.ActiveTestOf(ctx, u.ID)
		if at == nil {
			t.Fatal("no active test after the race")
		}
		if _, err := users.LeaveAttempt(ctx, u.ID, at.AttemptID); err != nil {
			t.Fatal(err)
		}
		_, _ = pool.Exec(ctx, fmt.Sprintf(`UPDATE test_attempts SET status = 'abandoned' WHERE id IN (%d,%d,%d,%d)`, ids[0], ids[1], ids[2], ids[3]))
	}
}

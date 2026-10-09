package repositories

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrOtherTestActive: the user is inside another test (users.active_attempt_id
// points to a different in-progress attempt) — a new test / menu section
// must not open until that one is finished or exited.
var ErrOtherTestActive = errors.New("another test is in progress")

// ActiveTest is the test the user is inside right now («test mode»).
type ActiveTest struct {
	AttemptID   int64
	TestID      int64
	TestKind    string
	TestNumber  int
	Title       string
	SubjectName string
}

// EnterAttempt makes attemptID the user's active test (migration 000031).
//
//   - changed = true: the pointer moved to attemptID (the caller hides the
//     main menu — sends the test-mode keyboard);
//   - changed = false, nil: attemptID already is the active test, or it is
//     not an in-progress attempt of the user (nothing to enter — the caller
//     shows the «attempt closed» screen as before);
//   - ErrOtherTestActive: another in-progress attempt is active.
//
// One UPDATE in the common case. Concurrent calls for two different
// attempts are serialised by the users row lock: the loser re-checks the
// WHERE clause against the winner's row version (READ COMMITTED
// re-evaluation) and sees a live active attempt — exactly one test wins.
func (r *UserRepository) EnterAttempt(ctx context.Context, userID, attemptID int64) (changed bool, err error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE users u SET active_attempt_id = $2
		WHERE u.id = $1
		  AND u.active_attempt_id IS DISTINCT FROM $2
		  AND EXISTS (SELECT 1 FROM test_attempts n
		              WHERE n.id = $2 AND n.user_id = $1 AND n.status = 'in_progress')
		  AND (u.active_attempt_id IS NULL OR NOT EXISTS (
		          SELECT 1 FROM test_attempts a
		          WHERE a.id = u.active_attempt_id AND a.user_id = $1 AND a.status = 'in_progress'))`,
		userID, attemptID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	// Not changed: already active, nothing to enter, or blocked.
	var cur int64
	var live bool
	err = r.pool.QueryRow(ctx, `
		SELECT COALESCE(u.active_attempt_id, 0),
		       EXISTS (SELECT 1 FROM test_attempts a
		               WHERE a.id = u.active_attempt_id AND a.user_id = u.id AND a.status = 'in_progress')
		FROM users u WHERE u.id = $1`, userID).Scan(&cur, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if cur != 0 && cur != attemptID && live {
		return false, ErrOtherTestActive
	}
	return false, nil
}

// LeaveAttempt clears the active test if it is attemptID (exit, finish).
// cleared = the pointer was attemptID (the caller brings the main menu back).
func (r *UserRepository) LeaveAttempt(ctx context.Context, userID, attemptID int64) (cleared bool, err error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE users SET active_attempt_id = NULL
		WHERE id = $1 AND active_attempt_id = $2`, userID, attemptID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ActiveTestOf returns the test the user is inside, or nil. A pointer to an
// attempt that is no longer in progress (reaped / restarted / completed by
// a concurrent update) means «no test» and is cleared on the way (only if
// it still points to a dead attempt — a concurrent EnterAttempt is never
// undone).
func (r *UserRepository) ActiveTestOf(ctx context.Context, userID int64) (*ActiveTest, error) {
	at := &ActiveTest{}
	err := r.pool.QueryRow(ctx, `
		SELECT a.id, a.test_id, t.kind, t.test_number, COALESCE(t.title, ''), COALESCE(s.name, '')
		FROM users u
		JOIN test_attempts a ON a.id = u.active_attempt_id AND a.user_id = u.id AND a.status = 'in_progress'
		JOIN tests t ON t.id = a.test_id
		LEFT JOIN subjects s ON s.id = t.subject_id
		WHERE u.id = $1`, userID).Scan(&at.AttemptID, &at.TestID, &at.TestKind, &at.TestNumber, &at.Title, &at.SubjectName)
	if err == nil {
		return at, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := r.pool.Exec(ctx, `
		UPDATE users u SET active_attempt_id = NULL
		WHERE u.id = $1 AND u.active_attempt_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM test_attempts a
		                  WHERE a.id = u.active_attempt_id AND a.user_id = u.id AND a.status = 'in_progress')`,
		userID); err != nil {
		return nil, err
	}
	return nil, nil
}

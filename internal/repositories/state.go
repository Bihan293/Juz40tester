package repositories

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// StateRepository persists per-user subject state (tests grid page,
// progress in the chain, generation requests) so nothing is lost when the
// user leaves the bot.
type StateRepository struct {
	pool *pgxpool.Pool
}

func NewStateRepository(pool *pgxpool.Pool) *StateRepository {
	return &StateRepository{pool: pool}
}

// Get returns the user's state for a subject; zero-value state if absent.
func (r *StateRepository) Get(ctx context.Context, userID, subjectID int64) (*models.UserSubjectState, error) {
	var s models.UserSubjectState
	err := r.pool.QueryRow(ctx, `
		SELECT user_id, subject_id, tests_page, last_test_number
		FROM user_subject_state
		WHERE user_id = $1 AND subject_id = $2`, userID, subjectID).
		Scan(&s.UserID, &s.SubjectID, &s.TestsPage, &s.LastTestNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return &models.UserSubjectState{UserID: userID, SubjectID: subjectID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SavePage remembers the last tests-grid page the user opened.
func (r *StateRepository) SavePage(ctx context.Context, userID, subjectID int64, page int) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO user_subject_state (user_id, subject_id, tests_page)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, subject_id) DO UPDATE SET
			tests_page = EXCLUDED.tests_page,
			updated_at = now()`, userID, subjectID, page)
	return err
}

// SaveProgress bumps the completed-chain watermark (never lowers it) without
// touching the remembered page. The legacy requested_up_to column is no
// longer read or written (it was always 0 → GREATEST no-op).
func (r *StateRepository) SaveProgress(ctx context.Context, userID, subjectID int64, lastTestNumber int) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO user_subject_state (user_id, subject_id, last_test_number)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, subject_id) DO UPDATE SET
			last_test_number = GREATEST(user_subject_state.last_test_number, EXCLUDED.last_test_number),
			updated_at = now()`, userID, subjectID, lastTestNumber)
	return err
}

// Watermarks returns the permanent unlock watermark (last_test_number) of
// every subject the user has state for — one query for the statistics
// screen instead of Get per subject. Missing subjects mean watermark 0.
func (r *StateRepository) Watermarks(ctx context.Context, userID int64) (map[int64]int, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT subject_id, last_test_number FROM user_subject_state
		WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var sid int64
		var n int
		if err := rows.Scan(&sid, &n); err != nil {
			return nil, err
		}
		out[sid] = n
	}
	return out, rows.Err()
}

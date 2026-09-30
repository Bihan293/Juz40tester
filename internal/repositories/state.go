package repositories

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40-test2/internal/models"
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
		SELECT user_id, subject_id, tests_page, last_test_number, requested_up_to
		FROM user_subject_state
		WHERE user_id = $1 AND subject_id = $2`, userID, subjectID).
		Scan(&s.UserID, &s.SubjectID, &s.TestsPage, &s.LastTestNumber, &s.RequestedUpTo)
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

// SaveProgress bumps the completed-chain watermark and/or the requested
// generation watermark without touching the remembered page.
func (r *StateRepository) SaveProgress(ctx context.Context, userID, subjectID int64, lastTestNumber, requestedUpTo int) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO user_subject_state (user_id, subject_id, last_test_number, requested_up_to)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, subject_id) DO UPDATE SET
			last_test_number = GREATEST(user_subject_state.last_test_number, EXCLUDED.last_test_number),
			requested_up_to  = GREATEST(user_subject_state.requested_up_to, EXCLUDED.requested_up_to),
			updated_at = now()`, userID, subjectID, lastTestNumber, requestedUpTo)
	return err
}

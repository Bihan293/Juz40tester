// Package repositories contains all database access code.
package repositories

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40-test2/internal/models"
)

// UserRepository handles persistence of Telegram users.
type UserRepository struct {
	pool *pgxpool.Pool
}

func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

// Upsert creates a user on /start or refreshes profile data if the user
// already exists. It also maintains the daily-activity streak (the 🔥
// "огонёк") in the same single query: one row touch per interaction at most,
// and the streak columns change only once per UTC calendar day.
//
// Streak rules (computed against the UTC date so the logic is timezone-free):
//   - first visit ever: streak = 1;
//   - activity on the next calendar day: streak + 1;
//   - a missed day: streak resets to 1.
//
// It returns the internal user row.
func (r *UserRepository) Upsert(ctx context.Context, u *models.User) (*models.User, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO users (telegram_id, username, first_name, last_name, language_code,
		                   streak_days, last_active_date)
		VALUES ($1, $2, $3, $4, $5, 1, CURRENT_DATE)
		ON CONFLICT (telegram_id) DO UPDATE SET
			username      = EXCLUDED.username,
			first_name    = EXCLUDED.first_name,
			last_name     = EXCLUDED.last_name,
			language_code = EXCLUDED.language_code,
			updated_at    = now(),
			streak_days   = CASE
				WHEN users.last_active_date = CURRENT_DATE THEN users.streak_days
				WHEN users.last_active_date = CURRENT_DATE - 1 THEN users.streak_days + 1
				ELSE 1
			END,
			last_active_date = CASE
				WHEN users.last_active_date = CURRENT_DATE THEN users.last_active_date
				ELSE CURRENT_DATE
			END
		RETURNING id, telegram_id, username, first_name, last_name, language_code, created_at, updated_at,
		          streak_days, COALESCE(last_active_date::text, ''), test_lang`,
		u.TelegramID, u.Username, u.FirstName, u.LastName, u.LanguageCode)

	out := &models.User{}
	var lastActive string
	err := row.Scan(&out.ID, &out.TelegramID, &out.Username, &out.FirstName,
		&out.LastName, &out.LanguageCode, &out.CreatedAt, &out.UpdatedAt,
		&out.StreakDays, &lastActive, &out.TestLang)
	if err != nil {
		return nil, err
	}
	if t, perr := time.Parse("2006-01-02", lastActive); perr == nil {
		out.LastActiveDate = t
	}
	return out, nil
}

// SetTestLang switches the language of the TEST CONTENT for the user
// (models.TestLangRU / models.TestLangKK). The bot interface always stays
// Russian — this flag only decides which version of a test the user gets.
func (r *UserRepository) SetTestLang(ctx context.Context, userID int64, lang string) error {
	if lang != models.TestLangRU && lang != models.TestLangKK {
		lang = models.TestLangRU
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE users SET test_lang = $2, updated_at = now()
		WHERE id = $1`, userID, lang)
	return err
}

// LeaderboardEntry is one row of a leaderboard query result.
type LeaderboardEntry = models.LeaderboardEntry

// streakBoard is the shared SELECT for the streak leaderboard.
const streakBoard = `
	SELECT u.id, u.first_name, u.username, u.streak_days
	FROM users u
	WHERE u.streak_days > 0
	ORDER BY u.streak_days DESC, u.id
	LIMIT $1`

// StreakLeaderboard returns the top users by daily-activity streak. One
// indexed scan over the users table — cheap enough to run on demand.
func (r *UserRepository) StreakLeaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error) {
	rows, err := r.pool.Query(ctx, streakBoard, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.LeaderboardEntry
	for rows.Next() {
		var e models.LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.FirstName, &e.Username, &e.StreakDays); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

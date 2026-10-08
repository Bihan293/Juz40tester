// Package repositories contains all database access code.
package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
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
// "огонёк") in the same single query. The row is WRITTEN only when the
// profile changed or on the first activity of a calendar day; all other
// taps are reads. Every message
// and every button tap (including answering test questions) goes through
// here, so passing tests keeps the streak alive.
//
// Streak rules (models.NextStreak, the calendar is models.StreakLocation —
// Kazakhstan, UTC+5; "today" is computed in Go and passed as a DATE, so the
// result does not depend on the database session time zone):
//   - first visit ever: streak = 1;
//   - activity on the next calendar day: streak + 1;
//   - a missed day: streak resets to 1.
//
// It returns the internal user row.
func (r *UserRepository) Upsert(ctx context.Context, u *models.User) (*models.User, error) {
	return r.upsertAt(ctx, u, models.StreakToday(time.Now()))
}

// upsertAt retries once on ErrNoRows: when two very first updates of the same
// user race, the losing INSERT conflicts with a row committed AFTER its
// statement snapshot, so the read-only branch may not see it yet. A new
// statement gets a fresh snapshot and finds the row.
func (r *UserRepository) upsertAt(ctx context.Context, u *models.User, today time.Time) (*models.User, error) {
	out, err := r.upsertOnce(ctx, u, today)
	if errors.Is(err, pgx.ErrNoRows) {
		out, err = r.upsertOnce(ctx, u, today)
	}
	return out, err
}

// upsertOnce is one attempt of Upsert with an explicit streak "today".
//
// The UPDATE branch only fires when something actually changes: the
// Telegram profile (username / names / language) or the calendar day of
// the streak (first activity of a new day, or a broken streak to repair).
// Every other tap — including every answer in a test — is a pure read:
// the ON CONFLICT ... WHERE guard skips the write, and the second branch of
// the UNION returns the existing row in the same round trip. Before this
// fix each tap rewrote the row (UPDATE users SET updated_at = now()).
func (r *UserRepository) upsertOnce(ctx context.Context, u *models.User, today time.Time) (*models.User, error) {
	row := r.pool.QueryRow(ctx, `
		WITH up AS (
			INSERT INTO users (telegram_id, username, first_name, last_name, language_code,
			                   streak_days, last_active_date)
			VALUES ($1, $2, $3, $4, $5, 1, $6::date)
			ON CONFLICT (telegram_id) DO UPDATE SET
				username      = EXCLUDED.username,
				first_name    = EXCLUDED.first_name,
				last_name     = EXCLUDED.last_name,
				language_code = EXCLUDED.language_code,
				updated_at    = now(),
				streak_days   = CASE
					WHEN users.last_active_date IS NULL OR users.streak_days <= 0 THEN 1
					-- same day, or a late/out-of-order update dated before the
					-- last activity (midnight race, clock skew): never reset.
					WHEN users.last_active_date >= $6::date THEN users.streak_days
					WHEN users.last_active_date = $6::date - 1 THEN users.streak_days + 1
					ELSE 1
				END,
				last_active_date = GREATEST(COALESCE(users.last_active_date, $6::date), $6::date),
				-- the user writes to the bot again: no longer «blocked».
				blocked_at    = NULL
			WHERE users.username      IS DISTINCT FROM EXCLUDED.username
			   OR users.first_name    IS DISTINCT FROM EXCLUDED.first_name
			   OR users.last_name     IS DISTINCT FROM EXCLUDED.last_name
			   OR users.language_code IS DISTINCT FROM EXCLUDED.language_code
			   OR users.last_active_date IS NULL
			   OR users.last_active_date < $6::date
			   OR users.streak_days <= 0
			   OR users.blocked_at IS NOT NULL
			RETURNING id, telegram_id, username, first_name, last_name, language_code, created_at, updated_at,
			          streak_days, COALESCE(last_active_date::text, ''), test_lang
		)
		SELECT * FROM up
		UNION ALL
		SELECT id, telegram_id, username, first_name, last_name, language_code, created_at, updated_at,
		       streak_days, COALESCE(last_active_date::text, ''), test_lang
		FROM users
		WHERE telegram_id = $1 AND NOT EXISTS (SELECT 1 FROM up)
		LIMIT 1`,
		u.TelegramID, u.Username, u.FirstName, u.LastName, u.LanguageCode, today.Format("2006-01-02"))

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
//
// The stored streak_days only changes when the user is active, so a user
// who disappeared a week ago still has the old number in the row. The board
// therefore shows only LIVE flames (active today or yesterday — see
// models.EffectiveStreak); before this fix it kept ranking long-extinguished
// streaks.
const streakBoard = `
	SELECT u.id, u.first_name, u.username, u.streak_days
	FROM users u
	WHERE u.streak_days > 0 AND u.last_active_date >= $2::date - 1
	ORDER BY u.streak_days DESC, u.id
	LIMIT $1`

// liveStreakSQL is the SQL form of models.EffectiveStreak for the user
// alias u and the "today" parameter $N (used by the subject boards).
func liveStreakSQL(todayParam string) string {
	return `CASE WHEN u.last_active_date >= ` + todayParam + `::date - 1 THEN u.streak_days ELSE 0 END`
}

// StreakLeaderboard returns the top users by daily-activity streak. One
// indexed scan over the users table — cheap enough to run on demand.
func (r *UserRepository) StreakLeaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error) {
	rows, err := r.pool.Query(ctx, streakBoard, limit, models.StreakToday(time.Now()).Format("2006-01-02"))
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

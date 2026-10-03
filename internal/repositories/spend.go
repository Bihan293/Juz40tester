package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSpendCapReached: the daily spending cap of a paid AI provider would be
// exceeded by the requested reservation.
var ErrSpendCapReached = errors.New("daily AI spending cap reached")

// SpendRepository is the per-day ledger of paid AI spend (ai_spend_daily).
type SpendRepository struct {
	pool *pgxpool.Pool
}

func NewSpendRepository(pool *pgxpool.Pool) *SpendRepository {
	return &SpendRepository{pool: pool}
}

// Reserve atomically books amount USD for provider on day, unless
// cost + reserved + amount would exceed capUSD (ErrSpendCapReached). The
// conditional UPDATE runs under the row lock, so concurrent reservations
// (other goroutines or instances) can never push the day over the cap.
func (r *SpendRepository) Reserve(ctx context.Context, provider string, day time.Time, amount, capUSD float64) error {
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO ai_spend_daily (provider, day) VALUES ($1, $2::date)
		ON CONFLICT DO NOTHING`, provider, day); err != nil {
		return err
	}
	var spent float64
	err := r.pool.QueryRow(ctx, `
		UPDATE ai_spend_daily
		SET reserved_usd = reserved_usd + $3, updated_at = now()
		WHERE provider = $1 AND day = $2::date
		  AND cost_usd + reserved_usd + $3 <= $4
		RETURNING cost_usd`, provider, day, amount, capUSD).Scan(&spent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSpendCapReached
	}
	return err
}

// Settle replaces a reservation by the actual (estimated) cost of the call.
func (r *SpendRepository) Settle(ctx context.Context, provider string, day time.Time, reserved, actual float64) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO ai_spend_daily (provider, day, cost_usd, calls)
		VALUES ($1, $2::date, $4, 1)
		ON CONFLICT (provider, day) DO UPDATE SET
			cost_usd     = ai_spend_daily.cost_usd + EXCLUDED.cost_usd,
			reserved_usd = GREATEST(ai_spend_daily.reserved_usd - $3, 0),
			calls        = ai_spend_daily.calls + 1,
			updated_at   = now()`, provider, day, reserved, actual)
	return err
}

// Spent returns the settled cost and the open reservations of the day.
func (r *SpendRepository) Spent(ctx context.Context, provider string, day time.Time) (cost, reserved float64, err error) {
	err = r.pool.QueryRow(ctx, `
		SELECT cost_usd, reserved_usd FROM ai_spend_daily
		WHERE provider = $1 AND day = $2::date`, provider, day).Scan(&cost, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return cost, reserved, err
}

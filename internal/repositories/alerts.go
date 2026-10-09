package repositories

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// alertDay is the fixed ai_spend_daily.day of the alert-claim rows.
const alertDay = "1970-01-01"

// ClaimAlert claims the right to send the administrator alert `key` now,
// cluster-wide: true when the key was never sent or its last send is older
// than every (the claim then books "now"). Several instances (zero-downtime
// deploys) and restarts therefore never repeat an alert inside its
// interval.
//
// The claims live in the small ai_spend_daily ledger under the provider
// "alert:<key>" on a fixed day (no extra migration); they never mix with
// the DeepSeek rows, which are always read by provider = 'deepseek'.
func (r *SpendRepository) ClaimAlert(ctx context.Context, key string, every time.Duration) (bool, error) {
	var one int
	err := r.pool.QueryRow(ctx, `
		INSERT INTO ai_spend_daily (provider, day, updated_at)
		VALUES ($1, $2::date, now())
		ON CONFLICT (provider, day) DO UPDATE SET updated_at = now()
		WHERE ai_spend_daily.updated_at <= now() - make_interval(secs => $3)
		RETURNING 1`, "alert:"+key, alertDay, every.Seconds()).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

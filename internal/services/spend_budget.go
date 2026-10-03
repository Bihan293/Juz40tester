package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// SpendProviderDeepSeek is the ai_spend_daily.provider key of DeepSeek.
const SpendProviderDeepSeek = "deepseek"

// DailyBudget implements deepseek.Budget on top of the ai_spend_daily
// ledger (R-9). The day is the UTC calendar day (DeepSeek's billing day).
// The ledger lives in PostgreSQL, so the cap holds across goroutines,
// restarts AND instances; it is fed by the same per-call cost estimate the
// DeepSeek client logs (no second cost model).
type DailyBudget struct {
	repo   *repositories.SpendRepository
	capUSD float64
	now    func() time.Time
}

// NewDailyBudget returns nil when capUSD <= 0 (cap disabled).
func NewDailyBudget(repo *repositories.SpendRepository, capUSD float64) *DailyBudget {
	if repo == nil || capUSD <= 0 {
		return nil
	}
	return &DailyBudget{repo: repo, capUSD: capUSD, now: time.Now}
}

func (b *DailyBudget) day() time.Time {
	y, m, d := b.now().UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Reserve books the worst-case cost of one call or reports the cap.
func (b *DailyBudget) Reserve(ctx context.Context, amountUSD float64) error {
	err := b.repo.Reserve(ctx, SpendProviderDeepSeek, b.day(), amountUSD, b.capUSD)
	if errors.Is(err, repositories.ErrSpendCapReached) {
		log.Printf("deepseek: daily spending cap $%.2f reached — paid call refused (needed ~$%.4f)", b.capUSD, amountUSD)
		return fmt.Errorf("%w ($%.2f/day)", deepseek.ErrBudgetExceeded, b.capUSD)
	}
	return err
}

// Settle replaces the reservation by the estimated actual cost.
func (b *DailyBudget) Settle(ctx context.Context, reservedUSD, actualUSD float64) {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.repo.Settle(sctx, SpendProviderDeepSeek, b.day(), reservedUSD, actualUSD); err != nil {
		log.Printf("deepseek: spend ledger settle: %v", err)
	}
}

// Exhausted reports whether not even a minimal call fits under the cap any
// more today (cheap pre-check, the real guard is Reserve).
func (b *DailyBudget) Exhausted(ctx context.Context) bool {
	if b == nil {
		return false
	}
	cost, reserved, err := b.repo.Spent(ctx, SpendProviderDeepSeek, b.day())
	if err != nil {
		return false // the hard guard (Reserve) fails closed anyway
	}
	return cost+reserved >= b.capUSD
}

// NextDay is the start of the next budget day (UTC midnight).
func (b *DailyBudget) NextDay() time.Time { return b.day().Add(24 * time.Hour) }

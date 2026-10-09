package repositories

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ClaimAlert: one send per key and interval, cluster-wide; DeepSeek spend
// rows are untouched.
func TestClaimAlertInterval(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewSpendRepository(pool)
	key := fmt.Sprintf("test-%d", time.Now().UnixNano())
	ok, err := r.ClaimAlert(ctx, key, time.Hour)
	if err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if ok, err := r.ClaimAlert(ctx, key, time.Hour); err != nil || ok {
		t.Fatalf("repeat inside the interval must be refused: %v %v", ok, err)
	}
	if ok, err := r.ClaimAlert(ctx, key+"-other", time.Hour); err != nil || !ok {
		t.Fatalf("another key is independent: %v %v", ok, err)
	}
	// Age the claim: the interval is over.
	if _, err := pool.Exec(ctx, `UPDATE ai_spend_daily SET updated_at = now() - interval '2 hours' WHERE provider = $1`, "alert:"+key); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.ClaimAlert(ctx, key, time.Hour); err != nil || !ok {
		t.Fatalf("claim after the interval: %v %v", ok, err)
	}
	if cost, reserved, err := r.Spent(ctx, "deepseek", time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil || cost != 0 || reserved != 0 {
		t.Fatalf("alert claims must not look like DeepSeek spend: %v %v %v", cost, reserved, err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM ai_spend_daily WHERE provider LIKE 'alert:test-%'`)
}

package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// RaiseWatermark reports a NEW unlock only when the watermark goes up.
func TestRaiseWatermark(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	users := NewUserRepository(pool)
	state := NewStateRepository(pool)
	sid, err := testutil.CreateSubject(ctx, pool, "WM "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := users.Upsert(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 8_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SavePage(ctx, user.ID, sid, 2); err != nil { // row exists, watermark 0
		t.Fatal(err)
	}
	for _, c := range []struct {
		n    int
		want bool
	}{{3, true}, {3, false}, {1, false}, {4, true}} {
		got, err := state.RaiseWatermark(ctx, user.ID, sid, c.n)
		if err != nil || got != c.want {
			t.Fatalf("RaiseWatermark(%d) = %v, %v; want %v", c.n, got, err, c.want)
		}
	}
	st, err := state.Get(ctx, user.ID, sid)
	if err != nil || st.LastTestNumber != 4 || st.TestsPage != 2 {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

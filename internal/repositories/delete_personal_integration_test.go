package repositories

import (
	"context"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestDeletePersonalTestOtherLineage (A2.7): a test of ANOTHER lineage with
// the same fingerprint is not a «copy» — finishing the last test of a
// lineage keeps it as a hidden template instead of deleting it.
func TestDeletePersonalTestOtherLineage(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	users := NewUserRepository(pool)

	sid, err := testutil.CreateSubject(ctx, pool, "Линейки "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(off int64) *models.User {
		u, err := users.upsertAt(ctx, &models.User{TelegramID: time.Now().UnixNano()%1_000_000_000 + 4_000_000_000 + off}, d("2026-10-01"))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	u1, u2 := mk(0), mk(7)
	q := func(p string) []models.SeedQuestion {
		return []models.SeedQuestion{{Text: "Вопрос линейки " + p, Options: [4]string{"a", "b", "c", "d"}, Topic: "Тема", Difficulty: 2}}
	}
	// Two independent lineages (two generations) with the same fingerprint.
	t1, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, Title: "P1", Kind: models.TestKindPersonal, OwnerUserID: u1.ID, TopicsFingerprint: "fp"}, q("1"))
	if err != nil {
		t.Fatal(err)
	}
	t2, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, Title: "P2", Kind: models.TestKindPersonal, OwnerUserID: u2.ID, TopicsFingerprint: "fp"}, q("2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.DeletePersonalTest(ctx, u1.ID, t1.ID, sid); err != nil {
		t.Fatal(err)
	}
	// B6a: no templates — the finished test is gone.
	var left int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM tests WHERE id = $1`, t1.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("finished personal test must be deleted: left=%d err=%v", left, err)
	}
	var active bool
	// The other lineage is untouched.
	if err := pool.QueryRow(ctx, `SELECT is_active FROM tests WHERE id = $1`, t2.ID).Scan(&active); err != nil || !active {
		t.Fatalf("other lineage changed: active=%v err=%v", active, err)
	}
}

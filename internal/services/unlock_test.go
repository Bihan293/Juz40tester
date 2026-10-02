package services

import (
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// computeUnlocked is the pure walk extracted from unlockedMax (#33): the
// rules must be exactly the old ones.
func TestComputeUnlocked(t *testing.T) {
	chain := []models.Test{{ID: 11, TestNumber: 1}, {ID: 12, TestNumber: 2}, {ID: 13, TestNumber: 3}}
	pass := &repositories.TestProgress{Green: models.UnlockGreen, Yellow: models.UnlockYellow}
	fail := &repositories.TestProgress{Green: models.UnlockGreen - 1, Yellow: models.UnlockYellow + 1}
	cases := []struct {
		name      string
		chain     []models.Test
		progress  map[int64]*repositories.TestProgress
		watermark int
		want      int
	}{
		{"fresh user", chain, nil, 0, 1},
		{"first passed", chain, map[int64]*repositories.TestProgress{11: pass}, 0, 2},
		{"chain breaks at failed", chain, map[int64]*repositories.TestProgress{11: pass, 12: fail, 13: pass}, 0, 2},
		{"all passed → next to generate", chain, map[int64]*repositories.TestProgress{11: pass, 12: pass, 13: pass}, 0, 4},
		{"watermark keeps unlock", chain, map[int64]*repositories.TestProgress{11: fail}, 2, 3},
		{"hole: test 1 missing", []models.Test{{ID: 12, TestNumber: 2}}, nil, 0, 1},
		{"empty chain", nil, nil, 0, 1},
		{"watermark past the chain end", chain, nil, models.MaxVisibleTests, models.MaxVisibleTests + 1},
	}
	for _, c := range cases {
		if got := computeUnlocked(c.chain, c.progress, c.watermark); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

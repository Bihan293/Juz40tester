package handlers

import (
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// After the last chain test is passed the raw level is MaxVisibleTests+1;
// the leaderboard shows at most MaxVisibleTests (like «Открыто тестов»).
func TestLeaderboardLevelClamped(t *testing.T) {
	if got := lbLevel(&models.LeaderboardEntry{UnlockedTests: models.MaxVisibleTests + 1}); got != models.MaxVisibleTests {
		t.Fatalf("level = %d, want %d", got, models.MaxVisibleTests)
	}
	if got := lbLevel(&models.LeaderboardEntry{UnlockedTests: 7}); got != 7 {
		t.Fatalf("level = %d, want 7", got)
	}
}

package services

import (
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// #37: the visible bound uses the highest TestNumber, not len(chain).
func TestMaxChainNumberWithGaps(t *testing.T) {
	chain := []models.Test{{TestNumber: 1}, {TestNumber: 2}, {TestNumber: 3}, {TestNumber: 7}, {TestNumber: 8}}
	if got := maxChainNumber(chain); got != 8 {
		t.Fatalf("maxChainNumber = %d, want 8 (len(chain)=5 would hide 7 and 8)", got)
	}
	if got := maxChainNumber(nil); got != 0 {
		t.Fatalf("empty chain: %d", got)
	}
}

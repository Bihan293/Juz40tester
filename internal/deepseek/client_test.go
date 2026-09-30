package deepseek

import (
	"testing"
)

// TestEstimateCost guards the per-test price target: a typical 20-question
// generation (small prompt, ~4500 output tokens including the thinking
// pass) must stay in the low single-digit cents even at PEAK pricing, and
// the worst case (a full max_tokens=8000 burn at peak) must stay under a
// cent — this is the regression alarm for another "$0.10 per test".
func TestEstimateCost(t *testing.T) {
	typical := &usage{PromptTokens: 2500, CompletionTokens: 4500, PromptCacheHit: 500, PromptCacheMiss: 2000}
	off, peak := estimateCost("deepseek-flash", typical)
	// off-peak: 500*0.003e-6 + 2000*0.15e-6 + 4500*0.6e-6 = 0.0030
	if off < 0.001 || off > 0.005 {
		t.Fatalf("typical flash off-peak cost out of target range: $%.6f", off)
	}
	if peak != off*2 {
		t.Fatalf("peak must be exactly 2x off-peak: %v vs %v", peak, off)
	}

	worst := &usage{PromptTokens: 3000, CompletionTokens: 8000, PromptCacheHit: 0}
	_, worstPeak := estimateCost("deepseek-flash", worst)
	if worstPeak > 0.0105 { // ~$0.0096 + a small margin
		t.Fatalf("worst-case peak cost exceeded the hard ceiling: $%.6f", worstPeak)
	}

	// Unknown models must not crash the estimator (they log without a price).
	if off, peak := estimateCost("some-proxy-model", typical); off != 0 || peak != 0 {
		t.Fatal("unknown model must estimate as 0")
	}
}

// TestDefaults ensures the legacy model names removed from the DeepSeek API
// on 2026-07-24 never come back as defaults — every request with them
// fails with "model not found" and test generation silently dies.
func TestDefaults(t *testing.T) {
	if DefaultReasonerModel == "deepseek-reasoner" || DefaultModel == "deepseek-chat" {
		t.Fatalf("legacy retired model names must not be defaults: %s / %s", DefaultReasonerModel, DefaultModel)
	}
	c := New("key", "", "", "")
	if c.ReasonerModel() != DefaultReasonerModel || c.Model() != DefaultModel {
		t.Fatal("empty env model names must fall back to the defaults")
	}
}

package deepseek

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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

	// An unknown model must not crash the estimator (logged without a price).
	if off, peak := estimateCost("some-proxy-model", typical); off != 0 || peak != 0 {
		t.Fatal("unknown model must estimate as 0")
	}
}

// TestModelIsFlash: the bot calls exactly one model, and it is never one
// of the legacy names removed from the DeepSeek API on 2026-07-24.
func TestModelIsFlash(t *testing.T) {
	if Model != "deepseek-flash" {
		t.Fatalf("Model = %q, want deepseek-flash", Model)
	}
	if len(prices) != 1 {
		t.Fatalf("only the flash price must be known, got %d entries", len(prices))
	}
}

// TestModes: generation is ALWAYS thinking with effort "high"; the
// translation fallback is ALWAYS non-thinking (thinking explicitly
// disabled, no reasoning_effort). Errors are returned as is — no retry
// without thinking, no other model.
func TestModes(t *testing.T) {
	var mu sync.Mutex
	var reqs []map[string]any
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field thinking","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	msgs := []Message{{Role: "user", Content: "x"}}

	if _, err := c.GenerateJSON(context.Background(), msgs, 100); err != nil {
		t.Fatal(err)
	}
	g := reqs[0]
	if g["model"] != Model || g["reasoning_effort"] != "high" || g["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatalf("generation must be flash thinking(high): %v", g)
	}
	if _, ok := g["temperature"]; ok {
		t.Fatalf("thinking mode must not send temperature: %v", g)
	}

	if _, err := c.TranslateJSON(context.Background(), msgs, 100); err != nil {
		t.Fatal(err)
	}
	tr := reqs[1]
	if tr["model"] != Model || tr["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("translation must be flash non-thinking: %v", tr)
	}
	if _, ok := tr["reasoning_effort"]; ok {
		t.Fatalf("non-thinking mode must not send reasoning_effort: %v", tr)
	}
	if tr["max_tokens"].(float64) != 100 {
		t.Fatalf("max_tokens must be passed through: %v", tr)
	}

	reqs, fail = nil, true
	if _, err := c.GenerateJSON(context.Background(), msgs, 100); err == nil {
		t.Fatal("a rejected request must be returned as an error")
	}
	if len(reqs) != 1 {
		t.Fatalf("a rejected request must not be retried in another mode/model: %d calls", len(reqs))
	}
}

// fixedBudget records reservations/settlements.
type fixedBudget struct {
	mu                sync.Mutex
	reserved, settled float64
}

func (b *fixedBudget) Reserve(_ context.Context, amount float64) error {
	b.mu.Lock()
	b.reserved += amount
	b.mu.Unlock()
	return nil
}

func (b *fixedBudget) Settle(_ context.Context, reserved, actual float64) {
	b.mu.Lock()
	b.settled += actual
	b.mu.Unlock()
}

// TestNonThinkingBudget: the non-thinking translation fallback is booked
// against the daily cap with the flash price (same per-token price as
// thinking), and its worst case is bounded by its own (tight) max_tokens.
func TestNonThinkingBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":2000}}`))
	}))
	defer srv.Close()
	b := &fixedBudget{}
	c := New("k", srv.URL).WithBudget(b, func(time.Time) bool { return true })
	if _, err := c.TranslateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 2500); err != nil {
		t.Fatal(err)
	}
	want := 1000*0.15e-6 + 2000*0.60e-6 // off-peak flash
	if math.Abs(b.settled-want) > 1e-12 {
		t.Fatalf("settled $%.8f, want $%.8f", b.settled, want)
	}
	// Worst case booked before the call: 2x (bytes*inMiss + 2500*out).
	if b.reserved <= 2*2500*0.60e-6 || b.reserved > 0.004 {
		t.Fatalf("unexpected worst-case reservation $%.6f", b.reserved)
	}
}

// The per-job statistics of the generator learn the tokens of paid calls
// through WithUsageHook.
func TestUsageHookReportsTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":800,"total_tokens":2000}}`))
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	var in, out int
	ctx := WithUsageHook(context.Background(), func(p, c int) { in += p; out += c })
	if _, err := c.GenerateJSON(ctx, []Message{{Role: "user", Content: "x"}}, 100); err != nil {
		t.Fatal(err)
	}
	if in != 1200 || out != 800 {
		t.Fatalf("hook got in=%d out=%d, want 1200/800", in, out)
	}
	// No hook: nothing breaks.
	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100); err != nil {
		t.Fatal(err)
	}
}

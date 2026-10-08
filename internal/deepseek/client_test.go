package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestThinkingParamErrorClassification(t *testing.T) {
	if isThinkingParamError(errors.New("deepseek: HTTP 400: maximum context length exceeded")) {
		t.Fatal("a context-length 400 must not disable thinking")
	}
	if !isThinkingParamError(errors.New("deepseek: unknown field reasoning_effort (invalid_request_error)")) {
		t.Fatal("explicit reasoning_effort rejection must be detected")
	}
	if isParamError(errors.New("deepseek: HTTP 400: This model's maximum context length is 128k")) {
		t.Fatal("context-length errors must not route to the pricier fallback")
	}
	if isParamError(context.DeadlineExceeded) || isParamError(errors.New("deepseek: HTTP 500: oops")) {
		t.Fatal("timeouts / 5xx must not route to the pricier fallback")
	}
}

// TestFallbackRouting: a 5xx on the reasoner must NOT go to the pricier
// fallback model, and an explicit thinking rejection disables thinking
// once — the next call goes straight to the reasoner without thinking.
func TestFallbackRouting(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	mode := "5xx"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, thinking := req["thinking"]
		mu.Lock()
		tag := req["model"].(string)
		if thinking {
			tag += "+thinking"
		}
		calls = append(calls, tag)
		m := mode
		mu.Unlock()
		switch {
		case m == "5xx":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`))
		case m == "reject-thinking" && thinking:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field thinking","type":"invalid_request_error"}}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`))
		}
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)

	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err == nil {
		t.Fatal("5xx must be returned as an error")
	}
	if len(calls) != 1 || calls[0] != "flash+thinking" || c.thinkingDisabled() {
		t.Fatalf("5xx must not retry / fall back / disable thinking: %v", calls)
	}

	calls, mode = nil, "reject-thinking"
	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != "flash" || !c.thinkingDisabled() {
		t.Fatalf("thinking rejection must retry reasoner without thinking: %v", calls)
	}
	calls = nil
	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "flash" {
		t.Fatalf("with thinking disabled the first call must go without thinking: %v", calls)
	}
}

// The per-job statistics of the generator learn the tokens of paid calls
// through WithUsageHook (they used to count only the free Groq tokens).
func TestUsageHookReportsTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":800,"total_tokens":2000}}`))
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)
	var in, out int
	ctx := WithUsageHook(context.Background(), func(p, c int) { in += p; out += c })
	if _, err := c.GenerateJSON(ctx, []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err != nil {
		t.Fatal(err)
	}
	if in != 1200 || out != 800 {
		t.Fatalf("hook got in=%d out=%d, want 1200/800", in, out)
	}
	// No hook: nothing breaks.
	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err != nil {
		t.Fatal(err)
	}
}

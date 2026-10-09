package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetries shrinks the transient-retry backoff for the test.
func fastRetries(t *testing.T) {
	t.Helper()
	old := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryBackoff = old })
}

const okReply = `{"choices":[{"message":{"content":"{\"ok\":true}"},"finish_reason":"stop"}]}`

// Transient failures (429, 5xx, broken body, empty content) are retried
// inside the call with backoff; the task succeeds once DeepSeek recovers.
func TestTransientErrorsAreRetried(t *testing.T) {
	fastRetries(t)
	for _, failure := range []string{"429", "503", "garbage", "empty"} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n.Add(1) <= 2 {
				switch failure {
				case "429":
					w.WriteHeader(429)
					_, _ = w.Write([]byte(`{"error":{"message":"rate limit","type":"rate_limit"}}`))
				case "503":
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`<html>bad gateway</html>`))
				case "garbage":
					_, _ = w.Write([]byte(`{"choices":[{"mess`))
				case "empty":
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  "},"finish_reason":"stop"}]}`))
				}
				return
			}
			_, _ = w.Write([]byte(okReply))
		}))
		c := New("k", "pro", "flash", srv.URL)
		raw, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow)
		srv.Close()
		if err != nil || !strings.Contains(raw, "ok") {
			t.Fatalf("%s: transient failure must be retried, got %q / %v", failure, raw, err)
		}
		if n.Load() != 3 {
			t.Fatalf("%s: want 3 calls, got %d", failure, n.Load())
		}
	}
}

// A non-transient 400 (not a parameter rejection) is returned at once.
func TestBadRequestIsNotRetried(t *testing.T) {
	fastRetries(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"maximum context length exceeded","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)
	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err == nil {
		t.Fatal("400 must fail")
	}
	if n.Load() != 1 {
		t.Fatalf("400 must not be retried, got %d calls", n.Load())
	}
}

// A retry is not started when the deadline leaves no time for it.
func TestTransientRetryRespectsDeadline(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // < backoff + minAttemptTime
	defer cancel()
	start := time.Now()
	if _, err := c.GenerateJSON(ctx, []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); err == nil || !IsTransient(err) {
		t.Fatalf("want a transient error, got %v", err)
	}
	if n.Load() != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("no retry fits the deadline: %d calls in %s", n.Load(), time.Since(start))
	}
}

// Prod incident: a batch reply truncated at max_tokens with 0 visible chars
// (thinking ate the budget). The client retries ONCE without thinking with
// the non-thinking ceiling.
func TestTruncatedReplyRetriedWithoutThinking(t *testing.T) {
	fastRetries(t)
	var mu sync.Mutex
	type call struct {
		thinking  bool
		maxTokens int
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, thinking := req["thinking"]
		mt, _ := req["max_tokens"].(float64)
		mu.Lock()
		calls = append(calls, call{thinking, int(mt)})
		mu.Unlock()
		if thinking {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`))
			return
		}
		_, _ = w.Write([]byte(okReply))
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)
	raw, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 16000, ThinkingEffortHigh)
	if err != nil || !strings.Contains(raw, "ok") {
		t.Fatalf("truncation must be retried: %q / %v", raw, err)
	}
	if len(calls) != 2 || !calls[0].thinking || calls[0].maxTokens != 16000 ||
		calls[1].thinking || calls[1].maxTokens != NonThinkingMaxTokens {
		t.Fatalf("calls: %+v", calls)
	}
	if c.thinkingDisabled() {
		t.Fatal("a truncation must not disable thinking for later calls")
	}

	// Truncated again → a clear error wrapping ErrTruncated, no loop.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"q"},"finish_reason":"length"}]}`))
	}))
	defer srv2.Close()
	c2 := New("k", "pro", "flash", srv2.URL)
	if _, err := c2.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

// Non-thinking calls are clamped to NonThinkingMaxTokens (a larger value is
// rejected by the endpoint).
func TestNonThinkingMaxTokensClamp(t *testing.T) {
	var got float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		got, _ = req["max_tokens"].(float64)
		_, _ = w.Write([]byte(okReply))
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)
	c.setThinkingUnsupported()
	if _, err := c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 16000, ThinkingEffortLow); err != nil {
		t.Fatal(err)
	}
	if int(got) != NonThinkingMaxTokens {
		t.Fatalf("max_tokens = %v, want %d", got, NonThinkingMaxTokens)
	}
}

// A 5xx whose text mentions "thinking" must not disable thinking forever.
func TestTransientErrorNeverDisablesThinking(t *testing.T) {
	fastRetries(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"thinking backend unsupported right now","type":"server_error"}}`))
	}))
	defer srv.Close()
	c := New("k", "pro", "flash", srv.URL)
	_, _ = c.GenerateJSON(context.Background(), []Message{{Role: "user", Content: "x"}}, 100, ThinkingEffortLow)
	if c.thinkingDisabled() {
		t.Fatal("transient 5xx disabled thinking")
	}
}

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
	fastRetries(t)
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
	// 5xx is transient: retried in place (same model, still thinking),
	// never routed to the pricier fallback, never disables thinking.
	if len(calls) != len(retryBackoff)+1 || c.thinkingDisabled() {
		t.Fatalf("5xx must be retried %d times on the same model: %v", len(retryBackoff), calls)
	}
	for _, cl := range calls {
		if cl != "flash+thinking" {
			t.Fatalf("5xx must not fall back / disable thinking: %v", calls)
		}
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

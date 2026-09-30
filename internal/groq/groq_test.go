package groq

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublishedLimits pins the free-tier numbers documented in
// docs/GROQ_LIMITS.md — changing them must be a deliberate decision.
func TestPublishedLimits(t *testing.T) {
	for _, m := range []string{ModelGPTOSS120B, ModelQwen27B} {
		l := LimitsFor(m)
		if l.RPM != 30 || l.RPD != 1000 || l.TPM != 8000 || l.TPD != 200000 {
			t.Fatalf("%s: unexpected free-tier limits %+v", m, l)
		}
		if l.MaxRequestTokens() >= l.TPM {
			t.Fatalf("%s: a single request must stay below TPM (413 guard)", m)
		}
	}
	if LimitsFor(ModelQwen27B).MaxCompletion != 16384 || LimitsFor(ModelGPTOSS120B).MaxCompletion != 65536 {
		t.Fatal("max completion tokens changed")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("GROQ_TPM", "250000")
	t.Setenv("GROQ_RPM", "1000")
	l := LimitsFor(ModelGPTOSS120B)
	if l.TPM != 250000 || l.RPM != 1000 || l.RPD != 1000 {
		t.Fatalf("override not applied: %+v", l)
	}
	// Per-request cap is still bounded by the context window.
	if l.MaxRequestTokens() > l.ContextWindow {
		t.Fatal("request cap must not exceed the context window")
	}
}

func TestFitMaxTokens(t *testing.T) {
	msgs := []Message{{Role: "user", Content: strings.Repeat("слово ", 200)}}
	n, err := FitMaxTokens(ModelQwen27B, msgs, 100000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n+EstimateTokens(msgs) > LimitsFor(ModelQwen27B).TPM {
		t.Fatalf("prompt + max_tokens exceeds TPM: %d", n+EstimateTokens(msgs))
	}
	huge := []Message{{Role: "user", Content: strings.Repeat("длинный текст ", 3000)}}
	if _, err := FitMaxTokens(ModelQwen27B, huge, 4000, 4000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

// fakeClock drives the limiter deterministically.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time      { return f.t }
func (f *fakeClock) add(d time.Duration) { f.t = f.t.Add(d) }
func newTestLimiter(l Limits) (*limiter, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	lim := newLimiter("m", l)
	lim.now = c.now
	return lim, c
}

func TestLimiterRPM(t *testing.T) {
	lim, clk := newTestLimiter(Limits{RPM: 30, RPD: 1000, TPM: 1 << 30, TPD: 1 << 30})
	soft := lim.lim.RPM // 27 with the safety factor
	for i := 0; i < soft; i++ {
		if _, err := lim.acquire(context.Background(), 1, 0); err != nil {
			t.Fatalf("request %d rejected: %v", i, err)
		}
	}
	_, err := lim.acquire(context.Background(), 1, 0)
	var rl *RateLimitError
	if !errors.As(err, &rl) || !strings.Contains(rl.Reason, "requests per minute") {
		t.Fatalf("expected RPM limit, got %v", err)
	}
	clk.add(61 * time.Second)
	if _, err := lim.acquire(context.Background(), 1, 0); err != nil {
		t.Fatalf("window must slide: %v", err)
	}
}

func TestLimiterTPMAndCommit(t *testing.T) {
	lim, clk := newTestLimiter(Limits{RPM: 100, RPD: 1000, TPM: 8000, TPD: 1 << 30})
	r, err := lim.acquire(context.Background(), 7000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lim.acquire(context.Background(), 2000, 0); !IsRateLimited(err) {
		t.Fatalf("expected TPM limit, got %v", err)
	}
	// Real usage was much smaller than the reservation — quota is freed.
	r.commit(3000)
	if _, err := lim.acquire(context.Background(), 2000, 0); err != nil {
		t.Fatalf("commit must release unused tokens: %v", err)
	}
	clk.add(time.Minute + time.Second)
	if _, _, _, tokDay := lim.snapshot(); tokDay != 5000 {
		t.Fatalf("daily tokens = %d, want 5000", tokDay)
	}
}

func TestLimiterDailyNeverWaits(t *testing.T) {
	lim, _ := newTestLimiter(Limits{RPM: 100, RPD: 2, TPM: 1 << 30, TPD: 1 << 30})
	lim.lim.RPD = 2
	for i := 0; i < 2; i++ {
		if _, err := lim.acquire(context.Background(), 1, 0); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	_, err := lim.acquire(context.Background(), 1, time.Hour)
	if !IsRateLimited(err) || time.Since(start) > time.Second {
		t.Fatalf("exhausted daily quota must fail fast, got %v", err)
	}
}

func TestLimiterTPD(t *testing.T) {
	lim, _ := newTestLimiter(Limits{RPM: 100, RPD: 1000, TPM: 1 << 30, TPD: 10000})
	if _, err := lim.acquire(context.Background(), 8900, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := lim.acquire(context.Background(), 200, 0); !IsRateLimited(err) {
		t.Fatalf("expected TPD limit (soft 9000), got %v", err)
	}
}

func TestParseReset(t *testing.T) {
	cases := map[string]time.Duration{
		"7.66s":    7660 * time.Millisecond,
		"2m59.56s": 2*time.Minute + 59560*time.Millisecond,
		"12":       12 * time.Second,
		"":         0,
	}
	for in, want := range cases {
		if got := parseResetDuration(in); got != want {
			t.Fatalf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestServerHeadersBlockWhenDailyAlmostGone(t *testing.T) {
	lim, _ := newTestLimiter(LimitsFor(ModelQwen27B))
	h := http.Header{}
	h.Set("x-ratelimit-limit-requests", "1000")
	h.Set("x-ratelimit-remaining-requests", "50") // inside the 10% reserve
	h.Set("x-ratelimit-reset-requests", "3h")
	lim.observeHeaders(h)
	if _, err := lim.acquire(context.Background(), 10, time.Minute); !IsRateLimited(err) {
		t.Fatalf("server-reported daily exhaustion must block, got %v", err)
	}
}

// --- client against a fake Groq server ------------------------------------

func okReply(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
	})
	return string(b)
}

func TestClientSuccessAndRequestShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("x-ratelimit-remaining-requests", "999")
		w.Header().Set("x-ratelimit-limit-requests", "1000")
		_, _ = io.WriteString(w, okReply(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	res, err := c.ChatJSON(context.Background(), Request{
		Model: ModelGPTOSS120B, Messages: []Message{{Role: "user", Content: "hi"}},
		MaxTokens: 100000, Effort: EffortLow,
		Schema: map[string]any{"type": "object"},
	})
	if err != nil || res.Content != `{"ok":true}` {
		t.Fatalf("unexpected: %v %+v", err, res)
	}
	mt := int(got["max_completion_tokens"].(float64))
	if mt <= 0 || mt >= 8000 {
		t.Fatalf("max_completion_tokens must be clamped under TPM, got %d", mt)
	}
	if got["reasoning_effort"] != "low" || got["include_reasoning"] != false {
		t.Fatalf("gpt-oss knobs wrong: %v", got)
	}
	rf := got["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("expected json_schema, got %v", rf)
	}
}

func TestClient429BlocksModel(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("retry-after", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached on tokens per minute (TPM)","type":"tokens","code":"rate_limit_exceeded"}}`)
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	req := Request{Model: ModelQwen27B, Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 100}
	if _, err := c.ChatJSON(context.Background(), req); !IsRateLimited(err) {
		t.Fatalf("expected rate limit error, got %v", err)
	}
	// Second call must be refused LOCALLY (no HTTP) while blocked.
	if _, err := c.ChatJSON(context.Background(), req); !IsRateLimited(err) {
		t.Fatalf("expected local block, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("blocked model must not be called again, calls=%d", calls)
	}
	// The other model has its own bucket.
	req.Model = ModelGPTOSS120B
	_, _ = c.ChatJSON(context.Background(), req)
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatal("other model must still be callable")
	}
}

func TestClientSchemaFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			if !strings.Contains(string(body), "json_schema") {
				t.Error("first call must use json_schema")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid JSON schema for response_format","type":"invalid_request_error"}}`)
			return
		}
		if !strings.Contains(string(body), `"json_object"`) {
			t.Error("fallback call must use json_object")
		}
		_, _ = io.WriteString(w, okReply(`{"a":1}`))
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	res, err := c.ChatJSON(context.Background(), Request{
		Model: ModelQwen27B, Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 100,
		Schema: map[string]any{"type": "object"},
	})
	if err != nil || res.Content != `{"a":1}` {
		t.Fatalf("schema fallback failed: %v", err)
	}
}

func TestClientEffortFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"reasoning_effort 'none' is not supported","type":"invalid_request_error"}}`)
			return
		}
		if strings.Contains(string(body), "reasoning_effort") {
			t.Error("retry must omit reasoning_effort")
		}
		_, _ = io.WriteString(w, okReply(`{"a":1}`))
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	req := Request{Model: ModelQwen27B, Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 100, Effort: EffortNone}
	if _, err := c.ChatJSON(context.Background(), req); err != nil {
		t.Fatalf("effort fallback failed: %v", err)
	}
	if _, err := c.ChatJSON(context.Background(), req); err != nil || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("second call must go straight without effort: %v calls=%d", err, calls)
	}
}

func TestClientTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"q\":"},"finish_reason":"length"}]}`)
	}))
	defer srv.Close()
	c := New("k", srv.URL)
	_, err := c.ChatJSON(context.Background(), Request{Model: ModelQwen27B, Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 100})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
}

func TestClientDisabledWithoutKey(t *testing.T) {
	var c *Client
	if c.Enabled() || New("", "").Enabled() {
		t.Fatal("client without key must be disabled")
	}
}

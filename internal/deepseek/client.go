// Package deepseek implements a minimal DeepSeek API client
// (OpenAI-compatible chat completions) used for AI test generation.
//
// Cost model (the whole point of this client):
//   - the primary generator is deepseek-flash (DeepSeek-V4.1-Flash) IN
//     THINKING MODE — {"thinking":{"type":"enabled"}} plus the official
//     reasoning_effort knob. Chain tests (one per subject, shared by every
//     user) are generated with effort "high"; personal weak-topics tests and
//     all retries use "low". V4.1-Flash-thinking beats the retired
//     deepseek-reasoner on quality while costing ~6x less: a 20-question
//     test lands at roughly $0.002–0.006 depending on effort and the
//     off-peak pricing window;
//   - the legacy model names deepseek-chat / deepseek-reasoner were REMOVED
//     from the DeepSeek API on 2026-07-24 (every request fails with
//     "model not found") — never reintroduce them as defaults;
//   - NEVER invent extra thinking fields: a "budget_tokens" sub-field is not
//     part of the DeepSeek API and makes every call fail with HTTP 400 —
//     that bug once silently killed ALL test generation;
//   - the fallback model (deepseek-v4-pro, non-thinking) is used ONLY when
//     the flash model rejects the thinking parameters — it is several times
//     pricier, so it must stay a fallback, never the default;
//   - prompts are deliberately compact and `why` fields are disabled —
//     output tokens dominate the price, so nothing but the required JSON is
//     requested;
//   - every successful call logs its REAL token usage and the estimated
//     cost (off-peak and peak) plus the running session total, so the spend
//     per test is always visible in the server logs — no more surprise
//     $0.10 bills.
//
// The API key and models are configured through environment variables:
// DEEPSEEK_API_KEY, DEEPSEEK_MODEL, DEEPSEEK_REASONER_MODEL, DEEPSEEK_BASE_URL.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/httpx"
)

// DefaultModel is used when DEEPSEEK_MODEL is not set: the strong
// non-thinking fallback (deepseek-v4-pro). Pricier than flash — fallback
// only, never the primary generator.
const DefaultModel = "deepseek-v4-pro"

// DefaultReasonerModel is used when DEEPSEEK_REASONER_MODEL is not set:
// the primary generator, DeepSeek-V4.1-Flash in thinking mode.
const DefaultReasonerModel = "deepseek-flash"

// DefaultBaseURL is the official DeepSeek API endpoint.
const DefaultBaseURL = "https://api.deepseek.com"

// Thinking effort levels (the official reasoning_effort knob). The API maps
// every requested value onto actual low/high/max — there is no real
// "medium", so:
//   - ThinkingEffortHigh is the "medium" level: used for chain tests (one
//     per subject, shared by all users — quality matters most here and the
//     cost is amortised across everyone);
//   - ThinkingEffortLow is the cheapest real thinking: used for personal
//     weak-topics tests (a simpler, topics-only task) and for every RETRY
//     (if a harder pass blew the token budget, a shorter thinking pass is
//     what actually fits).
const (
	ThinkingEffortHigh = "high"
	ThinkingEffortLow  = "low"
)

// pricePerToken is the OFF-PEAK price per token (USD) by model; the peak
// price is exactly 2x (DeepSeek pricing, 2026). Cache-hit input is priced
// separately — DeepSeek reports it in prompt_cache_hit_tokens.
type pricePerToken struct {
	inHit  float64
	inMiss float64
	out    float64
}

var prices = map[string]pricePerToken{
	"deepseek-flash":  {inHit: 0.003e-6, inMiss: 0.15e-6, out: 0.60e-6},
	"deepseek-v4-pro": {inHit: 0.022e-6, inMiss: 0.66e-6, out: 1.98e-6},
	// Legacy names kept for cost estimates in case someone overrides the env
	// vars back to V3.x on a private proxy.
	"deepseek-chat":     {inHit: 0.07e-6, inMiss: 0.27e-6, out: 1.10e-6},
	"deepseek-reasoner": {inHit: 0.14e-6, inMiss: 0.55e-6, out: 2.19e-6},
}

type usageHookKey struct{}

// WithUsageHook returns a context whose DeepSeek calls report the token
// usage of every successful reply to fn (the generator adds it to the
// per-job statistics, which otherwise counted only the free Groq tokens).
func WithUsageHook(ctx context.Context, fn func(prompt, completion int)) context.Context {
	return context.WithValue(ctx, usageHookKey{}, fn)
}

// Client calls the DeepSeek chat completions API.
type Client struct {
	apiKey        string
	model         string
	reasonerModel string
	baseURL       string
	httpClient    *http.Client

	// thinkingUnsupported is flipped to true at runtime if the model rejects
	// the thinking parameters — further calls then omit them instead of
	// failing. Guarded by the mutex: the generation worker may call the
	// client from several goroutines.
	mu                  sync.Mutex
	thinkingUnsupported bool

	// Session cost totals (off-peak estimate) — logged on every call so the
	// spend is observable without an external dashboard.
	totalIn, totalOut int64
	totalCost         float64

	// budget enforces the daily spending cap (R-9). nil = no cap.
	budget Budget
	// offPeak tells which price applies at a moment (nil = always peak,
	// the conservative choice for the cap).
	offPeak func(time.Time) bool
}

// Budget is the daily spending cap of paid DeepSeek calls (R-9). It is fed
// by the SAME estimate the client logs for every call (estimateCost):
//   - Reserve books the WORST-CASE cost of a call before it is sent and
//     fails (wrapping ErrBudgetExceeded) when that would exceed the cap —
//     the HTTP request is then never made;
//   - Settle replaces the reservation by the estimated real cost of the
//     reply (or releases it, actual = 0, when the call failed without
//     usage data).
type Budget interface {
	Reserve(ctx context.Context, amountUSD float64) error
	Settle(ctx context.Context, reservedUSD, actualUSD float64)
}

// ErrBudgetExceeded: the daily DeepSeek spending cap is reached; the call
// was NOT sent.
var ErrBudgetExceeded = errors.New("deepseek: daily spending cap reached")

// WithBudget installs the daily spending cap. offPeak (may be nil) selects
// the off-peak/peak price of the settled cost.
func (c *Client) WithBudget(b Budget, offPeak func(time.Time) bool) *Client {
	c.budget = b
	c.offPeak = offPeak
	return c
}

// worstCaseCost bounds the price of one call before it is sent: every
// input token priced as a cache miss (input tokens bounded by the request
// size in BYTES — a BPE token is never shorter than one byte), the
// whole max_tokens budget as output, at the PEAK rate. Unknown models are
// priced like the most expensive known one.
func worstCaseCost(model string, body []byte, maxTokens int) float64 {
	p, ok := prices[model]
	if !ok {
		for _, q := range prices {
			if q.out > p.out {
				p = q
			}
		}
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	in := float64(len(body))
	return 2 * (in*p.inMiss + float64(maxTokens)*p.out)
}

// thinkingDisabled reports (under lock) whether thinking params must be
// omitted, and setThinkingUnsupported flips the switch (under lock).
func (c *Client) thinkingDisabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thinkingUnsupported
}

func (c *Client) setThinkingUnsupported() {
	c.mu.Lock()
	c.thinkingUnsupported = true
	c.mu.Unlock()
}

// addUsage accumulates session totals (under lock) and returns the running
// cost estimate for logging.
func (c *Client) addUsage(u *usage, cost float64) (total float64, in, out int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalIn += int64(u.PromptTokens)
	c.totalOut += int64(u.CompletionTokens)
	c.totalCost += cost
	return c.totalCost, c.totalIn, c.totalOut
}

// New creates a client. model / reasonerModel / baseURL may be empty —
// defaults are used.
func New(apiKey, model, reasonerModel, baseURL string) *Client {
	if model == "" {
		model = DefaultModel
	}
	if reasonerModel == "" {
		reasonerModel = DefaultReasonerModel
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:        apiKey,
		model:         model,
		reasonerModel: reasonerModel,
		baseURL:       strings.TrimRight(baseURL, "/"),
		// The HTTP timeout is only a safety net ABOVE the callers' own
		// deadlines (generation 8 min, batches/repairs 5 min, translation
		// jobs 4 min). It used to be 300 s — shorter than the 8-minute
		// generation call: a long thinking reply (max_tokens 16000) was cut
		// by the client after the server had already produced (and billed)
		// it, and the transient-error retry then paid for it again.
		httpClient: httpx.NewClient(httpSafetyTimeout),
	}
}

// Model returns the configured fallback (non-thinking) model name.
func (c *Client) Model() string { return c.model }

// ReasonerModel returns the configured primary (thinking) model name.
func (c *Client) ReasonerModel() string { return c.reasonerModel }

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"` // "system", "user", "assistant"
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// Temperature is sent only to the non-thinking fallback — thinking mode
	// ignores it (DeepSeek silently no-ops it, so we just omit it).
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens caps output tokens. In thinking mode it also covers the
	// hidden reasoning tokens, so it is a hard cost limiter.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Thinking toggles the reasoning mode: {"type":"enabled"} — the only
	// thinking field the DeepSeek API documents. Never sent to the
	// non-thinking fallback.
	Thinking *thinking `json:"thinking,omitempty"`
	// ReasoningEffort ("low"/"high"/"max") bounds the thinking length.
	// Sent only together with Thinking.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ResponseFormat enables DeepSeek JSON Output mode: the model is
	// constrained to emit valid JSON.
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type thinking struct {
	Type string `json:"type"` // "enabled"
}

type responseFormat struct {
	Type string `json:"type"` // "json_object"
}

// usage mirrors the token counters DeepSeek returns for every call. The
// cache split matters for the price: cache-hit input tokens are ~50x
// cheaper than cache-miss ones.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	PromptCacheHit   int `json:"prompt_cache_hit_tokens"`
	PromptCacheMiss  int `json:"prompt_cache_miss_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// estimateCost returns the (off-peak, peak) USD cost of a call. Peak rates
// are exactly 2x off-peak. Unknown models estimate as 0 (logged without a
// dollar figure).
func estimateCost(model string, u *usage) (offPeak, peak float64) {
	p, ok := prices[model]
	if !ok || u == nil {
		return 0, 0
	}
	hit := u.PromptCacheHit
	miss := u.PromptTokens - hit
	if hit <= 0 || miss < 0 {
		// The proxy did not split cache stats — price all input as miss
		// (the conservative, higher estimate).
		hit, miss = 0, u.PromptTokens
	}
	offPeak = float64(hit)*p.inHit + float64(miss)*p.inMiss + float64(u.CompletionTokens)*p.out
	return offPeak, offPeak * 2
}

// GenerateJSON is the single entry point used by the test generator: it asks
// the THINKING model (JSON mode) for the whole test. effort is the
// reasoning_effort knob (ThinkingEffortHigh for chain tests,
// ThinkingEffortLow for personal tests and retries). If the model rejects
// the thinking parameters (older proxy, unsupported knobs) the client
// transparently retries without them, and as a last resort falls back to
// the non-thinking model — generation never breaks.
func (c *Client) GenerateJSON(ctx context.Context, messages []Message, maxTokens int, effort string) (string, error) {
	if effort == "" {
		effort = ThinkingEffortLow
	}
	return c.jsonWithFallback(ctx, "generate", messages, maxTokens, effort, 0.7)
}

// TranslateJSON asks the model for a pure translation (JSON mode, low
// thinking effort — translation is a mechanical task, not a reasoning one).
// It is used to translate an already-generated Russian test into Kazakh ONCE
// per question; the result is cached in the database and reused by every
// user, so this entry point is expected to be called rarely.
func (c *Client) TranslateJSON(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	return c.jsonWithFallback(ctx, "translate", messages, maxTokens, ThinkingEffortLow, 0.3)
}

// Reliability knobs of the paid fallback. DeepSeek is the LAST provider in
// every route (Groq runs first), so a DeepSeek failure fails the whole
// task — it must survive transient trouble on its own:
//
//   - transient errors (HTTP 429 / 408 / 5xx, transport errors, client
//     timeouts, empty replies) are retried with backoff (retryBackoff);
//   - a reply truncated at max_tokens (finish_reason=length — typically the
//     hidden thinking ate the whole budget, 0 visible chars) is retried ONCE
//     without thinking, with the non-thinking output ceiling.
//
// The daily cap is untouched: every retry is a separate call that reserves
// and settles its own cost against the same Budget.
var retryBackoff = []time.Duration{2 * time.Second, 6 * time.Second, 15 * time.Second}

// maxRetryAfter caps a server-requested Retry-After wait.
const maxRetryAfter = 30 * time.Second

// minAttemptTime: a retry is not started when less than this is left until
// the context deadline (it could not finish anyway).
const minAttemptTime = 20 * time.Second

// NonThinkingMaxTokens is the output ceiling of a call WITHOUT thinking
// (the non-thinking DeepSeek endpoints reject larger max_tokens). Every
// non-thinking call is clamped to it; it is also the budget of the
// truncation retry (no hidden reasoning — the whole budget is visible JSON,
// enough for a full 20-question test).
const NonThinkingMaxTokens = 8192

// httpSafetyTimeout bounds one HTTP exchange when the caller's context has
// no (shorter) deadline.
const httpSafetyTimeout = 10 * time.Minute

// ErrTruncated: the reply hit max_tokens (finish_reason=length).
var ErrTruncated = errors.New("deepseek: reply truncated")

// transientError marks a failure that a plain resend may fix.
type transientError struct {
	err        error
	retryAfter time.Duration
}

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func transient(err error) error { return &transientError{err: err} }

// IsTransient reports whether err is a transient DeepSeek failure (rate
// limit, server error, network trouble, timeout, empty reply).
func IsTransient(err error) bool {
	var t *transientError
	return errors.As(err, &t)
}

// callRetry is call with in-task retries of transient failures (backoff,
// honouring Retry-After). A cancelled/expired ctx, the daily cap and
// non-transient errors (400, truncation) are returned at once.
func (c *Client) callRetry(ctx context.Context, model string, messages []Message, temperature float64, maxTokens int, jsonMode bool, effort string) (string, error) {
	for attempt := 0; ; attempt++ {
		raw, err := c.call(ctx, model, messages, temperature, maxTokens, jsonMode, effort)
		if err == nil || !IsTransient(err) || ctx.Err() != nil || attempt >= len(retryBackoff) {
			return raw, err
		}
		wait := retryBackoff[attempt]
		var t *transientError
		if errors.As(err, &t) && t.retryAfter > wait {
			wait = min(t.retryAfter, maxRetryAfter)
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait+minAttemptTime {
			return raw, err
		}
		log.Printf("deepseek: %s transient error (attempt %d/%d): %v — retrying in %s",
			model, attempt+1, len(retryBackoff)+1, err, wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return raw, err
		case <-timer.C:
		}
	}
}

// jsonWithFallback runs the provider route shared by GenerateJSON and
// TranslateJSON (every call below is callRetry — transient errors are
// retried with backoff inside):
//
//  1. thinking mode on the reasoner model — skipped entirely once the model
//     has explicitly rejected the thinking parameters (no wasted 400 call);
//  2. the same reasoner model WITHOUT thinking parameters — only after an
//     explicit thinking-parameter rejection (the flag is set only then);
//  3. the pricier non-thinking fallback (c.model) — ONLY when the reasoner
//     rejected the request parameters. Timeouts, 5xx and context-length
//     errors never route to it (no silent ~3x spend);
//  4. a reply truncated at max_tokens is retried ONCE on the same model
//     WITHOUT thinking with NonThinkingMaxTokens — all of that budget goes
//     to the visible JSON, so the retry practically never truncates.
func (c *Client) jsonWithFallback(ctx context.Context, op string, messages []Message, maxTokens int, effort string, fallbackTemp float64) (string, error) {
	model, temp := c.reasonerModel, 0.0
	eff := effort
	if c.thinkingDisabled() {
		eff = ""
	}
	raw, err := c.callRetry(ctx, model, messages, temp, maxTokens, true, eff)
	if eff != "" && err != nil && !IsTransient(err) && isThinkingParamError(err) {
		log.Printf("deepseek: thinking params rejected on %s (%v) — retrying without them", op, err)
		c.setThinkingUnsupported()
		eff = ""
		raw, err = c.callRetry(ctx, model, messages, temp, maxTokens, true, "")
	}
	if eff == "" && err != nil && !IsTransient(err) && isParamError(err) {
		// The reasoner rejects the request itself — last resort: the
		// non-thinking fallback model (deepseek-v4-pro — strong, but
		// pricier; it must never become the default).
		log.Printf("deepseek: %s rejected %s request (%v) — falling back to %s (non-thinking)", c.reasonerModel, op, err, c.model)
		model, temp = c.model, fallbackTemp
		raw, err = c.callRetry(ctx, model, messages, temp, maxTokens, true, "")
	}
	if err != nil && errors.Is(err, ErrTruncated) && ctx.Err() == nil {
		log.Printf("deepseek: %s %s reply truncated (%v) — retrying once without thinking, max_tokens=%d", model, op, err, NonThinkingMaxTokens)
		raw, err = c.callRetry(ctx, model, messages, temp, NonThinkingMaxTokens, true, "")
		if err != nil {
			err = fmt.Errorf("after truncation retry: %w", err)
		}
	}
	return raw, err
}

// isThinkingParamError reports an explicit rejection of the thinking-mode
// knobs (the error text names "thinking" or "reasoning_effort"). Any other
// 400 (too long context, bad JSON, …) must NOT disable thinking forever.
func isThinkingParamError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "thinking") || strings.Contains(s, "reasoning_effort")
}

// isParamError reports whether the API rejected request parameters (HTTP 400
// class) rather than failing for a transient reason. Context-length errors
// are excluded — a different model would not help with them.
func isParamError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	ls := strings.ToLower(s)
	if strings.Contains(ls, "context length") || strings.Contains(ls, "context_length") ||
		strings.Contains(ls, "maximum context") || strings.Contains(ls, "too long") {
		return false
	}
	return strings.Contains(s, "HTTP 400") ||
		strings.Contains(s, "invalid_request") ||
		strings.Contains(s, "unknown field") ||
		strings.Contains(s, "unsupported")
}

func (c *Client) call(ctx context.Context, model string, messages []Message, temperature float64, maxTokens int, jsonMode bool, effort string) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("deepseek: API key is not configured (set DEEPSEEK_API_KEY)")
	}
	reqBody := chatRequest{Model: model, Messages: messages}
	if temperature > 0 {
		reqBody.Temperature = &temperature
	}
	if effort == "" && maxTokens > NonThinkingMaxTokens {
		// Without thinking the endpoint caps the output lower — a larger
		// max_tokens would be rejected with HTTP 400.
		maxTokens = NonThinkingMaxTokens
	}
	if maxTokens > 0 {
		reqBody.MaxTokens = maxTokens
	}
	if jsonMode {
		reqBody.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	if effort != "" {
		// Official thinking-mode knobs only (api-docs.deepseek.com/guides/
		// thinking_mode): enable thinking and bound it with
		// reasoning_effort — that is what keeps the hidden reasoning tokens
		// (billed as output) cheap. No "budget_tokens": DeepSeek rejects
		// unknown fields with HTTP 400.
		reqBody.Thinking = &thinking{Type: "enabled"}
		reqBody.ReasoningEffort = effort
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	// R-9: reserve the worst-case cost against the daily cap BEFORE the
	// request — a capped call is never sent.
	reserved := 0.0
	settled := false
	mayBeBilled := false
	if c.budget != nil {
		reserved = worstCaseCost(model, body, maxTokens)
		if err := c.budget.Reserve(ctx, reserved); err != nil {
			if errors.Is(err, ErrBudgetExceeded) {
				return "", err
			}
			// Ledger unavailable: fail CLOSED — no uncontrolled paid call.
			return "", fmt.Errorf("%w (ledger error: %v)", ErrBudgetExceeded, err)
		}
		defer func() {
			if settled {
				return
			}
			if mayBeBilled {
				// The request reached DeepSeek but the reply was lost (cut
				// by a deadline, connection dropped mid-reply): the server
				// may well have produced — and billed — the completion.
				// Keep the worst case booked so the daily cap stays honest.
				c.budget.Settle(context.WithoutCancel(ctx), reserved, reserved)
				return
			}
			// No usage data (connection never made, API error): DeepSeek
			// does not bill a request that produced no completion — release.
			c.budget.Settle(context.WithoutCancel(ctx), reserved, 0)
		}()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		mayBeBilled = !isDialError(err)
		if ctx.Err() != nil {
			return "", err // the caller's deadline/cancel — not retryable
		}
		// Transport failure / client timeout: a resend may well succeed.
		return "", transient(fmt.Errorf("deepseek: request: %w", err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		mayBeBilled = resp.StatusCode == http.StatusOK
		if ctx.Err() != nil {
			return "", err
		}
		return "", transient(fmt.Errorf("deepseek: read response (HTTP %d): %w", resp.StatusCode, err))
	}

	// A 200 is billed even when its body turns out to be unusable (broken
	// JSON, no usage block): keep the reservation unless usage settles it.
	mayBeBilled = resp.StatusCode == http.StatusOK
	var cr chatResponse
	decodeErr := json.Unmarshal(raw, &cr)
	if resp.StatusCode != http.StatusOK {
		// Bounded, valid UTF-8 excerpt: the body may be large, and the
		// error ends up in logs and in generation_jobs.last_error.
		msg := strings.ToValidUTF8(string(raw), "\uFFFD")
		if decodeErr == nil && cr.Error != nil {
			msg = fmt.Sprintf("%s (%s)", cr.Error.Message, cr.Error.Type)
		}
		if r := []rune(msg); len(r) > 500 {
			msg = string(r[:500]) + "…"
		}
		err := fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, msg)
		if isTransientStatus(resp.StatusCode) {
			return "", &transientError{err: err, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
		}
		return "", err
	}
	if decodeErr != nil {
		// A 200 with a broken body (proxy hiccup, cut stream) — resend.
		return "", transient(fmt.Errorf("deepseek: decode response (HTTP %d): %w", resp.StatusCode, decodeErr))
	}
	if cr.Error != nil {
		return "", fmt.Errorf("deepseek: %s (%s)", cr.Error.Message, cr.Error.Type)
	}
	if len(cr.Choices) == 0 {
		return "", transient(fmt.Errorf("deepseek: empty choices"))
	}

	// Cost observability: log the real token usage of every successful call
	// with an estimated price, plus the running session total. This is the
	// early-warning system against runaway spend.
	if cr.Usage != nil {
		if fn, ok := ctx.Value(usageHookKey{}).(func(prompt, completion int)); ok && fn != nil {
			fn(cr.Usage.PromptTokens, cr.Usage.CompletionTokens)
		}
		off, peak := estimateCost(model, cr.Usage)
		if c.budget != nil {
			actual := peak
			if c.offPeak != nil && c.offPeak(time.Now()) {
				actual = off
			}
			if _, known := prices[model]; !known {
				actual = reserved // unknown price: keep the worst case booked
			}
			c.budget.Settle(context.WithoutCancel(ctx), reserved, actual)
			settled = true
		}
		total, totIn, totOut := c.addUsage(cr.Usage, off)
		if off > 0 {
			log.Printf("deepseek: %s effort=%q in=%d (cache hit %d) out=%d => ~$%.4f off-peak / $%.4f peak | session ~$%.4f (%d in / %d out)",
				model, effort, cr.Usage.PromptTokens, cr.Usage.PromptCacheHit, cr.Usage.CompletionTokens,
				off, peak, total, totIn, totOut)
		} else {
			log.Printf("deepseek: %s effort=%q in=%d out=%d (price unknown for this model)",
				model, effort, cr.Usage.PromptTokens, cr.Usage.CompletionTokens)
		}
	}

	content := cr.Choices[0].Message.Content
	// "length" means the model spent the whole max_tokens budget (mostly on
	// hidden thinking) and the visible JSON got truncated — such a reply can
	// never parse, so fail fast and let the job retry (at a lower thinking
	// effort) instead of storing garbage or looping on a JSON syntax error.
	if cr.Choices[0].FinishReason == "length" {
		return "", fmt.Errorf("%w at max_tokens=%d (finish_reason=length, %d visible chars)", ErrTruncated, maxTokens, len(content))
	}
	if strings.TrimSpace(content) == "" {
		return "", transient(fmt.Errorf("deepseek: empty content (finish_reason=%q)", cr.Choices[0].FinishReason))
	}
	return content, nil
}

// isDialError reports a failure to even open the connection (DNS, refused,
// TLS handshake): the request never reached the API, nothing was billed.
func isDialError(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// isTransientStatus: rate limit, request timeout and every server error.
func isTransientStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
}

// parseRetryAfter reads a Retry-After header given in seconds (0 if absent
// or in the HTTP-date form, which DeepSeek does not use).
func parseRetryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// Package deepseek implements a minimal DeepSeek API client
// (OpenAI-compatible chat completions).
//
// The bot uses exactly ONE DeepSeek model — deepseek-flash
// (DeepSeek-V4.1-Flash) — in two modes:
//
//   - GenerateJSON: THINKING mode with reasoning_effort="high"
//     ({"thinking":{"type":"enabled"},"reasoning_effort":"high"}). Every
//     test generation goes through it: chain tests, personal weak-topics
//     tests, retries, batches, topic-bank batches and the repair of flagged
//     questions. There is no lower effort and no other model: if the API
//     rejects the request, the call simply fails and the job is retried
//     later (no silent switch to a pricier or weaker model);
//   - TranslateJSON: NON-thinking mode ({"thinking":{"type":"disabled"}}) —
//     the cheapest call possible. Used only as the fallback of the Kazakh
//     translation when the primary translator (Groq Qwen) is out of quota
//     or failed.
//
// Cost notes:
//   - the legacy model names deepseek-chat / deepseek-reasoner were REMOVED
//     from the DeepSeek API on 2026-07-24 — never reintroduce them;
//   - NEVER invent extra thinking fields: a "budget_tokens" sub-field is not
//     part of the DeepSeek API and makes every call fail with HTTP 400;
//   - max_tokens is the hard cost limiter (in thinking mode it also covers
//     the hidden reasoning tokens);
//   - every successful call logs its REAL token usage and the estimated
//     cost (off-peak and peak) plus the running session total, and feeds the
//     daily spending cap (Budget). Thinking and non-thinking calls of the
//     same model are billed at the same per-token price — non-thinking is
//     cheaper only because it produces far fewer output tokens.
//
// Configuration: DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL (optional).
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/httpx"
)

// Model is the only DeepSeek model the bot calls (DeepSeek-V4.1-Flash).
const Model = "deepseek-flash"

// DefaultBaseURL is the official DeepSeek API endpoint.
const DefaultBaseURL = "https://api.deepseek.com"

// GenerationEffort is the reasoning_effort of every generation call.
const GenerationEffort = "high"

// pricePerToken is the OFF-PEAK price per token (USD) by model; the peak
// price is exactly 2x (DeepSeek pricing, 2026). Cache-hit input is priced
// separately — DeepSeek reports it in prompt_cache_hit_tokens.
type pricePerToken struct {
	inHit  float64
	inMiss float64
	out    float64
}

// The price is per model, not per mode: thinking and non-thinking calls of
// deepseek-flash cost the same per token (reasoning tokens are billed as
// output).
var prices = map[string]pricePerToken{
	Model: {inHit: 0.003e-6, inMiss: 0.15e-6, out: 0.60e-6},
}

type usageHookKey struct{}

// WithUsageHook returns a context whose DeepSeek calls report the token
// usage of every successful reply to fn (the generator adds it to the
// per-job statistics).
func WithUsageHook(ctx context.Context, fn func(prompt, completion int)) context.Context {
	return context.WithValue(ctx, usageHookKey{}, fn)
}

// Client calls the DeepSeek chat completions API.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client

	// mu guards the session totals: the workers call the client from
	// several goroutines.
	mu sync.Mutex

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
// whole max_tokens budget as output, at the PEAK rate. An unknown model is
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

// New creates a client. baseURL may be empty (official endpoint).
func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpx.NewClient(300 * time.Second),
	}
}

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"` // "system", "user", "assistant"
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// Temperature is sent only in non-thinking mode (translation) —
	// thinking mode ignores it, so it is omitted there.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens caps output tokens. In thinking mode it also covers the
	// hidden reasoning tokens, so it is a hard cost limiter.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Thinking toggles the reasoning mode: {"type":"enabled"} (generation)
	// or {"type":"disabled"} (translation fallback). It is ALWAYS sent
	// explicitly, so the mode never depends on the API default.
	Thinking *thinking `json:"thinking,omitempty"`
	// ReasoningEffort bounds the thinking length. Sent only in thinking
	// mode (always GenerationEffort).
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ResponseFormat enables DeepSeek JSON Output mode: the model is
	// constrained to emit valid JSON.
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type thinking struct {
	Type string `json:"type"` // "enabled" | "disabled"
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

// GenerateJSON asks deepseek-flash in THINKING mode (reasoning_effort
// "high", JSON mode) for a generation reply. It is the only generation
// entry point: chain tests, personal tests, retries, batches and repairs
// all use it. Errors are returned as is — the caller retries later.
func (c *Client) GenerateJSON(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	return c.call(ctx, messages, maxTokens, true)
}

// TranslateJSON asks deepseek-flash in NON-thinking mode (JSON mode,
// temperature 0.3) — the cheapest possible call. It is the fallback of the
// Kazakh translation only (the primary translator is Groq Qwen).
func (c *Client) TranslateJSON(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	return c.call(ctx, messages, maxTokens, false)
}

// mode returns the log label of a call mode.
func mode(think bool) string {
	if think {
		return "thinking(" + GenerationEffort + ")"
	}
	return "non-thinking"
}

func (c *Client) call(ctx context.Context, messages []Message, maxTokens int, think bool) (string, error) {
	if c.apiKey == "" {
		return "", fmt.Errorf("deepseek: API key is not configured (set DEEPSEEK_API_KEY)")
	}
	model := Model
	reqBody := chatRequest{
		Model:          model,
		Messages:       messages,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}
	if maxTokens > 0 {
		reqBody.MaxTokens = maxTokens
	}
	if think {
		// Official thinking-mode knobs only (api-docs.deepseek.com/guides/
		// thinking_mode). No "budget_tokens": DeepSeek rejects unknown
		// fields with HTTP 400.
		reqBody.Thinking = &thinking{Type: "enabled"}
		reqBody.ReasoningEffort = GenerationEffort
	} else {
		reqBody.Thinking = &thinking{Type: "disabled"}
		temp := 0.3
		reqBody.Temperature = &temp
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	// R-9: reserve the worst-case cost against the daily cap BEFORE the
	// request — a capped call is never sent.
	reserved := 0.0
	settled := false
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
			if !settled {
				// No usage data (transport error, API error): DeepSeek does
				// not bill a request that produced no completion — release.
				c.budget.Settle(context.WithoutCancel(ctx), reserved, 0)
			}
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
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("deepseek: decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if cr.Error != nil {
		return "", fmt.Errorf("deepseek: %s (%s)", cr.Error.Message, cr.Error.Type)
	}
	if resp.StatusCode != http.StatusOK {
		// Bounded, valid UTF-8 excerpt: the body may be large, and the
		// error ends up in logs and in generation_jobs.last_error.
		body := strings.ToValidUTF8(string(raw), "\uFFFD")
		if r := []rune(body); len(r) > 500 {
			body = string(r[:500]) + "…"
		}
		return "", fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, body)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("deepseek: empty choices")
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
			log.Printf("deepseek: %s %s in=%d (cache hit %d) out=%d => ~$%.4f off-peak / $%.4f peak | session ~$%.4f (%d in / %d out)",
				model, mode(think), cr.Usage.PromptTokens, cr.Usage.PromptCacheHit, cr.Usage.CompletionTokens,
				off, peak, total, totIn, totOut)
		} else {
			log.Printf("deepseek: %s %s in=%d out=%d (price unknown for this model)",
				model, mode(think), cr.Usage.PromptTokens, cr.Usage.CompletionTokens)
		}
	}

	content := cr.Choices[0].Message.Content
	// "length" means the model spent the whole max_tokens budget (mostly on
	// hidden thinking) and the visible JSON got truncated — such a reply can
	// never parse, so fail fast and let the job retry instead of storing
	// garbage or looping on a JSON syntax error.
	if cr.Choices[0].FinishReason == "length" {
		return "", fmt.Errorf("deepseek: reply truncated at max_tokens=%d (finish_reason=length, %d visible chars)", maxTokens, len(content))
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("deepseek: empty content (finish_reason=%q)", cr.Choices[0].FinishReason)
	}
	return content, nil
}

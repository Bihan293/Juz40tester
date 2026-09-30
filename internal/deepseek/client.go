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
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
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
		httpClient:    &http.Client{Timeout: 300 * time.Second},
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
	raw, err := c.call(ctx, c.reasonerModel, messages, 0, maxTokens, true, effort)
	if err != nil && !c.thinkingDisabled() && isParamError(err) {
		log.Printf("deepseek: thinking params rejected (%v) — retrying without them", err)
		c.setThinkingUnsupported()
		raw, err = c.call(ctx, c.reasonerModel, messages, 0, maxTokens, true, "")
	}
	if err != nil && c.thinkingDisabled() {
		// Thinking mode unusable at all — last resort: the non-thinking
		// fallback model (deepseek-v4-pro — strong, but pricier; it must
		// never become the default).
		log.Printf("deepseek: %s failed (%v) — falling back to %s (non-thinking)", c.reasonerModel, err, c.model)
		raw, err = c.call(ctx, c.model, messages, 0.7, maxTokens, true, "")
	}
	return raw, err
}

// TranslateJSON asks the model for a pure translation (JSON mode, low
// thinking effort — translation is a mechanical task, not a reasoning one).
// It is used to translate an already-generated Russian test into Kazakh ONCE
// per question; the result is cached in the database and reused by every
// user, so this entry point is expected to be called rarely.
func (c *Client) TranslateJSON(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	raw, err := c.call(ctx, c.reasonerModel, messages, 0, maxTokens, true, ThinkingEffortLow)
	if err != nil && !c.thinkingDisabled() && isParamError(err) {
		log.Printf("deepseek: thinking params rejected on translate (%v) — retrying without them", err)
		c.setThinkingUnsupported()
		raw, err = c.call(ctx, c.reasonerModel, messages, 0, maxTokens, true, "")
	}
	if err != nil && c.thinkingDisabled() {
		log.Printf("deepseek: %s failed on translate (%v) — falling back to %s (non-thinking)", c.reasonerModel, err, c.model)
		raw, err = c.call(ctx, c.model, messages, 0.3, maxTokens, true, "")
	}
	return raw, err
}

// isParamError reports whether the API rejected request parameters (HTTP 400
// class) rather than failing for a transient reason.
func isParamError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
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
		return "", fmt.Errorf("deepseek: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("deepseek: empty choices")
	}

	// Cost observability: log the real token usage of every successful call
	// with an estimated price, plus the running session total. This is the
	// early-warning system against runaway spend.
	if cr.Usage != nil {
		off, peak := estimateCost(model, cr.Usage)
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
		return "", fmt.Errorf("deepseek: reply truncated at max_tokens=%d (finish_reason=length, %d visible chars)", maxTokens, len(content))
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("deepseek: empty content (finish_reason=%q)", cr.Choices[0].FinishReason)
	}
	return content, nil
}

package groq

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
)

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Reasoning effort values. GPT-OSS supports low/medium/high; Qwen 3.8
// additionally supports "none" (instruct mode — no reasoning tokens at all).
const (
	EffortNone   = "none"
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
)

// Request is one JSON-mode chat completion.
type Request struct {
	Model    string
	Messages []Message
	// MaxTokens is the desired output cap (reasoning tokens included). It is
	// automatically SHRUNK so that prompt + max_tokens fits the per-request
	// TPM ceiling (8000 on the free tier) — see Limits.MaxRequestTokens.
	MaxTokens int
	// MinTokens: if the shrunk max_tokens would fall below this, the request
	// is not sent at all (ErrTooLarge) — the caller must split the work.
	MinTokens   int
	Effort      string  // reasoning_effort; "" = model default
	Temperature float64 // 0 = model default
	TopP        float64 // 0 = model default
	// Schema, when set, enables Structured Outputs (json_schema, strict) —
	// constrained decoding guarantees the reply matches the schema. If the
	// API rejects the schema the client transparently falls back to plain
	// json_object mode (still validated locally by the caller).
	Schema     map[string]any
	SchemaName string
	// MaxWait bounds how long the call may wait for the local rate limiter.
	// Waiting longer than that returns *RateLimitError immediately so the
	// caller can try the next provider instead of stalling the user.
	MaxWait time.Duration
}

// Result is a successful completion.
type Result struct {
	Content          string
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
	Model            string
}

// ErrTooLarge means the request cannot fit into ONE free-tier request
// (prompt + MinTokens > TPM). Waiting does not help — split the input.
var ErrTooLarge = errors.New("groq: request exceeds the per-request token budget")

// ErrTruncated means the model spent max_tokens before finishing the JSON.
var ErrTruncated = errors.New("groq: reply truncated (finish_reason=length)")

// APIError is a non-rate-limit HTTP error from Groq.
type APIError struct {
	Status  int
	Message string
	Type    string
	Code    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("groq: HTTP %d: %s (%s/%s)", e.Status, e.Message, e.Type, e.Code)
}

// Client is a Groq API client with a local per-model rate limiter.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client

	mu               sync.Mutex
	limiters         map[string]*limiter
	schemaRejected   map[string]bool // model -> json_schema unsupported, use json_object
	effortRejected   map[string]bool // model -> reasoning_effort value rejected, omit it
	requests, tokens int64           // session totals (logging)
}

// New creates a client. baseURL may be empty (official endpoint).
func New(apiKey, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		apiKey:         apiKey,
		baseURL:        strings.TrimRight(baseURL, "/"),
		httpClient:     &http.Client{Timeout: 120 * time.Second},
		limiters:       map[string]*limiter{},
		schemaRejected: map[string]bool{},
		effortRejected: map[string]bool{},
	}
}

// Enabled reports whether the client has an API key.
func (c *Client) Enabled() bool { return c != nil && c.apiKey != "" }

func (c *Client) limiterFor(model string) *limiter {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limiters[model]
	if !ok {
		l = newLimiter(model, LimitsFor(model))
		c.limiters[model] = l
	}
	return l
}

func (c *Client) schemaDisabled(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.schemaRejected[model]
}

func (c *Client) effortDisabled(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effortRejected[model]
}

func (c *Client) disableEffort(model string) {
	c.mu.Lock()
	c.effortRejected[model] = true
	c.mu.Unlock()
}

func (c *Client) disableSchema(model string) {
	c.mu.Lock()
	c.schemaRejected[model] = true
	c.mu.Unlock()
}

// Budget returns the max output tokens a request with these messages may
// ask for without exceeding the per-request ceiling of the model.
func Budget(model string, messages []Message) int {
	return LimitsFor(model).MaxRequestTokens() - EstimateTokens(messages)
}

// FitMaxTokens clamps want into the per-request budget and the model's
// max completion size. Returns ErrTooLarge when the result is below min.
func FitMaxTokens(model string, messages []Message, want, min int) (int, error) {
	l := LimitsFor(model)
	budget := Budget(model, messages)
	n := want
	if n <= 0 || n > budget {
		n = budget
	}
	if l.MaxCompletion > 0 && n > l.MaxCompletion {
		n = l.MaxCompletion
	}
	if n < min || n < 64 {
		return 0, fmt.Errorf("%w: prompt ~%d tokens, only %d left for the reply (need %d, TPM %d)",
			ErrTooLarge, EstimateTokens(messages), n, min, l.TPM)
	}
	return n, nil
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type chatRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	MaxTokens        int             `json:"max_completion_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
	IncludeReasoning *bool           `json:"include_reasoning,omitempty"`
	ResponseFormat   *responseFormat `json:"response_format,omitempty"`
	Stream           bool            `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		TotalTokens             int `json:"total_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// ChatJSON runs one JSON-mode completion within the model's quota.
func (c *Client) ChatJSON(ctx context.Context, r Request) (*Result, error) {
	if !c.Enabled() {
		return nil, errors.New("groq: API key is not configured (set GROQ_API_KEY)")
	}
	maxTokens, err := FitMaxTokens(r.Model, r.Messages, r.MaxTokens, r.MinTokens)
	if err != nil {
		return nil, err
	}
	if c.effortDisabled(r.Model) {
		r.Effort = ""
	}
	useSchema := r.Schema != nil && !c.schemaDisabled(r.Model)
	res, err := c.do(ctx, r, maxTokens, useSchema)
	if err != nil && r.Effort != "" && isParamError(err, "reasoning") {
		log.Printf("groq: %s rejected reasoning_effort=%q (%v) — omitting it from now on", r.Model, r.Effort, err)
		c.disableEffort(r.Model)
		r.Effort = ""
		res, err = c.do(ctx, r, maxTokens, useSchema)
	}
	if err != nil && useSchema && isSchemaError(err) {
		log.Printf("groq: %s rejected json_schema (%v) — falling back to json_object mode", r.Model, err)
		c.disableSchema(r.Model)
		res, err = c.do(ctx, r, maxTokens, false)
	}
	return res, err
}

// isParamError reports an HTTP 400 whose message mentions the given word.
func isParamError(err error, word string) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ae.Message+" "+ae.Code), word)
}

func isSchemaError(err error) bool {
	return isParamError(err, "schema") || isParamError(err, "response_format") || isParamError(err, "json")
}

func (c *Client) do(ctx context.Context, r Request, maxTokens int, useSchema bool) (*Result, error) {
	body := chatRequest{Model: r.Model, Messages: r.Messages, MaxTokens: maxTokens}
	if r.Temperature > 0 {
		t := r.Temperature
		body.Temperature = &t
	}
	if r.TopP > 0 {
		p := r.TopP
		body.TopP = &p
	}
	if r.Effort != "" {
		body.ReasoningEffort = r.Effort
	}
	// GPT-OSS: never ship the reasoning text back — it is not needed and
	// only bloats the response (reasoning tokens are billed either way).
	// Qwen: reasoning_format/include_reasoning are left at the JSON-mode
	// default (parsed) — with effort "none" there is no reasoning anyway.
	if strings.HasPrefix(r.Model, "openai/gpt-oss") {
		f := false
		body.IncludeReasoning = &f
	}
	if useSchema {
		name := r.SchemaName
		if name == "" {
			name = "result"
		}
		body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: &jsonSchema{Name: name, Strict: true, Schema: r.Schema}}
	} else {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	// Reserve quota exactly as Groq checks it: prompt + max_tokens.
	lim := c.limiterFor(r.Model)
	reserved := EstimateTokens(r.Messages) + maxTokens
	resv, err := lim.acquire(ctx, reserved, r.MaxWait)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		resv.commit(0)
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	started := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Unknown whether Groq counted it — keep the reservation (safe side).
		return nil, fmt.Errorf("groq: %s: %w", r.Model, err)
	}
	defer resp.Body.Close()
	lim.observeHeaders(resp.Header)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("groq: %s: read body: %w", r.Model, err)
	}

	var cr chatResponse
	_ = json.Unmarshal(raw, &cr)

	if resp.StatusCode == http.StatusTooManyRequests {
		wait := parseRetryAfter(resp.Header.Get("retry-after"))
		msg := ""
		if cr.Error != nil {
			msg = cr.Error.Message
		}
		reason := "429"
		if strings.Contains(strings.ToLower(msg), "per day") {
			reason = "429 per day"
			if wait < time.Minute {
				wait = time.Hour // daily quota: stop hammering, re-probe hourly
			}
		}
		if wait <= 0 {
			wait = 15 * time.Second
		}
		lim.block(wait, reason)
		log.Printf("groq: %s HTTP 429 (%s) — model blocked for %s", r.Model, msg, wait.Round(time.Second))
		return nil, &RateLimitError{Model: r.Model, Reason: reason + ": " + msg, RetryAfter: wait}
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		resv.commit(0) // rejected before inference — does not consume quota
		msg := string(raw)
		if cr.Error != nil {
			msg = cr.Error.Message
		}
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, msg)
	}
	if resp.StatusCode != http.StatusOK || cr.Error != nil {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 422 {
			resv.commit(0) // rejected request — no tokens consumed
		}
		ae := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		if cr.Error != nil {
			ae.Message, ae.Type, ae.Code = cr.Error.Message, cr.Error.Type, cr.Error.Code
		}
		if len(ae.Message) > 500 {
			ae.Message = ae.Message[:500]
		}
		return nil, ae
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("groq: %s: empty choices", r.Model)
	}

	res := &Result{Content: cr.Choices[0].Message.Content, Model: r.Model}
	if cr.Usage != nil {
		res.PromptTokens = cr.Usage.PromptTokens
		res.CompletionTokens = cr.Usage.CompletionTokens
		if cr.Usage.CompletionTokensDetails != nil {
			res.ReasoningTokens = cr.Usage.CompletionTokensDetails.ReasoningTokens
		}
		resv.commit(cr.Usage.PromptTokens + cr.Usage.CompletionTokens)
	}
	c.mu.Lock()
	c.requests++
	c.tokens += int64(res.PromptTokens + res.CompletionTokens)
	totReq, totTok := c.requests, c.tokens
	c.mu.Unlock()
	rm, tm, rd, td := lim.snapshot()
	soft := lim.lim
	log.Printf("groq: %s effort=%q in=%d out=%d (reasoning %d) max=%d %.1fs | quota min %d/%d req %d/%d tok, day %d/%d req %d/%d tok | session %d req %d tok ($0 — free tier)",
		r.Model, r.Effort, res.PromptTokens, res.CompletionTokens, res.ReasoningTokens, maxTokens,
		time.Since(started).Seconds(), rm, soft.RPM, tm, soft.TPM, rd, soft.RPD, td, soft.TPD, totReq, totTok)

	if cr.Choices[0].FinishReason == "length" {
		return nil, fmt.Errorf("%w at max_completion_tokens=%d (%s)", ErrTruncated, maxTokens, r.Model)
	}
	if strings.TrimSpace(res.Content) == "" {
		return nil, fmt.Errorf("groq: %s: empty content (finish_reason=%q)", r.Model, cr.Choices[0].FinishReason)
	}
	return res, nil
}

// IsRateLimited reports whether err is a (local or remote) quota error.
func IsRateLimited(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl)
}

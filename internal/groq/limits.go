// Package groq implements a rate-limit-aware client for the Groq API
// (OpenAI-compatible chat completions). The bot uses it for ONE task only:
// the RU→KK translation of tests with qwen/qwen3.8-27b in instruct mode
// (reasoning_effort=none). Test generation never touches Groq.
//
// When the free quota is exhausted or a call fails, the translator falls
// back to DeepSeek flash in non-thinking mode (internal/deepseek).
//
// The free-tier limits are HARD and per model (see docs/AI_PROVIDERS.md):
// the client never sends a request that is known to exceed them — it waits
// (bounded by the caller's MaxWait) or returns a *RateLimitError so the
// caller can move on to the next provider immediately.
package groq

import (
	"os"
	"strconv"
	"strings"
)

// ModelQwen27B is the translation model (official Groq id, 2026-09).
const ModelQwen27B = "qwen/qwen3.8-27b"

// DefaultBaseURL is the official Groq OpenAI-compatible endpoint.
const DefaultBaseURL = "https://api.groq.com/openai/v1"

// Limits describes the quota of ONE model for the whole organization.
// Source: https://console.groq.com/docs/rate-limits (free tier) and
// https://console.groq.com/docs/models (context / max completion).
type Limits struct {
	RPM int // requests per minute
	RPD int // requests per day
	TPM int // tokens per minute (prompt + completion) — ALSO the max size of ONE request (HTTP 413 above it)
	TPD int // tokens per day

	ContextWindow int // max prompt + completion tokens of a request
	MaxCompletion int // max output tokens of a request (includes reasoning tokens)
}

// freeTierLimits are the Groq FREE plan limits as published on
// console.groq.com/docs/rate-limits (verified 2026-09-30).
var freeTierLimits = map[string]Limits{
	ModelQwen27B: {RPM: 30, RPD: 1000, TPM: 8000, TPD: 200000, ContextWindow: 131072, MaxCompletion: 16384},
}

// conservativeDefault is used for an unknown model id (e.g. a model swapped
// in via env): the smallest free-tier bucket seen on Groq chat models.
var conservativeDefault = Limits{RPM: 30, RPD: 1000, TPM: 6000, TPD: 100000, ContextWindow: 32768, MaxCompletion: 8192}

// Safety margins. The bot must NEVER hit the real ceiling (a 429 wastes a
// request and forces the paid DeepSeek translation fallback), so the soft limits used by
// the local limiter are a bit lower than the published ones.
const (
	// safetyFactor is applied to RPM / RPD / TPD (window counters).
	safetyFactor = 0.9
	// requestTokenMargin is kept free below TPM for ONE request: our token
	// estimate is heuristic (no tokenizer), Groq counts prompt + max_tokens.
	requestTokenMargin = 400
)

// LimitsFor returns the published limits of a model, with optional env
// overrides applied (GROQ_RPM, GROQ_RPD, GROQ_TPM, GROQ_TPD — they apply to
// every Groq model; use them after upgrading to the Developer plan).
func LimitsFor(model string) Limits {
	l, ok := freeTierLimits[model]
	if !ok {
		l = conservativeDefault
	}
	if v, ok := envInt("GROQ_RPM"); ok {
		l.RPM = v
	}
	if v, ok := envInt("GROQ_RPD"); ok {
		l.RPD = v
	}
	if v, ok := envInt("GROQ_TPM"); ok {
		l.TPM = v
	}
	if v, ok := envInt("GROQ_TPD"); ok {
		l.TPD = v
	}
	return l
}

// soft returns the limits actually enforced by the local limiter.
func (l Limits) soft() Limits {
	s := l
	s.RPM = atLeast1(int(float64(l.RPM) * safetyFactor))
	s.RPD = atLeast1(int(float64(l.RPD) * safetyFactor))
	s.TPD = atLeast1(int(float64(l.TPD) * safetyFactor))
	// TPM stays at the published value for the WINDOW (a single max-size
	// request must still fit); the per-request cap below keeps the margin.
	return s
}

// MaxRequestTokens is the largest prompt+max_tokens a single request may
// carry without risking HTTP 413 (request larger than the TPM budget).
func (l Limits) MaxRequestTokens() int {
	n := l.TPM - requestTokenMargin
	if l.ContextWindow > 0 && n > l.ContextWindow {
		n = l.ContextWindow
	}
	return n
}

func atLeast1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func envInt(key string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// EstimateTokens is a deliberately PESSIMISTIC token estimate for a chat
// request (no tokenizer dependency). Cyrillic (Russian / Kazakh) text costs
// ~1 token per 2.5–4 characters on the Qwen tokenizer;
// we assume 2.5, plus a fixed per-message overhead. Over-estimating only
// shrinks max_tokens a little — under-estimating could trigger HTTP 413.
func EstimateTokens(messages []Message) int {
	total := 3
	for _, m := range messages {
		total += 6 + EstimateTextTokens(m.Content)
	}
	return total
}

// EstimateTextTokens estimates the token count of a plain string.
func EstimateTextTokens(s string) int {
	var ascii, other int
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	// ASCII (JSON punctuation, digits, Latin) ≈ 4 chars/token; Cyrillic ≈ 2.5.
	return ascii/4 + (other*2)/5 + 1
}

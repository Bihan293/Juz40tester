package groq

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitError is returned when a request cannot be admitted within the
// caller's wait budget (local limiter) or Groq answered 429. RetryAfter is
// the best known moment the quota frees up again.
type RateLimitError struct {
	Model      string
	Reason     string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("groq: %s rate limited (%s), retry in %s", e.Model, e.Reason, e.RetryAfter.Round(time.Second))
}

// event is one admitted request in the sliding windows.
type event struct {
	id     uint64
	at     time.Time
	tokens int
}

// limiter enforces the per-model quota LOCALLY, before a request is sent:
//
//   - RPM / TPM — 60-second sliding windows;
//   - RPD / TPD — 24-hour sliding windows (stricter than any daily reset);
//   - server feedback — x-ratelimit-* headers and 429 retry-after block the
//     model until the moment Groq says the quota is back.
//
// Tokens are reserved at admission time as prompt + max_tokens (exactly what
// Groq checks) and corrected to the real usage once the reply arrives.
type limiter struct {
	model string
	lim   Limits // soft (safety-adjusted) limits
	now   func() time.Time

	mu     sync.Mutex
	seq    uint64  // event id counter
	minute []event // last 60 s
	day    []event // last 24 h

	blockedUntil time.Time // hard block from a 429 / exhausted daily quota
	blockReason  string

	// Server-reported TPM state (x-ratelimit-remaining-tokens), valid until
	// srvTokensReset.
	srvTokensRemaining int
	srvTokensReset     time.Time
}

func newLimiter(model string, l Limits) *limiter {
	return &limiter{model: model, lim: l.soft(), now: time.Now, srvTokensRemaining: -1}
}

// prune drops window entries that fell out of their windows. Caller holds mu.
func (l *limiter) prune(now time.Time) {
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(l.minute) && !l.minute[i].at.After(cut) {
		i++
	}
	l.minute = l.minute[i:]
	cut = now.Add(-24 * time.Hour)
	i = 0
	for i < len(l.day) && !l.day[i].at.After(cut) {
		i++
	}
	l.day = l.day[i:]
}

func sumTokens(ev []event) int {
	n := 0
	for _, e := range ev {
		n += e.tokens
	}
	return n
}

// admitWait returns 0 when a request of `tokens` can be sent right now, or
// how long to wait. daily=true marks a daily-quota block (waiting minutes
// won't help). Caller holds mu.
func (l *limiter) admitWait(now time.Time, tokens int) (wait time.Duration, reason string, daily bool) {
	l.prune(now)
	if now.Before(l.blockedUntil) {
		return l.blockedUntil.Sub(now), l.blockReason, strings.Contains(l.blockReason, "day")
	}
	// Daily windows first — if they are exhausted there is no point waiting.
	if len(l.day) >= l.lim.RPD {
		return l.day[0].at.Add(24 * time.Hour).Sub(now), "requests per day", true
	}
	if l.lim.TPD > 0 && sumTokens(l.day)+tokens > l.lim.TPD {
		return l.freeAfter(l.day, 24*time.Hour, l.lim.TPD, tokens, now), "tokens per day", true
	}
	if len(l.minute) >= l.lim.RPM {
		return l.minute[0].at.Add(time.Minute).Sub(now), "requests per minute", false
	}
	if sumTokens(l.minute)+tokens > l.lim.TPM {
		return l.freeAfter(l.minute, time.Minute, l.lim.TPM, tokens, now), "tokens per minute", false
	}
	if l.srvTokensRemaining >= 0 && now.Before(l.srvTokensReset) && tokens > l.srvTokensRemaining {
		return l.srvTokensReset.Sub(now), "tokens per minute (server)", false
	}
	return 0, "", false
}

// freeAfter returns how long until enough old events leave the window for
// `need` more tokens to fit under `limit`.
func (l *limiter) freeAfter(ev []event, window time.Duration, limit, need int, now time.Time) time.Duration {
	used := sumTokens(ev)
	for _, e := range ev {
		if used+need <= limit {
			break
		}
		used -= e.tokens
		if used+need <= limit {
			return e.at.Add(window).Sub(now) + 50*time.Millisecond
		}
	}
	return window
}

// reservation is an admitted request; commit it with the real token usage.
type reservation struct {
	l  *limiter
	id uint64
}

// acquire blocks until a request of `tokens` fits the quota, or fails with a
// *RateLimitError when that would take longer than maxWait (or the daily
// quota is exhausted). The request is recorded immediately, so concurrent
// callers never overshoot together.
func (l *limiter) acquire(ctx context.Context, tokens int, maxWait time.Duration) (*reservation, error) {
	deadline := l.now().Add(maxWait)
	for {
		l.mu.Lock()
		now := l.now()
		wait, reason, daily := l.admitWait(now, tokens)
		if wait <= 0 {
			l.seq++
			ev := event{id: l.seq, at: now, tokens: tokens}
			l.minute = append(l.minute, ev)
			l.day = append(l.day, ev)
			l.mu.Unlock()
			return &reservation{l: l, id: ev.id}, nil
		}
		l.mu.Unlock()
		if daily || now.Add(wait).After(deadline) {
			return nil, &RateLimitError{Model: l.model, Reason: reason, RetryAfter: wait}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// commit replaces the reserved token count with the real usage (Groq bills
// the quota by actual tokens; the reservation used prompt + max_tokens).
// actual < 0 keeps the reservation as is (unknown usage).
func (r *reservation) commit(actual int) {
	if r == nil || actual < 0 {
		return
	}
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	fix := func(ev []event) {
		for i := len(ev) - 1; i >= 0; i-- {
			if ev[i].id == r.id {
				ev[i].tokens = actual
				return
			}
		}
	}
	fix(l.minute)
	fix(l.day)
}

// block hard-blocks the model (429 / exhausted quota) for d.
func (l *limiter) block(d time.Duration, reason string) {
	if d <= 0 {
		d = 5 * time.Second
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	until := l.now().Add(d)
	if until.After(l.blockedUntil) {
		l.blockedUntil = until
		l.blockReason = reason
	}
}

// observeHeaders syncs the local limiter with the authoritative server
// counters (x-ratelimit-*; see docs/AI_PROVIDERS.md). This also repairs the
// daily counters after a restart: the local 24h window starts empty, but
// the first reply already reports how many daily requests are really left.
func (l *limiter) observeHeaders(h http.Header) {
	now := l.now()
	if rem, ok := headerInt(h, "x-ratelimit-remaining-requests"); ok {
		limit, _ := headerInt(h, "x-ratelimit-limit-requests")
		reset := parseResetDuration(h.Get("x-ratelimit-reset-requests"))
		// Keep the same safety margin the local limiter uses.
		reserve := 0
		if limit > 0 {
			reserve = limit - int(float64(limit)*safetyFactor)
		}
		if rem <= reserve {
			if reset <= 0 {
				reset = time.Hour
			}
			l.block(reset, "requests per day (server)")
		}
	}
	if rem, ok := headerInt(h, "x-ratelimit-remaining-tokens"); ok {
		reset := parseResetDuration(h.Get("x-ratelimit-reset-tokens"))
		if reset <= 0 {
			reset = time.Minute
		}
		l.mu.Lock()
		l.srvTokensRemaining = rem
		l.srvTokensReset = now.Add(reset)
		l.mu.Unlock()
	}
}

// snapshot returns usage counters for logging.
func (l *limiter) snapshot() (reqMin, tokMin, reqDay, tokDay int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(l.now())
	return len(l.minute), sumTokens(l.minute), len(l.day), sumTokens(l.day)
}

func headerInt(h http.Header, key string) (int, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		f, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, false
		}
		n = int(f)
	}
	return n, true
}

// parseResetDuration parses Groq reset values: "7.66s", "2m59.56s", "1h2m3s"
// or a bare number of seconds.
func parseResetDuration(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(f * float64(time.Second))
	}
	return 0
}

// parseRetryAfter parses the retry-after header (seconds, may be fractional).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(f*float64(time.Second)) + 250*time.Millisecond
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return 0
}

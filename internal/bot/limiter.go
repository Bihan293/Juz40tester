package bot

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultMaxRPS is the default global rate of outgoing message-changing
// Bot API calls (TG_MAX_RPS). Telegram allows ~30 messages per second per
// bot; 20 leaves headroom for answerCallbackQuery and service calls.
const DefaultMaxRPS = 20

// defaultBurst is the number of calls that may go out back-to-back after an
// idle period.
const defaultBurst = 5

// slowWaitThreshold: waits longer than this are counted and reported (a
// signal that the bot needs to scale or lower its outgoing traffic).
const slowWaitThreshold = 2 * time.Second

// slowWaitLogEvery bounds the slow-wait warning to one line per interval.
const slowWaitLogEvery = time.Minute

// maxGlobalPause caps how long one 429 stops the WHOLE limiter. A longer
// retry_after is usually a per-chat flood ban: that chat is handled by the
// R-3 per-chat ban + deferred queue, the other chats keep going.
const maxGlobalPause = 5 * time.Second

// limitedMethods are the calls that create or change messages: they share
// the global limiter. answerCallbackQuery (must answer at once, otherwise
// the button keeps spinning) and service calls (getMe, setWebhook) are NOT
// limited.
var limitedMethods = map[string]bool{
	"sendMessage":            true,
	"editMessageText":        true,
	"editMessageReplyMarkup": true,
	"editMessageCaption":     true,
	"editMessageMedia":       true,
	"deleteMessage":          true,
	"deleteMessages":         true,
	"sendPhoto":              true,
	"sendDocument":           true,
	"sendAudio":              true,
	"sendVideo":              true,
	"sendVoice":              true,
	"sendAnimation":          true,
	"sendMediaGroup":         true,
	"sendSticker":            true,
	"copyMessage":            true,
	"forwardMessage":         true,
}

// rateLimiter is a global token bucket (GCRA form, no extra dependencies).
// A call never gets dropped: Wait blocks until its slot or until ctx ends.
// pause stops the whole limiter (Telegram 429 retry_after).
type rateLimiter struct {
	mu          sync.Mutex
	interval    time.Duration // 1 / rate
	tau         time.Duration // (burst-1) * interval
	tat         time.Time     // theoretical arrival time of the next call
	pausedUntil time.Time
	now         func() time.Time

	slowWaits atomic.Int64 // calls that waited longer than slowWaitThreshold
	slowMu    sync.Mutex
	lastSlow  time.Time
	slowSince int64 // slowWaits value at the last warning
}

func newRateLimiter(rps, burst int) *rateLimiter {
	if rps <= 0 {
		rps = DefaultMaxRPS
	}
	if burst <= 0 {
		burst = 1
	}
	iv := time.Second / time.Duration(rps)
	return &rateLimiter{
		interval: iv,
		tau:      time.Duration(burst-1) * iv,
		now:      time.Now,
	}
}

// reserve books the next slot and returns how long the caller must wait.
func (l *rateLimiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	base := l.tat
	if base.Before(now) {
		base = now
	}
	if base.Before(l.pausedUntil) {
		base = l.pausedUntil
	}
	allowAt := base.Add(-l.tau)
	if allowAt.Before(now) {
		allowAt = now
	}
	if allowAt.Before(l.pausedUntil) {
		allowAt = l.pausedUntil
	}
	l.tat = base.Add(l.interval)
	return allowAt.Sub(now)
}

// cancel returns a booked but unused slot (the waiting call was cancelled).
func (l *rateLimiter) cancel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tat = l.tat.Add(-l.interval)
	if now := l.now(); l.tat.Before(now) {
		l.tat = now
	}
}

// Wait blocks until the caller may send. A cancelled / timed out ctx
// returns its error (like a network error of the call).
func (l *rateLimiter) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := l.reserve()
	if d <= 0 {
		return nil
	}
	if d > slowWaitThreshold {
		l.noteSlow(d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		l.cancel()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pause stops the whole limiter for d (Telegram 429 retry_after).
func (l *rateLimiter) pause(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if until := l.now().Add(d); until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}

// noteSlow counts a long wait and logs a warning at most once a minute.
func (l *rateLimiter) noteSlow(d time.Duration) {
	total := l.slowWaits.Add(1)
	l.slowMu.Lock()
	defer l.slowMu.Unlock()
	now := l.now()
	if !l.lastSlow.IsZero() && now.Sub(l.lastSlow) < slowWaitLogEvery {
		return
	}
	log.Printf("WARNING: telegram rate limiter: %d call(s) waited > %s since last report (total %d, current wait %s) — consider raising TG_MAX_RPS or scaling",
		total-l.slowSince, slowWaitThreshold, total, d.Round(time.Millisecond))
	l.lastSlow = now
	l.slowSince = total
}

// SlowWaits returns how many limited calls waited longer than 2 s.
func (c *Client) SlowWaits() int64 {
	if c.limiter == nil {
		return 0
	}
	return c.limiter.slowWaits.Load()
}

// WithMaxRPS sets the global rate of message-changing calls (TG_MAX_RPS).
// rps <= 0 keeps the default.
func (c *Client) WithMaxRPS(rps int) *Client {
	if rps <= 0 {
		rps = DefaultMaxRPS
	}
	c.limiter = newRateLimiter(rps, min(defaultBurst, rps))
	return c
}

// waitLimit waits for the global limiter when method is a limited one.
func (c *Client) waitLimit(ctx context.Context, method string) error {
	if c.limiter == nil || !limitedMethods[method] {
		return nil
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return c.safeErr(method, err)
	}
	return nil
}

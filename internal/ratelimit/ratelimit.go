// Package ratelimit implements the per-user action throttle (R-9).
//
// LIMITATION (documented on purpose): the state is IN MEMORY, per process.
// The bot is designed to run as ONE instance (Render web service); with N
// instances behind the webhook a user could get up to N actions per
// interval in the worst case, and the state is lost on restart. That is
// acceptable for abuse protection against tap-spam (it is not a security
// boundary). Limits that MUST hold across instances — the daily personal
// generations per user and the DeepSeek spending cap — are enforced in
// PostgreSQL instead (see repositories.SpendRepository /
// GenerationRepository.PersonalJobsSince).
package ratelimit

import (
	"sync"
	"time"
)

// Limiter allows at most one action per key every Interval.
type Limiter struct {
	interval time.Duration
	now      func() time.Time

	mu   sync.Mutex
	last map[int64]time.Time
	// maxKeys triggers a sweep of stale keys so memory stays bounded.
	maxKeys int
}

// New creates a limiter; interval <= 0 disables it (Allow always true).
func New(interval time.Duration) *Limiter {
	return &Limiter{interval: interval, now: time.Now, last: map[int64]time.Time{}, maxKeys: 50000}
}

// WithClock replaces the clock (tests).
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allow reports whether key may act now and, if so, records the action.
// A rejected action does NOT move the window, so a user who keeps tapping
// gets through again exactly Interval after the last accepted action.
func (l *Limiter) Allow(key int64) bool {
	if l == nil || l.interval <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.interval {
		return false
	}
	if len(l.last) >= l.maxKeys {
		for k, t := range l.last {
			if now.Sub(t) >= l.interval {
				delete(l.last, k)
			}
		}
	}
	l.last[key] = now
	return true
}

// Len returns the number of tracked keys (tests / diagnostics).
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.last)
}

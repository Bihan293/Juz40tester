package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// healthCache serves /health from a cached database ping result (R-5c):
// Render's health check and uptime monitors hit /health often, and a ping
// on every request kept Neon from ever scaling to zero. A ping runs at most
// once per ttl; concurrent requests share one in-flight ping. ttl <= 0
// disables the cache.
type healthCache struct {
	ping func(context.Context) error
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	checked time.Time
	healthy bool
}

func newHealthCache(ping func(context.Context) error, ttl time.Duration) *healthCache {
	return &healthCache{ping: ping, ttl: ttl, now: time.Now}
}

// Healthy returns the cached result, refreshing it when it is older than ttl.
func (h *healthCache) Healthy(ctx context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ttl > 0 && !h.checked.IsZero() && h.now().Sub(h.checked) < h.ttl {
		return h.healthy
	}
	// Detached from the request: a client that hangs up (a monitor with a
	// short timeout) cancels r.Context(), and that ping error was cached as
	// «unhealthy» for the whole ttl — every /health answered 503 meanwhile.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	h.healthy = h.ping(pctx) == nil
	h.checked = h.now()
	return h.healthy
}

func (h *healthCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !h.Healthy(r.Context()) {
		http.Error(w, `{"status":"unhealthy"}`, http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

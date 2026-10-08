package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthCachePingsOncePerTTL(t *testing.T) {
	calls := 0
	var pingErr error
	h := newHealthCache(func(context.Context) error { calls++; return pingErr }, 45*time.Second)
	now := time.Unix(1000, 0)
	h.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", rec.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("pings = %d, want 1 within TTL", calls)
	}

	now = now.Add(46 * time.Second)
	pingErr = errors.New("down")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusServiceUnavailable || calls != 2 {
		t.Fatalf("after TTL: code = %d, pings = %d; want 503, 2", rec.Code, calls)
	}
}

// A client that hangs up must not make /health cache «unhealthy» for the
// whole TTL: the ping is detached from the request context.
func TestHealthCacheIgnoresCancelledRequest(t *testing.T) {
	h := newHealthCache(func(ctx context.Context) error { return ctx.Err() }, 45*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !h.Healthy(ctx) {
		t.Fatal("a cancelled request context made the cached result unhealthy")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

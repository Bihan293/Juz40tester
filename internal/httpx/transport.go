// Package httpx holds the shared HTTP transport tuning of the outbound API
// clients (Telegram, Groq, DeepSeek).
package httpx

import (
	"net/http"
	"time"
)

const (
	// MaxIdleConns is the total idle keep-alive connections kept open.
	MaxIdleConns = 100
	// MaxIdleConnsPerHost: http.DefaultTransport keeps only 2 idle
	// connections per host, so a burst of concurrent Bot API calls (up to
	// 32 updates in flight) kept opening fresh TCP+TLS connections. 64
	// lets them be reused.
	MaxIdleConnsPerHost = 64
	// IdleConnTimeout closes keep-alive connections idle for that long.
	IdleConnTimeout = 90 * time.Second
)

// NewTransport returns a clone of http.DefaultTransport (so proxy from the
// environment, dial/TLS-handshake timeouts, HTTP/2 and the default TLS
// config are all preserved) with larger keep-alive pools.
func NewTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = MaxIdleConns
	t.MaxIdleConnsPerHost = MaxIdleConnsPerHost
	t.IdleConnTimeout = IdleConnTimeout
	return t
}

// NewClient returns an http.Client with the given overall request timeout
// over a fresh tuned transport. Per-request context cancellation still
// works as usual (requests are built with NewRequestWithContext).
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport()}
}

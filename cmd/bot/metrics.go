package main

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Bihan293/Juz40tester/internal/metrics"
)

// metricsLogEvery: how often the counters are summarised in the log
// ("metrics: …" — for log analysis without a metrics backend).
const metricsLogEvery = 15 * time.Minute

// metricsHandler serves GET /metrics (Prometheus text format). With a
// non-empty token the request must carry «Authorization: Bearer <token>»
// or ?token=<token>.
func metricsHandler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		metrics.WritePrometheus(w)
	})
}

// logMetrics writes a one-line summary of the counters every interval.
func logMetrics(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			log.Printf("metrics: %s", metrics.Summary())
		}
	}
}

// Package metrics keeps process-local counters of the AI generation
// pipeline (difficulty violations, rejected replies, strategy outcomes,
// template hits, …).
//
// The counters are cheap (one mutex-protected map increment), exported in
// a Prometheus-compatible text format by GET /metrics and summarised in the
// log every few minutes ("metrics: …" lines) for log analysis. Durable
// per-day aggregates per generation strategy live in PostgreSQL
// (gen_strategy_stats) — these in-memory counters reset on restart.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Counter names (kept in one place so the log analysis is stable).
const (
	// DifficultyViolations: a generated reply (or question of a batch) broke
	// the difficulty constraint of its chain position.
	DifficultyViolations = "gen_difficulty_violations_total"
	// RejectedReplies: a model reply rejected by a local validator.
	RejectedReplies = "gen_rejected_replies_total"
	// RepeatRejects: a question repeated the previous chain test.
	RepeatRejects = "gen_repeat_rejects_total"
	// WeakCoverageRejects: a reply ignored the weak topics it had to train.
	WeakCoverageRejects = "gen_weak_coverage_rejects_total"
	// AICalls: model calls by provider step.
	AICalls = "gen_ai_calls_total"
	// Jobs: finished generation jobs by kind / strategy / result.
	Jobs = "gen_jobs_total"
	// TemplateHits / TemplateSaves: fingerprint template reuse.
	TemplateHits  = "gen_template_hits_total"
	TemplateSaves = "gen_template_saves_total"
	// BatchReuse: batches of a retried job reused instead of regenerated.
	BatchReuse = "gen_batch_reuse_total"
	// QueueBackpressure: personal generations refused because the queue is full.
	QueueBackpressure = "gen_queue_backpressure_total"
	// DuplicateUpdates: Telegram updates dropped as already seen (update_id).
	DuplicateUpdates = "tg_duplicate_updates_total"
)

type registry struct {
	mu sync.Mutex
	c  map[string]float64 // key = name{labels}
}

var std = &registry{c: map[string]float64{}}

// Inc adds 1 to the counter name with the given label pairs
// ("k1", "v1", "k2", "v2", …).
func Inc(name string, labels ...string) { Add(name, 1, labels...) }

// Add adds v to the counter.
func Add(name string, v float64, labels ...string) {
	key := name + labelString(labels)
	std.mu.Lock()
	std.c[key] += v
	std.mu.Unlock()
}

// Get returns the current value of a counter (tests, health output).
func Get(name string, labels ...string) float64 {
	key := name + labelString(labels)
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.c[key]
}

// Sum returns the sum of every label combination of the counter.
func Sum(name string) float64 {
	std.mu.Lock()
	defer std.mu.Unlock()
	var s float64
	for k, v := range std.c {
		if k == name || strings.HasPrefix(k, name+"{") {
			s += v
		}
	}
	return s
}

// Snapshot returns a copy of every counter.
func Snapshot() map[string]float64 {
	std.mu.Lock()
	defer std.mu.Unlock()
	out := make(map[string]float64, len(std.c))
	for k, v := range std.c {
		out[k] = v
	}
	return out
}

// Reset clears every counter (tests only).
func Reset() {
	std.mu.Lock()
	std.c = map[string]float64{}
	std.mu.Unlock()
}

// WritePrometheus writes every counter in the Prometheus text format.
func WritePrometheus(w io.Writer) {
	snap := Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	typed := map[string]bool{}
	for _, k := range keys {
		name := k
		if i := strings.IndexByte(k, '{'); i >= 0 {
			name = k[:i]
		}
		if !typed[name] {
			fmt.Fprintf(w, "# TYPE %s counter\n", name)
			typed[name] = true
		}
		fmt.Fprintf(w, "%s %g\n", k, snap[k])
	}
}

// Summary is a compact one-line rendering for the periodic log line.
func Summary() string {
	snap := Snapshot()
	if len(snap) == 0 {
		return "(no events)"
	}
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%g", k, snap[k]))
	}
	return strings.Join(parts, " ")
}

func labelString(labels []string) string {
	if len(labels) < 2 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i])
		b.WriteString(`="`)
		b.WriteString(escape(labels[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", " ")
}

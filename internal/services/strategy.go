package services

// Generation strategies and their A/B testing (audit item 4).
//
//   - "batch": the test is assembled from small batches (GenBatchSize, 5
//     questions) — each model call writes a few questions against an exact
//     per-question spec (topic + difficulty). A bad question costs one small
//     call, valid questions of a partly bad reply are kept, finished batches
//     survive a retry of the job (gen_job_batches).
//   - "full": the original single call that writes all 20 questions.
//   - "ab":   per job, GEN_AB_BATCH_PERCENT % of the jobs run "batch", the
//     rest "full" (deterministic by job id, so a retried job keeps its arm).
//
// Every job records its strategy (generation_jobs.strategy) and its outcome
// (gen_strategy_stats per day/strategy/kind + in-memory metrics), so the
// arms can be compared on success rate, AI calls, rejects, difficulty
// violations, tokens and time.

import (
	"context"
	"hash/fnv"
	"strconv"
	"sync/atomic"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
)

const (
	strategyBatch = config.GenStrategyBatch
	strategyFull  = config.GenStrategyFull
	// strategyTopicBatch: B4 bank batches (one call, ~10 questions, own path).
	strategyTopicBatch = "topic10"
	// strategyTemplate: the job was served by a fingerprint template clone.
	strategyTemplate = "template"
)

// chooseStrategy picks the generation strategy of a job.
//
// A zero-value config (unit/integration tests building config.Config{}
// directly) keeps the original single-call behaviour; config.Load defaults
// GEN_STRATEGY to "batch" in production.
//
// Retry escalation: a RETRIED job always runs "batch" — models ignore the
// difficulty/topic instructions most often exactly on a retry, and the
// per-question spec of the batch prompt is much harder to ignore (and is
// validated question by question).
func (g *GeneratorService) chooseStrategy(job *models.GenerationJob) string {
	if job.Kind == models.JobKindTopicBatch {
		return strategyTopicBatch
	}
	s := ""
	pct := config.DefaultGenABBatchPercent
	if g.cfg != nil {
		s = g.cfg.GenStrategy
		if g.cfg.GenABBatchPercent >= 0 {
			pct = g.cfg.GenABBatchPercent
		}
	}
	var chosen string
	switch s {
	case config.GenStrategyBatch:
		chosen = strategyBatch
	case config.GenStrategyAB:
		if abBucket(job.ID) < pct {
			chosen = strategyBatch
		} else {
			chosen = strategyFull
		}
	default: // "full" or unset
		chosen = strategyFull
	}
	if chosen == strategyFull && job.Attempts > 1 && s != "" {
		return strategyBatch
	}
	return chosen
}

// abBucket maps a job id to 0..99 (stable).
func abBucket(id int64) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strconv.FormatInt(id, 10)))
	return int(h.Sum32() % 100)
}

// genRun accumulates the per-job outcome while the job runs (attached to
// the job context, updated by runSteps / the Groq steps).
type genRun struct {
	strategy      string
	kind          string
	calls         atomic.Int32
	rejects       atomic.Int32
	diffViolation atomic.Int32
	repeats       atomic.Int32
	promptTok     atomic.Int64
	complTok      atomic.Int64
	templateHit   bool
}

type genRunKey struct{}

func withGenRun(ctx context.Context, r *genRun) context.Context {
	ctx = context.WithValue(ctx, genRunKey{}, r)
	// Paid DeepSeek calls report their tokens too (the Groq steps call
	// noteTokens themselves): the per-job «tokens_in/out» used to show
	// only the free tokens.
	return deepseek.WithUsageHook(ctx, func(prompt, completion int) {
		r.promptTok.Add(int64(prompt))
		r.complTok.Add(int64(completion))
	})
}

func genRunFrom(ctx context.Context) *genRun {
	r, _ := ctx.Value(genRunKey{}).(*genRun)
	return r
}

// noteAICall counts one model call (metrics + job outcome).
func noteAICall(ctx context.Context, step string) {
	r := genRunFrom(ctx)
	strategy, kind := "", ""
	if r != nil {
		r.calls.Add(1)
		strategy, kind = r.strategy, r.kind
	}
	metrics.Inc(metrics.AICalls, "step", step, "strategy", strategy, "kind", kind)
}

// noteTokens adds token usage of a call to the job outcome.
func noteTokens(ctx context.Context, prompt, completion int) {
	if r := genRunFrom(ctx); r != nil {
		r.promptTok.Add(int64(prompt))
		r.complTok.Add(int64(completion))
	}
}

// noteReject counts a rejected reply (metrics + job outcome), by class.
func noteReject(ctx context.Context, step string, err error) {
	class := rejectClass(err)
	if class == "" {
		class = rejectFormat
	}
	r := genRunFrom(ctx)
	strategy, kind := "", ""
	if r != nil {
		r.rejects.Add(1)
		strategy, kind = r.strategy, r.kind
		switch class {
		case rejectDifficulty:
			r.diffViolation.Add(1)
		case rejectRepeat:
			r.repeats.Add(1)
		}
	}
	metrics.Inc(metrics.RejectedReplies, "class", class, "step", step, "strategy", strategy, "kind", kind)
	switch class {
	case rejectDifficulty:
		metrics.Inc(metrics.DifficultyViolations, "step", step, "strategy", strategy, "kind", kind)
	case rejectRepeat:
		metrics.Inc(metrics.RepeatRejects, "strategy", strategy, "kind", kind)
	case rejectWeak:
		metrics.Inc(metrics.WeakCoverageRejects, "strategy", strategy, "kind", kind)
	}
}

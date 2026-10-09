// Package services — AI provider routing (cost optimisation).
//
// Every AI task goes through a short, ordered list of "steps". A step is one
// provider+model+effort combination; the first step whose reply passes the
// task's local validator wins. The order is chosen by price:
//
//	Groq free tier (GPT-OSS 120B / Qwen 3.8 27B) → paid DeepSeek fallback.
//
// Groq quota is enforced locally by internal/groq (RPM/RPD/TPM/TPD), so a
// step that would exceed the free-tier limits is SKIPPED instantly (no HTTP
// call, no error in the logs beyond one line) and the next step runs. The
// bot therefore never gets stuck on a 429 and never breaks when the daily
// free quota is exhausted — it just spends DeepSeek money for the rest of
// the day, exactly like before this integration.
package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
)

// aiStep is one attempt in a provider chain.
type aiStep struct {
	name string // for logs, e.g. "groq/openai/gpt-oss-120b(medium)"
	run  func(ctx context.Context) (string, error)
	// timeout bounds this single step (0 = only the parent context).
	timeout time.Duration
	// reserve is the time this (paid) step needs to run. Every EARLIER step
	// is cut so that at least `reserve` of the parent deadline is left for
	// it — the free Groq steps can never eat the time of the paid DeepSeek
	// fallback (or of the repair that follows the generation).
	reserve time.Duration
	// paid marks a step that costs money (DeepSeek). Before such a step
	// runs, free steps that failed only on a SHORT-LIVED Groq quota (the
	// per-minute window) are retried once after the quota is back — see
	// runStepsOpts.
	paid bool
}

// freeRetryMaxWait is the longest runStepsFeedback waits for a free Groq
// model's per-minute quota before it pays for DeepSeek instead. The Groq
// free tier allows ~one generation request per model per minute (TPM
// 8000), so with several workers and parallel batches a free step was
// often skipped after a short limiter wait — and the paid fallback ran
// although Groq would have served the request a few seconds later.
const freeRetryMaxWait = 70 * time.Second

// errStepNotNeeded is returned by a conditional step that does not apply
// to this run (e.g. the cheap retry of a free model whose first reply was
// not rejected). No call was made: it is neither logged nor counted.
var errStepNotNeeded = errors.New("step not needed")

// maxPaidCallsPerJob caps the paid DeepSeek steps of ONE generation job
// run (all its batches, rescue rounds and repairs together). A test the
// models keep getting wrong (every batch rejected, rounds and rescues
// re-asking) could otherwise make dozens of paid calls in one run — and
// again on each of its retries. Past the cap the job fails this attempt
// (retried later with the batches it already paid for) instead.
const maxPaidCallsPerJob = 12

// errPaidCallLimit: the job used up its paid calls for this run.
var errPaidCallLimit = errors.New("paid DeepSeek calls of this job run exhausted")

type paidCallsKey struct{}

type paidCalls struct {
	n   atomic.Int32
	max int32
}

// withPaidCallLimit attaches a per-job paid-call counter to ctx.
func withPaidCallLimit(ctx context.Context, max int) context.Context {
	return context.WithValue(ctx, paidCallsKey{}, &paidCalls{max: int32(max)})
}

// takePaidCall books one paid call of the job (no limit when ctx carries
// no counter, e.g. the quality sweep — bounded by the daily cap).
func takePaidCall(ctx context.Context) error {
	pc, _ := ctx.Value(paidCallsKey{}).(*paidCalls)
	if pc == nil {
		return nil
	}
	if pc.n.Add(1) > pc.max {
		return fmt.Errorf("%w (%d)", errPaidCallLimit, pc.max)
	}
	return nil
}

// stepOpts tunes runStepsOpts.
type stepOpts struct {
	onReject func(error)
	// freeRetryWait caps the wait for a rate-limited free step before the
	// paid one (0 = do not wait, pay at once).
	freeRetryWait time.Duration
}

// groqStepTimeout bounds one Groq step: the local rate-limiter wait
// (MaxWait) plus the HTTP request itself.
const groqStepTimeout = 150 * time.Second

// stepContext derives the context of step i: its own timeout, further cut
// so that the largest reserve of the later steps stays available. ok=false
// means there is no time left for this step at all (it is skipped).
func stepContext(ctx context.Context, steps []aiStep, i int) (context.Context, context.CancelFunc, bool) {
	var reserve time.Duration
	for _, later := range steps[i+1:] {
		if later.reserve > reserve {
			reserve = later.reserve
		}
	}
	timeout := steps[i].timeout
	if dl, has := ctx.Deadline(); has && reserve > 0 {
		left := time.Until(dl) - reserve
		if left <= 0 {
			return ctx, func() {}, false
		}
		if timeout == 0 || left < timeout {
			timeout = left
		}
	}
	if timeout <= 0 {
		return ctx, func() {}, true
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	return sctx, cancel, true
}

// runSteps executes steps in order until validate accepts a reply. It
// returns the accepted raw reply and the name of the step that produced it.
// Errors of every failed step are joined into the final error.
func runSteps(ctx context.Context, task string, steps []aiStep, validate func(raw string) error) (string, string, error) {
	return runStepsFeedback(ctx, task, steps, validate, nil)
}

// runStepsFeedback is runSteps with a rejection hook: onReject is called
// with the validation error of a step's reply BEFORE the next step runs, so
// the caller can feed the reason back into the next prompt («your previous
// reply was rejected: mean difficulty 1.6, need 3.5») — models ignore
// instructions most often on a blind retry. Every model call and every
// rejected reply is counted (metrics + the job's genRun); a difficulty
// violation is additionally logged as one greppable line:
//
//	difficulty_violation task=… step=… err=…
func runStepsFeedback(ctx context.Context, task string, steps []aiStep, validate func(raw string) error, onReject func(error)) (string, string, error) {
	return runStepsOpts(ctx, task, steps, validate, stepOpts{onReject: onReject, freeRetryWait: freeRetryMaxWait})
}

// runStepsOpts is runStepsFeedback with options. Before the first PAID step
// runs, every free step that failed only because its per-minute Groq quota
// was busy (groq.RetryableSoon) is retried ONCE, after waiting until the
// quota is back (at most opts.freeRetryWait, and never into the time
// reserved for the paid step). The provider order is unchanged — DeepSeek
// still runs when the free retry fails too — it only stops paying for work
// the free tier would have done a few seconds later.
func runStepsOpts(ctx context.Context, task string, steps []aiStep, validate func(raw string) error, opts stepOpts) (string, string, error) {
	var errs []error
	var busyFree []int // free steps that failed on a short-lived quota
	var busyWait time.Duration
	freeRetried := false
	runOne := func(i int) (string, bool) {
		st := steps[i]
		sctx, cancel, ok := stepContext(ctx, steps, i)
		if !ok {
			log.Printf("ai[%s]: %s skipped — time is reserved for the next provider", task, st.name)
			errs = append(errs, fmt.Errorf("%s: skipped (time reserved for fallback)", st.name))
			return "", false
		}
		started := time.Now()
		raw, err := st.run(sctx)
		cancel()
		if errors.Is(err, errStepNotNeeded) {
			return "", false // conditional step that did not apply (no call made)
		}
		if err == nil || (!groq.IsRateLimited(err) && !errors.Is(err, groq.ErrTooLarge) && !errors.Is(err, errPaidCallLimit)) {
			noteAICall(ctx, st.name)
		}
		if err == nil {
			if verr := validate(raw); verr != nil {
				noteReject(ctx, st.name, verr)
				if rejectClass(verr) == rejectDifficulty {
					log.Printf("difficulty_violation task=%q step=%s err=%q", task, st.name, verr.Error())
				}
				if opts.onReject != nil {
					opts.onReject(verr)
				}
				err = fmt.Errorf("invalid reply: %w", verr)
			}
		}
		if err == nil {
			log.Printf("ai[%s]: served by %s in %.1fs", task, st.name, time.Since(started).Seconds())
			return raw, true
		}
		if groq.IsRateLimited(err) {
			log.Printf("ai[%s]: %s skipped — free-tier quota: %v", task, st.name, err)
			if wait, soon := groq.RetryableSoon(err); soon && !st.paid {
				busyFree = append(busyFree, i)
				if busyWait == 0 || wait < busyWait {
					busyWait = wait
				}
			}
		} else {
			log.Printf("ai[%s]: %s failed: %v", task, st.name, err)
		}
		errs = append(errs, fmt.Errorf("%s: %w", st.name, err))
		return "", false
	}
	for i, st := range steps {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if st.paid && !freeRetried && len(busyFree) > 0 {
			freeRetried = true
			retry := busyFree
			busyFree = nil
			if waitForFreeQuota(ctx, task, st, busyWait, opts.freeRetryWait) {
				for _, fi := range retry {
					if ctx.Err() != nil {
						break
					}
					if raw, ok := runOne(fi); ok {
						return raw, steps[fi].name, nil
					}
				}
			}
			if ctx.Err() != nil {
				errs = append(errs, ctx.Err())
				break
			}
		}
		if raw, ok := runOne(i); ok {
			return raw, st.name, nil
		}
	}
	if len(errs) == 0 {
		return "", "", fmt.Errorf("ai[%s]: no AI provider configured", task)
	}
	return "", "", errors.Join(errs...)
}

// waitForFreeQuota sleeps until a rate-limited free model should accept a
// request again. false = not worth it (the wait is longer than maxWait or
// would eat the time reserved for the paid step, or ctx ended).
func waitForFreeQuota(ctx context.Context, task string, paid aiStep, wait, maxWait time.Duration) bool {
	if maxWait <= 0 || wait > maxWait {
		return false
	}
	if wait < 0 {
		wait = 0
	}
	if dl, ok := ctx.Deadline(); ok {
		// Leave the paid step its reserve plus a minimal free call.
		if time.Until(dl)-wait < paid.reserve+30*time.Second {
			return false
		}
	}
	log.Printf("ai[%s]: free Groq quota is back in %s — waiting instead of paying for %s", task, wait.Round(time.Second), paid.name)
	if wait == 0 {
		return true
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// toGroqMessages converts DeepSeek-style messages (shared prompt builders).
func toGroqMessages(msgs []deepseek.Message) []groq.Message {
	out := make([]groq.Message, len(msgs))
	for i, m := range msgs {
		out[i] = groq.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

// groqStep builds a Groq step.
func groqStep(gc *groq.Client, req groq.Request) aiStep {
	name := "groq/" + req.Model
	if req.Effort != "" {
		name += "(" + req.Effort + ")"
	}
	return aiStep{name: name, timeout: groqStepTimeout, run: func(ctx context.Context) (string, error) {
		res, err := gc.ChatJSON(ctx, req)
		if err != nil {
			return "", err
		}
		noteTokens(ctx, res.PromptTokens, res.CompletionTokens)
		return res.Content, nil
	}}
}

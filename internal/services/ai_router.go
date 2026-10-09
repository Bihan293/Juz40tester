// Package services — AI call steps.
//
// Every AI task runs through a short, ordered list of "steps" (one
// provider+model+mode each); the first step whose reply passes the task's
// local validator wins. There are only two routes left:
//
//	test generation (every kind, retries, batches, repairs):
//	    DeepSeek deepseek-flash, thinking, reasoning_effort=high — ONE step
//	RU→KK translation:
//	    Groq qwen/qwen3.8-27b (instruct) → DeepSeek deepseek-flash (non-thinking)
//
// Groq quota is enforced locally by internal/groq (RPM/RPD/TPM/TPD), so a
// translation step that would exceed the free-tier limits is SKIPPED
// instantly (no HTTP call) and the DeepSeek fallback runs.
package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
)

// aiStep is one attempt in a provider chain.
type aiStep struct {
	name string // for logs, e.g. "deepseek/deepseek-flash(thinking-high)"
	run  func(ctx context.Context) (string, error)
	// timeout bounds this single step (0 = only the parent context).
	timeout time.Duration
}

// groqStepTimeout bounds one Groq step: the local rate-limiter wait
// (MaxWait) plus the HTTP request itself.
const groqStepTimeout = 150 * time.Second

// stepContext derives the context of one step (its own timeout, if any).
func stepContext(ctx context.Context, st aiStep) (context.Context, context.CancelFunc) {
	if st.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, st.timeout)
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
	var errs []error
	for _, st := range steps {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		sctx, cancel := stepContext(ctx, st)
		started := time.Now()
		raw, err := st.run(sctx)
		cancel()
		if err == nil || (!groq.IsRateLimited(err) && !errors.Is(err, groq.ErrTooLarge)) {
			noteAICall(ctx, st.name)
		}
		if err == nil {
			if verr := validate(raw); verr != nil {
				noteReject(ctx, st.name, verr)
				if rejectClass(verr) == rejectDifficulty {
					log.Printf("difficulty_violation task=%q step=%s err=%q", task, st.name, verr.Error())
				}
				if onReject != nil {
					onReject(verr)
				}
				err = fmt.Errorf("invalid reply: %w", verr)
			}
		}
		if err == nil {
			log.Printf("ai[%s]: served by %s in %.1fs", task, st.name, time.Since(started).Seconds())
			return raw, st.name, nil
		}
		if groq.IsRateLimited(err) {
			log.Printf("ai[%s]: %s skipped — Groq free-tier quota: %v", task, st.name, err)
		} else {
			log.Printf("ai[%s]: %s failed: %v", task, st.name, err)
		}
		errs = append(errs, fmt.Errorf("%s: %w", st.name, err))
	}
	if len(errs) == 0 {
		return "", "", fmt.Errorf("ai[%s]: no AI provider configured", task)
	}
	return "", "", errors.Join(errs...)
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

// genStep is THE generation step: deepseek-flash in thinking mode with
// reasoning_effort=high. Every generation (chain, personal, retry, batch,
// topic batch, repair) uses it; there is no other provider and no lower
// effort. When bounded is set, the call takes a slot of the paid-call
// semaphore (GEN_DEEPSEEK_CONCURRENCY) — more workers must not mean a
// proportional burst of parallel DeepSeek calls.
func (g *GeneratorService) genStep(messages func() []deepseek.Message, maxTokens int, timeout time.Duration, bounded bool) aiStep {
	ds := g.ds
	return aiStep{
		name:    genStepName,
		timeout: timeout,
		run: func(ctx context.Context) (string, error) {
			if bounded {
				release, err := g.acquireDeepSeek(ctx)
				if err != nil {
					return "", err
				}
				defer release()
			}
			return ds.GenerateJSON(ctx, messages(), maxTokens)
		},
	}
}

// genStepName is the log/metrics name of the generation step.
const genStepName = "deepseek/" + deepseek.Model + "(thinking-" + deepseek.GenerationEffort + ")"

// genSteps returns the generation route (empty without DEEPSEEK_API_KEY —
// runSteps then reports "no AI provider configured").
func (g *GeneratorService) genSteps(messages func() []deepseek.Message, maxTokens int, timeout time.Duration, bounded bool) []aiStep {
	if g.ds == nil {
		return nil
	}
	return []aiStep{g.genStep(messages, maxTokens, timeout, bounded)}
}

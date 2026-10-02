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
	var errs []error
	for i, st := range steps {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		sctx, cancel, ok := stepContext(ctx, steps, i)
		if !ok {
			log.Printf("ai[%s]: %s skipped — time is reserved for the next provider", task, st.name)
			errs = append(errs, fmt.Errorf("%s: skipped (time reserved for fallback)", st.name))
			continue
		}
		started := time.Now()
		raw, err := st.run(sctx)
		cancel()
		if err == nil {
			err = validate(raw)
			if err != nil {
				err = fmt.Errorf("invalid reply: %w", err)
			}
		}
		if err == nil {
			log.Printf("ai[%s]: served by %s in %.1fs", task, st.name, time.Since(started).Seconds())
			return raw, st.name, nil
		}
		if groq.IsRateLimited(err) {
			log.Printf("ai[%s]: %s skipped — free-tier quota: %v", task, st.name, err)
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
		return res.Content, nil
	}}
}

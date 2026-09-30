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

	"github.com/Bihan293/Juz40-test2/internal/deepseek"
	"github.com/Bihan293/Juz40-test2/internal/groq"
)

// aiStep is one attempt in a provider chain.
type aiStep struct {
	name string // for logs, e.g. "groq/openai/gpt-oss-120b(medium)"
	run  func(ctx context.Context) (string, error)
}

// runSteps executes steps in order until validate accepts a reply. It
// returns the accepted raw reply and the name of the step that produced it.
// Errors of every failed step are joined into the final error.
func runSteps(ctx context.Context, task string, steps []aiStep, validate func(raw string) error) (string, string, error) {
	var errs []error
	for _, st := range steps {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		started := time.Now()
		raw, err := st.run(ctx)
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
	return aiStep{name: name, run: func(ctx context.Context) (string, error) {
		res, err := gc.ChatJSON(ctx, req)
		if err != nil {
			return "", err
		}
		return res.Content, nil
	}}
}

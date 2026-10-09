package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
)

func TestRunStepsFallsThrough(t *testing.T) {
	var order []string
	mk := func(name, reply string, err error) aiStep {
		return aiStep{name: name, run: func(context.Context) (string, error) {
			order = append(order, name)
			return reply, err
		}}
	}
	steps := []aiStep{
		mk("groq-a", "", &groq.RateLimitError{Model: "a", Reason: "requests per day"}),
		mk("groq-b", "garbage", nil),
		mk("deepseek", "good", nil),
		mk("never", "good", nil),
	} // quota skip, invalid reply, success, never reached
	raw, by, err := runSteps(context.Background(), "t", steps, func(s string) error {
		if s != "good" {
			return errors.New("bad")
		}
		return nil
	})
	if err != nil || raw != "good" || by != "deepseek" {
		t.Fatalf("unexpected: %q %q %v", raw, by, err)
	}
	if strings.Join(order, ",") != "groq-a,groq-b,deepseek" {
		t.Fatalf("wrong order: %v", order)
	}
}

func TestRunStepsAllFail(t *testing.T) {
	_, _, err := runSteps(context.Background(), "t", []aiStep{
		{name: "x", run: func(context.Context) (string, error) { return "", errors.New("boom") }},
	}, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected joined error, got %v", err)
	}
	if _, _, err := runSteps(context.Background(), "t", nil, nil); err == nil {
		t.Fatal("no providers must be an error")
	}
}

// TestGenerationRoute: every generation (chain, personal, retry, topic
// batch, repair) is ONE step — DeepSeek flash, thinking, effort high.
// No Groq, no lower effort, no other model.
func TestGenerationRoute(t *testing.T) {
	g := &GeneratorService{ds: deepseek.New("k", "")}
	msgs := func() []deepseek.Message { return []deepseek.Message{{Role: "user", Content: "x"}} }
	want := "deepseek/deepseek-flash(thinking-high)"
	for _, r := range []string{
		stepNames(g.genSteps(msgs, genMaxTokens, 0, true)),
		stepNames(g.genSteps(msgs, genBatchMaxTokens, deepseekBatchTimeout, true)),
		stepNames(g.repairSteps(msgs())),
	} {
		if r != want {
			t.Fatalf("generation route: got %s, want %s", r, want)
		}
	}
	// Without DEEPSEEK_API_KEY there is no generation at all.
	if r := (&GeneratorService{}).genSteps(msgs, genMaxTokens, 0, true); len(r) != 0 {
		t.Fatalf("no DeepSeek client must mean no generation steps: %d", len(r))
	}
}

// TestGenerationRequestsAreThinkingHigh checks the wire format of every
// kind of generation call (chain, personal, retry, batch, repair).
func TestGenerationRequestsAreThinkingHigh(t *testing.T) {
	var mu sync.Mutex
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		reqs = append(reqs, req)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	g := NewGeneratorService(deepseek.New("k", srv.URL), &config.Config{}, nil, nil, nil)
	msgs := func() []deepseek.Message { return []deepseek.Message{{Role: "user", Content: "x"}} }
	routes := [][]aiStep{
		g.genSteps(msgs, genMaxTokens, 0, true),
		g.genSteps(msgs, genBatchMaxTokens, deepseekBatchTimeout, true),
		g.repairSteps(msgs()),
	}
	for _, steps := range routes {
		if _, _, err := runSteps(context.Background(), "t", steps, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if len(reqs) != len(routes) {
		t.Fatalf("want %d calls, got %d", len(routes), len(reqs))
	}
	for _, r := range reqs {
		th, _ := r["thinking"].(map[string]any)
		if r["model"] != "deepseek-flash" || th["type"] != "enabled" || r["reasoning_effort"] != "high" {
			t.Fatalf("generation call must be flash thinking(high): %v", r)
		}
	}
}

// TestTranslationRoute: Qwen (instruct) first, then DeepSeek flash
// non-thinking — nothing else.
func TestTranslationRoute(t *testing.T) {
	tr := &TranslatorService{gq: groq.New("k", ""), ds: deepseek.New("k", "")}
	msgs := []deepseek.Message{{Role: "user", Content: "x"}}
	if r := stepNames(tr.translationSteps(msgs, 1000)); r != "groq/qwen/qwen3.8-27b(none),deepseek/deepseek-flash(non-thinking)" {
		t.Fatalf("translation route: %s", r)
	}
	tr.gq = nil
	if r := stepNames(tr.translationSteps(msgs, 1000)); r != "deepseek/deepseek-flash(non-thinking)" {
		t.Fatalf("translation route without Groq: %s", r)
	}
}

// TestTranslationFallbackIsNonThinking: when Qwen is out of quota the
// chunk goes to DeepSeek flash with thinking DISABLED and a tight
// max_tokens (estimate + 33%, far below the generation caps).
func TestTranslationFallbackIsNonThinking(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	tr := &TranslatorService{ds: deepseek.New("k", srv.URL)}
	quotaGone := aiStep{name: "groq/qwen", run: func(context.Context) (string, error) {
		return "", &groq.RateLimitError{Model: groq.ModelQwen27B, Reason: "requests per day"}
	}}
	steps := append([]aiStep{quotaGone}, tr.translationSteps([]deepseek.Message{{Role: "user", Content: "x"}}, 900)...)
	_, by, err := runSteps(context.Background(), "t", steps, func(string) error { return nil })
	if err != nil || by != "deepseek/deepseek-flash(non-thinking)" {
		t.Fatalf("fallback: %q %v", by, err)
	}
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != "disabled" || got["reasoning_effort"] != nil {
		t.Fatalf("translation fallback must be non-thinking: %v", got)
	}
	if mt := got["max_tokens"].(float64); mt != 1200 {
		t.Fatalf("translation fallback max_tokens = %v, want 1200", mt)
	}
}

func sampleQuestions(n int) []models.Question {
	qs := make([]models.Question, n)
	for i := range qs {
		qs[i] = models.Question{
			ID:      int64(i + 100),
			Text:    fmt.Sprintf("Вопрос %d: какой органоид клетки отвечает за синтез белка на рибосомах эндоплазматической сети?", i),
			OptionA: "Митохондрия", OptionB: "Рибосома", OptionC: "Аппарат Гольджи", OptionD: "Лизосома",
			CorrectAnswer: "B", Topic: "Клетка",
		}
	}
	return qs
}

// TestChunkingFitsFreeTier: every translation chunk must fit ONE Groq
// request (prompt + expected reply < TPM), no question may be lost or
// duplicated, and a normal 20-question test needs only a few chunks.
func TestChunkingFitsFreeTier(t *testing.T) {
	qs := sampleQuestions(20)
	chunks := chunkForTranslation(qs)
	total := 0
	for _, ch := range chunks {
		total += len(ch)
		if !fitsOneGroqRequest(ch) {
			t.Fatalf("chunk of %d questions does not fit a Groq request", len(ch))
		}
	}
	if total != 20 {
		t.Fatalf("questions lost in chunking: %d", total)
	}
	if len(chunks) > 3 {
		t.Fatalf("too many chunks (%d) — wastes RPM/RPD", len(chunks))
	}
	// An oversized question still gets its own chunk (DeepSeek handles it).
	big := sampleQuestions(1)
	big[0].Text = strings.Repeat("очень длинный вопрос ", 800)
	if c := chunkForTranslation(big); len(c) != 1 || len(c[0]) != 1 {
		t.Fatalf("oversized question must form a single chunk: %v", len(c))
	}
}

func TestBuildTranslationsValidation(t *testing.T) {
	qs := sampleQuestions(2)
	master := map[int]*models.Question{1: &qs[0], 2: &qs[1]}
	ok := `{"translations":[{"id":1,"question":"Сұрақ бір","options":["а","б","в","г"],"topic":"Жасуша"},{"id":2,"question":"Сұрақ екі","options":["а","б","в","г"],"topic":"Жасуша"}]}`
	rows, err := buildTranslations(ok, 2, master)
	if err != nil || len(rows) != 2 || rows[0].CorrectAnswer != "B" || rows[1].QuestionID != 101 {
		t.Fatalf("unexpected: %v %+v", err, rows)
	}
	dup := `{"translations":[{"id":1,"question":"Сұрақ бір","options":["а","б","в","г"],"topic":"x"},{"id":1,"question":"Сұрақ бір","options":["а","б","в","г"],"topic":"x"}]}`
	if _, err := buildTranslations(dup, 2, master); err == nil {
		t.Fatal("duplicate ids must be rejected")
	}
	short := `{"translations":[{"id":1,"question":"Сұрақ","options":["а","б","в"],"topic":"x"},{"id":2,"question":"Сұрақ екі","options":["а","б","в","г"],"topic":"x"}]}`
	if _, err := buildTranslations(short, 2, master); err == nil {
		t.Fatal("3 options must be rejected")
	}
}

func stepNames(steps []aiStep) string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.name
	}
	return strings.Join(names, ",")
}

// TestStepTimeout: a step's own timeout bounds only that step.
func TestStepTimeout(t *testing.T) {
	sctx, cancel := stepContext(context.Background(), aiStep{timeout: time.Minute})
	defer cancel()
	if dl, ok := sctx.Deadline(); !ok || time.Until(dl) > time.Minute {
		t.Fatal("step timeout not applied")
	}
	c, cc := stepContext(context.Background(), aiStep{})
	defer cc()
	if _, has := c.Deadline(); has {
		t.Fatal("no timeout must mean no deadline")
	}
}

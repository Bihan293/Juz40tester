package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
	}
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

// TestGenerationRoute pins the cost order: free Groq models first, paid
// DeepSeek strictly last; Qwen runs in instruct mode.
func TestGenerationRoute(t *testing.T) {
	g := &GeneratorService{gq: groq.New("k", ""), ds: deepseek.New("k", "", "", "")}
	msgs := []deepseek.Message{{Role: "user", Content: "x"}}

	chain := stepNames(g.generationSteps(msgs, models.TestKindChain, 1))
	want := "groq/openai/gpt-oss-120b(medium),groq/openai/gpt-oss-120b(low),groq/qwen/qwen3.8-27b(none),deepseek/deepseek-flash(high)"
	if chain != want {
		t.Fatalf("chain route:\n got %s\nwant %s", chain, want)
	}
	personal := stepNames(g.generationSteps(msgs, models.TestKindPersonal, 1))
	if personal != "groq/openai/gpt-oss-120b(low),groq/qwen/qwen3.8-27b(none),deepseek/deepseek-flash(low)" {
		t.Fatalf("personal route: %s", personal)
	}
	// Without Groq the old behaviour (DeepSeek only) is preserved.
	g.gq = nil
	if r := stepNames(g.generationSteps(msgs, models.TestKindChain, 2)); r != "deepseek/deepseek-flash(low)" {
		t.Fatalf("deepseek-only route: %s", r)
	}
}

func TestGenerationPromptFitsGroq(t *testing.T) {
	// A worst-case chain prompt (20 long previous stems) must leave enough
	// output budget for a full 20-question test on the free tier.
	prev := make([]models.Question, 20)
	marks := make([]int, 20)
	for i := range prev {
		prev[i] = models.Question{Topic: "Молекулярная генетика", Text: strings.Repeat("Какой процесс происходит ", 10)}
	}
	msgs := toGroqMessages([]deepseek.Message{
		{Role: "system", Content: genSystemPrompt},
		{Role: "user", Content: chainGenPrompt("Биология", 5, prev, marks)},
	})
	for _, m := range []string{groq.ModelGPTOSS120B, groq.ModelQwen27B} {
		if b := groq.Budget(m, msgs); b < groqGenMinTokens {
			t.Fatalf("%s: only %d output tokens left, need %d", m, b, groqGenMinTokens)
		}
	}
}

func TestTranslationRoute(t *testing.T) {
	tr := &TranslatorService{gq: groq.New("k", ""), ds: deepseek.New("k", "", "", "")}
	r := stepNames(tr.translationSteps([]deepseek.Message{{Role: "user", Content: "x"}}, 1000))
	if r != "groq/qwen/qwen3.8-27b(none),groq/openai/gpt-oss-120b(low),deepseek/deepseek-flash(translate)" {
		t.Fatalf("translation route: %s", r)
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

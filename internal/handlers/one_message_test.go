package handlers

import (
	"strings"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

func headerView() *services.QuestionView {
	return &services.QuestionView{
		Attempt:      &models.TestAttempt{ID: 1, TestID: 9, Status: models.AttemptInProgress},
		AttemptQ:     &models.AttemptQuestion{Position: 3, OptionOrder: []string{"C", "A", "D", "B"}},
		Question:     &models.Question{CorrectAnswer: "B"},
		Text:         "Столица Казахстана?",
		Total:        20,
		DisplayTexts: []string{"Алматы", "Шымкент", "Караганда", "Астана"},
	}
}

func TestVerdictToastAndHeader(t *testing.T) {
	v := headerView()
	ok := &repositories.AnswerResult{Correct: true}
	bad := &repositories.AnswerResult{Correct: false, SelectedAnswer: "C"}
	if got := verdictToast(ok); got != "🟢 Правильно" {
		t.Fatalf("toast ok = %q", got)
	}
	if got := verdictToast(bad); got != "🔴 Неправильно" {
		t.Fatalf("toast bad = %q", got)
	}
	if got := answerHeader(v, ok); got != "🟢 Правильно" {
		t.Fatalf("header ok = %q", got)
	}
	// The correct original option B is displayed 4th -> D) Астана.
	if got, want := answerHeader(v, bad), "🔴 Неправильно — правильный ответ: D) Астана"; got != want {
		t.Fatalf("header bad = %q, want %q", got, want)
	}
	// A very long correct option is shortened in the header.
	v.DisplayTexts[3] = strings.Repeat("я", 1000)
	if got := answerHeader(v, bad); len([]rune(got)) > 400 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long header not shortened: %d runes", len([]rune(got)))
	}
}

func TestWithHeaderFitsTelegramLimit(t *testing.T) {
	if got := withHeader("🟢 Правильно", "❓ Вопрос 2/20"); got != "🟢 Правильно\n\n❓ Вопрос 2/20" {
		t.Fatalf("withHeader = %q", got)
	}
	if got := withHeader("", "body"); got != "body" {
		t.Fatalf("empty header = %q", got)
	}
	long := strings.Repeat("😀", 1990) // 3980 UTF-16 units
	hdr := "🔴 Неправильно — правильный ответ: D) " + strings.Repeat("x", 300)
	got := withHeader(hdr, long)
	if utf16Len(got) > 4096 {
		t.Fatalf("text too long: %d", utf16Len(got))
	}
	if !strings.HasPrefix(got, "🔴 Неправильно\n\n") {
		t.Fatalf("header must shrink to the bare verdict, got prefix %q", got[:40])
	}
	huge := strings.Repeat("😀", 5000)
	got = withHeader(hdr, huge)
	if n := utf16Len(got); n > 4096 {
		t.Fatalf("huge body not truncated: %d", n)
	}
}

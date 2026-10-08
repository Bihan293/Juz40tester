package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

func adminView() *services.QuestionView {
	return &services.QuestionView{
		Attempt:      &models.TestAttempt{ID: 5},
		AttemptQ:     &models.AttemptQuestion{Position: 1, OptionOrder: []string{"C", "A", "D", "B"}},
		Question:     &models.Question{CorrectAnswer: "A"},
		Text:         "2+2?",
		Total:        20,
		DisplayTexts: []string{"5", "4", "3", "6"},
	}
}

// The correct option is revealed to administrators only, and only in the
// rendering: the callback data of the buttons is identical.
func TestAdminSeesCorrectAnswer(t *testing.T) {
	v := adminView()
	user := renderQuestionFor(v, false)
	admin := renderQuestionFor(v, true)
	if strings.Contains(user, "✅") || strings.Contains(user, "правильный") {
		t.Fatalf("a user sees the answer:\n%s", user)
	}
	if !strings.Contains(admin, "B) 4 ✅") || !strings.Contains(admin, "правильный ответ — B") || strings.Count(admin, "✅") != 1 {
		t.Fatalf("admin rendering:\n%s", admin)
	}
	ku, ka := questionKeyboardFor(v, false), questionKeyboardFor(v, true)
	for i := range ku.InlineKeyboard {
		if ku.InlineKeyboard[i][0].CallbackData != ka.InlineKeyboard[i][0].CallbackData {
			t.Fatal("admin buttons must answer exactly like user buttons")
		}
		if strings.Contains(ku.InlineKeyboard[i][0].Text, "✅") {
			t.Fatal("user button marked")
		}
	}
	if !strings.HasPrefix(ka.InlineKeyboard[1][0].Text, "✅ B)") {
		t.Fatalf("admin button: %q", ka.InlineKeyboard[1][0].Text)
	}
	if renderQuestion(v) != user {
		t.Fatal("renderQuestion must stay the user rendering")
	}
	// Unknown correct answer: nothing is marked, nothing breaks.
	v.Question.CorrectAnswer = ""
	if strings.Contains(renderQuestionFor(v, true), "✅") {
		t.Fatal("marked without a correct answer")
	}
}

func TestAdminMenuButtonOnlyForAdmins(t *testing.T) {
	h := New(nil, nil, nil).WithAdmins(func(id int64) bool { return id == 42 }).
		WithAdminPanel(services.NewAdminService(nil, nil, func(id int64) bool { return id == 42 }, nil), nil)
	if !strings.Contains(fmt.Sprint(h.menuKeyboard(42)), kbAdmin) {
		t.Fatal("admin has no admin button")
	}
	if strings.Contains(fmt.Sprint(h.menuKeyboard(43)), kbAdmin) {
		t.Fatal("a user sees the admin button")
	}
	// Without the panel nobody sees it.
	h2 := New(nil, nil, nil).WithAdmins(func(int64) bool { return true })
	if strings.Contains(fmt.Sprint(h2.menuKeyboard(42)), kbAdmin) {
		t.Fatal("admin button without the panel")
	}
}

func TestParsePlanArgs(t *testing.T) {
	if id, p, d, ok := parsePlanArgs("12:pro:30", true); !ok || id != 12 || p != "pro" || d != 30 {
		t.Fatal(id, p, d, ok)
	}
	if id, p, _, ok := parsePlanArgs("12:plus", false); !ok || id != 12 || p != "plus" {
		t.Fatal(id, p, ok)
	}
	for _, bad := range []string{"", "x:pro:30", "12::30", "12:pro:-1", "12:pro:99999", "12:pro"} {
		if _, _, _, ok := parsePlanArgs(bad, true); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	// Every admin callback fits Telegram's 64-byte limit.
	long := fmt.Sprintf("%s%d:%s:%d", cbAdmPlanApply, int64(9_223_372_036_854_775_807), "premium", 365)
	if len(long) > 64 {
		t.Fatalf("callback too long: %d", len(long))
	}
}

func TestRenderBroadcastCard(t *testing.T) {
	start := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Second)
	b := &repositories.Broadcast{ID: 3, Status: repositories.BroadcastDone, Text: "Привет", MediaType: "photo",
		Buttons: [][]repositories.BroadcastButton{{{Text: "a", URL: "https://a"}}}, StartedAt: &start, FinishedAt: &end, CreatedAt: start}
	s := renderBroadcastCard(b, repositories.BroadcastCounts{Total: 10, Sent: 7, Blocked: 2, Failed: 1}, time.UTC, end)
	for _, want := range []string{"#3 — завершена", "фото + подпись, кнопок: 1", "Получателей: 10", "Отправлено: 7", "Заблокировали бота: 2", "Ошибки: 1", "Время: 1 мин"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	b.Status, b.FinishedAt = repositories.BroadcastSending, nil
	s = renderBroadcastCard(b, repositories.BroadcastCounts{Total: 10, Sent: 5, Pending: 5}, time.UTC, end)
	if !strings.Contains(s, "Осталось: 5") || !strings.Contains(s, "осталось ≈") {
		t.Errorf("progress:\n%s", s)
	}
}

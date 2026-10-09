package handlers

import (
	"context"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

func TestCallbackAllowedInTest(t *testing.T) {
	at := &repositories.ActiveTest{AttemptID: 42, TestID: 7}
	cases := map[string]bool{
		cbNoop:                        true,
		cbAnswer + "42:3:1":           true,
		cbAnswer + "41:3:1":           false, // another (paused) attempt
		cbAnswer + "x:1:1":            false,
		cbAnswer + "42":               false,
		cbExit + "42":                 true,
		cbExitYes + "42":              true,
		cbExitNo + "42":               true,
		cbExit + "41":                 false,
		cbExitYes + "41":              false,
		cbOpenTest + "7":              true, // resume the same test
		cbOpenTest + "8":              false,
		cbRetry + "42":                false,
		cbSubjects:                    false,
		cbSubject + "1":               false,
		cbWeakMenu:                    false,
		cbWeakSubject + "1":           false,
		cbCustomMenu:                  false,
		cbMainMenu:                    false,
		cbPlans:                       false,
		cbBuyPlan + "plus":            false,
		cbAdmPrefix + "menu":          false,
		cbFinish + "7":                false,
		cbPendingID + "1:2":           false,
		cbSetLang + models.TestLangKK: false,
	}
	for data, want := range cases {
		if got := callbackAllowedInTest(data, at); got != want {
			t.Errorf("%q: allowed = %v, want %v", data, got, want)
		}
	}
}

func TestActiveTestLabel(t *testing.T) {
	cases := []struct {
		at   repositories.ActiveTest
		want string
	}{
		{repositories.ActiveTest{TestKind: models.TestKindChain, Title: "Тест 3", SubjectName: "Математика"}, "Тест 3 · Математика"},
		{repositories.ActiveTest{TestKind: models.TestKindChain, TestNumber: 5}, "Тест 5"},
		{repositories.ActiveTest{TestKind: models.TestKindPersonal, Title: "x", SubjectName: "Химия"}, "🎯 Слабые темы · Химия"},
		{repositories.ActiveTest{TestKind: models.TestKindCustom, Title: "✨ Дроби", SubjectName: "Математика"}, "✨ Свой тест · Математика"},
	}
	for _, c := range cases {
		if got := activeTestLabel(&c.at); got != c.want {
			t.Errorf("%+v → %q, want %q", c.at, got, c.want)
		}
	}
}

func TestTestModeKeyboard(t *testing.T) {
	kb := testModeKeyboard()
	if len(kb.Keyboard) != 1 || len(kb.Keyboard[0]) != 1 || kb.Keyboard[0][0].Text != kbExitTest || !kb.ResizeKeyboard {
		t.Fatalf("test-mode keyboard: %+v", kb)
	}
	g := activeGuardKeyboard(&repositories.ActiveTest{AttemptID: 9, TestID: 3})
	if g.InlineKeyboard[0][0].CallbackData != cbOpenTest+"3" || g.InlineKeyboard[1][0].CallbackData != cbExit+"9" {
		t.Fatalf("guard keyboard: %+v", g)
	}
	for _, c := range []string{"/menu", "/exit", kbExitTest} {
		if !isMenuCommand(c) {
			t.Errorf("%q must leave the «Свой тест» description step", c)
		}
	}
	if !isMenuButton(kbCustom) {
		t.Error("«✨ Свой тест» is a main-menu button")
	}
	if !adminTextCommand("/grant") || adminTextCommand("/admin") || adminTextCommand("/start") {
		t.Error("admin text commands")
	}
}

// Without the users repository (bare unit-test handlers) test mode is off:
// nothing is blocked, nothing is queried.
func TestTestModeOffWithoutUsers(t *testing.T) {
	h := &Handler{}
	u := &models.User{ID: 1, ActiveAttemptID: 5}
	if ok, notice := h.enterTest(context.Background(), 1, u, 6); !ok || notice {
		t.Fatal("enterTest without users must pass")
	}
	if h.activeTest(context.Background(), u) != nil {
		t.Fatal("activeTest without users")
	}
	h.leaveTest(context.Background(), 1, u, 5) // no panic, no call
}

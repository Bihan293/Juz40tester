package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// The result screen must announce only what the completion really did: a
// retry of an earlier (long open) test opens nothing new, and «я уже начал
// его собирать» is said only while the next test is actually generating.
func TestSummaryAnnouncesOnlyRealUnlock(t *testing.T) {
	sum := &services.AttemptSummary{
		Attempt: &models.TestAttempt{CorrectCount: 20},
		Test:    &models.Test{ID: 5, SubjectID: 3, TestNumber: 4, Kind: models.TestKindChain},
		Total:   20,
		StatusCounts: map[int]int{
			models.StatusMastered: 16, models.StatusPartial: 4, models.StatusNone: 0,
		},
	}
	cases := []struct {
		name    string
		outcome services.CompletionOutcome
		want    string
		mustNot []string
	}{
		{"new unlock, generating", services.CompletionOutcome{NewUnlock: true, NextGenerating: true},
			"🔓 Ты открыл «Тест 5»! Я уже начал его собирать", nil},
		{"new unlock, test exists", services.CompletionOutcome{NewUnlock: true},
			"🔓 Ты открыл «Тест 5»!", []string{"начал его собирать"}},
		{"retry of an open test", services.CompletionOutcome{},
			"➡️", []string{"🔓", "начал его собирать"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &kbFake{}
			h := New(bot.NewClient("T").WithBaseURL(f.server(t).URL), nil, nil)
			h.renderSummary(context.Background(), sum, &models.User{ID: 1}, 77, 42, "", c.outcome)
			if len(f.sent) == 0 {
				t.Fatal("no result message sent")
			}
			text := f.sent[0]
			if c.want != "➡️" && !strings.Contains(text, c.want) {
				t.Fatalf("result text %q does not contain %q", text, c.want)
			}
			for _, bad := range c.mustNot {
				if strings.Contains(text, bad) {
					t.Fatalf("result text %q must not contain %q", text, bad)
				}
			}
		})
	}
}

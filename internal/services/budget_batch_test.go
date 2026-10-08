package services

import (
	"context"
	"errors"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// capBudget is a DeepSeek ledger whose daily cap is always reached.
type capBudget struct{}

func (capBudget) Reserve(context.Context, float64) error { return deepseek.ErrBudgetExceeded }
func (capBudget) Settle(context.Context, float64, float64) {}

// The batch strategy must surface the daily DeepSeek cap (wrapped
// ErrBudgetExceeded) so executeJob defers the job instead of burning one
// of its attempts — exactly like the full strategy.
func TestGenerateBatchedSurfacesBudgetExceeded(t *testing.T) {
	ds := deepseek.New("test-key", "m", "r", "http://127.0.0.1:1").WithBudget(capBudget{}, nil)
	g := &GeneratorService{ds: ds}
	spec := &genSpec{
		kind:           models.TestKindPersonal,
		subjectName:    "Биология",
		personalTopics: []string{"Генетика"},
		slots:          planPersonalSlots([]string{"Генетика"}, 4, 2),
	}
	job := &models.GenerationJob{Kind: models.TestKindPersonal, Attempts: 1}
	_, err := g.generateBatched(context.Background(), job, spec)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, deepseek.ErrBudgetExceeded) {
		t.Fatalf("batch error must wrap ErrBudgetExceeded, got %v", err)
	}
}

// A repair that could not run because of the daily cap must also be
// reported as ErrBudgetExceeded (deferral, not a failed attempt).
func TestRepairFlaggedSurfacesBudgetExceeded(t *testing.T) {
	ds := deepseek.New("test-key", "m", "r", "http://127.0.0.1:1").WithBudget(capBudget{}, nil)
	g := &GeneratorService{ds: ds}
	qs := validQuestions()
	// A giveaway: two identical options (hard quality issue).
	qs[0].Options = []string{"Митоз", "Митоз", "Мейоз", "Амитоз"}
	gt := &generatedTest{Questions: qs}
	if countHard(auditGenerated(gt)) == 0 {
		t.Fatal("test setup: question 1 must be hard-flagged")
	}
	err := g.repairFlagged(context.Background(), "t", "Биология", gt)
	if err == nil || !errors.Is(err, deepseek.ErrBudgetExceeded) {
		t.Fatalf("repair error must wrap ErrBudgetExceeded, got %v", err)
	}
}

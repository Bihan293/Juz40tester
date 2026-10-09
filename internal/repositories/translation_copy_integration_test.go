package repositories

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/testutil"
)

// TestCopyTranslationsSameStemDifferentOptions: two questions of a test
// share the generic stem but have different options. The cached Kazakh
// translations carried to a clone must pair by the WHOLE question (text +
// options + correct letter), never by the text alone — otherwise the
// Kazakh options of one question appear under the other and the correct
// answer the student sees is wrong.
func TestCopyTranslationsSameStemDifferentOptions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	gen := NewGenerationRepository(pool)
	subjects := NewSubjectRepository(pool)
	tr := NewTranslationRepository(pool)
	sid, err := testutil.CreateSubject(ctx, pool, "TRCOPY "+time.Now().Format("150405.000000"))
	if err != nil {
		t.Fatal(err)
	}
	stem := fmt.Sprintf("Какое утверждение верно? %d", sid)
	qs := []models.SeedQuestion{
		{Text: stem, Options: [4]string{"Земля плоская", "Вода кипит при 100°C", "Луна — звезда", "Солнце — планета"}, Correct: 1, Topic: "Т", Difficulty: 1},
		{Text: stem, Options: [4]string{"Кит — рыба", "Паук — насекомое", "Дельфин — млекопитающее", "Летучая мышь — птица"}, Correct: 2, Topic: "Т", Difficulty: 1},
	}
	mk := func(n int) (int64, []int64, []models.Question) {
		test, err := gen.CreateGeneratedTest(ctx, &models.Test{SubjectID: sid, TestNumber: n, Title: "T", Kind: models.TestKindChain}, qs)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := subjects.TestQuestionIDs(ctx, test.ID)
		if err != nil {
			t.Fatal(err)
		}
		full, err := subjects.TestQuestions(ctx, test.ID)
		if err != nil {
			t.Fatal(err)
		}
		return test.ID, ids, full
	}
	srcID, _, srcQs := mk(1)
	_, dstIDs, dstQs := mk(2)

	// The Kazakh options of each source question are tagged with the
	// source option texts, so a mismatch is visible.
	var trs []models.QuestionTranslation
	correct := map[int64]string{}
	for _, q := range srcQs {
		trs = append(trs, models.QuestionTranslation{QuestionID: q.ID, Lang: models.TestLangKK, Text: "KK " + q.Text,
			OptionA: "KK " + q.OptionA, OptionB: "KK " + q.OptionB, OptionC: "KK " + q.OptionC, OptionD: "KK " + q.OptionD})
		correct[q.ID] = q.CorrectAnswer
	}
	if err := tr.SaveTranslations(ctx, trs, correct); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.CopyTranslationsToTest(ctx, srcID, dstIDs, models.TestLangKK); err != nil {
		t.Fatal(err)
	}
	for _, q := range dstQs {
		var a, b, c, d string
		if err := pool.QueryRow(ctx, `SELECT option_a, option_b, option_c, option_d FROM question_translations
			WHERE question_id = $1 AND lang = $2`, q.ID, models.TestLangKK).Scan(&a, &b, &c, &d); err != nil {
			t.Fatalf("question %d: no translation copied: %v", q.ID, err)
		}
		if a != "KK "+q.OptionA || b != "KK "+q.OptionB || c != "KK "+q.OptionC || d != "KK "+q.OptionD {
			t.Fatalf("question %d got the options of another question: %q %q %q %q (master %q)", q.ID, a, b, c, d, q.OptionA)
		}
	}
}

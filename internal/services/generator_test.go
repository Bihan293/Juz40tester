package services

import (
	"strings"
	"testing"

	"github.com/Bihan293/Juz40-test2/internal/models"
)

func validQuestions() []generatedQuestion {
	qs := make([]generatedQuestion, 0, GeneratedQuestionsPerTest)
	keys := []string{"А", "Б", "В", "Г"}
	for i := 0; i < GeneratedQuestionsPerTest; i++ {
		qs = append(qs, generatedQuestion{
			Text: strings.Repeat("в", 8) + " вопрос " + keys[i%4] + strings.Repeat("о", i%7),
			Options: []string{
				"вариант раз " + keys[i%4], "вариант два " + keys[i%4],
				"вариант три " + keys[i%4], "вариант четыре " + keys[i%4],
			},
			Correct:    i % 4, // balanced 5/5/5/5
			Topic:      "Тема",
			Difficulty: 2,
		})
	}
	return qs
}

func TestValidateTestOK(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	if err := validateTest(gt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTestWrongCount(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()[:19]}
	if err := validateTest(gt); err == nil {
		t.Fatal("expected error for 19 questions")
	}
}

func TestValidateTestBadIndex(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	gt.Questions[3].Correct = 7
	if err := validateTest(gt); err == nil {
		t.Fatal("expected error for correct_index out of range")
	}
}

func TestValidateTestDuplicate(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	gt.Questions[5].Text = gt.Questions[4].Text
	if err := validateTest(gt); err == nil {
		t.Fatal("expected error for duplicate question text")
	}
}

func TestValidateTestUnbalancedKeys(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	for i := range gt.Questions {
		gt.Questions[i].Correct = 0
	}
	if err := validateTest(gt); err == nil {
		t.Fatal("expected error for degenerate key distribution")
	}
}

func TestParseTestJSONToleratesFences(t *testing.T) {
	raw := "Вот тест:\n```json\n{\"questions\": []}\n```"
	_, err := parseTestJSON(raw)
	// Extraction succeeds, validation fails on the count — both prove the
	// fence stripping worked.
	if err == nil || !strings.Contains(err.Error(), "need") {
		t.Fatalf("expected a count validation error, got: %v", err)
	}
}

func TestDifficultyBand(t *testing.T) {
	// The ramp must stay gentle: school level for the first dozen tests.
	cases := map[int]string{1: "1–2", 2: "1–2", 3: "2–3", 4: "2–3", 5: "3", 7: "3", 8: "3–4", 12: "3–4", 13: "4–5", 50: "4–5"}
	for n, want := range cases {
		if got := difficultyBand(n); got != want {
			t.Fatalf("difficultyBand(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestChainGenPromptCompact(t *testing.T) {
	// Cost guard: the prompt for the next chain test must stay compact even
	// with a full 20-question previous test (long stems are truncated and no
	// answer options are included).
	prev := make([]models.Question, 0, GeneratedQuestionsPerTest)
	for i := 0; i < GeneratedQuestionsPerTest; i++ {
		prev = append(prev, models.Question{
			Text: strings.Repeat("длинный текст вопроса ", 20), Topic: "Тема",
		})
	}
	marks := make([]int, GeneratedQuestionsPerTest)
	for i := range marks {
		marks[i] = i % 3 // 0,1,2 knowledge levels
	}
	p := chainGenPrompt("Биология", 2, prev, marks)
	if len(p) > 6000 {
		t.Fatalf("chain prompt too large: %d bytes", len(p))
	}
	if !strings.Contains(p, "уровень ученика") {
		t.Fatal("chain prompt must include the knowledge marks legend")
	}
	first := chainGenPrompt("Биология", 1, nil, nil)
	if !strings.Contains(first, "ПЕРВЫЙ тест") {
		t.Fatal("first-test prompt must describe the base level")
	}
}

func TestTopicsFingerprintSharing(t *testing.T) {
	// The whole point of the fingerprint: the SAME weak-topic set must map
	// to the SAME fingerprint regardless of order, case or whitespace —
	// two users with identical weakness profiles then share one generated
	// test (a clone, zero AI cost for the second one).
	a := topicsFingerprint([]string{"Генетика", "Микроорганизмы", "Обмен веществ"})
	b := topicsFingerprint([]string{"микроорганизмы", " Обмен   веществ ", "генетика"})
	if a == "" || a != b {
		t.Fatalf("fingerprints must match for the same topic set: %q vs %q", a, b)
	}
	c := topicsFingerprint([]string{"Генетика", "Микроорганизмы"})
	if c == a {
		t.Fatal("different topic sets must produce different fingerprints")
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex must be 64 chars, got %d", len(a))
	}
}

func TestValidatePersonalCoverage(t *testing.T) {
	topics := []string{"Генетика", "Микроорганизмы"}

	// OK: every question tagged with a requested topic, all topics covered
	// (case/whitespace differences are tolerated).
	ok := &generatedTest{Questions: []generatedQuestion{
		{Topic: "Генетика"}, {Topic: " генетика "}, {Topic: "МИКРООРГАНИЗМЫ"},
	}}
	if err := validatePersonalCoverage(ok, topics); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A topic outside the requested list is rejected (never train mastered
	// topics — that wastes money).
	foreign := &generatedTest{Questions: []generatedQuestion{
		{Topic: "Генетика"}, {Topic: "Ботаника"},
	}}
	if err := validatePersonalCoverage(foreign, topics); err == nil {
		t.Fatal("expected error for a topic outside the weak-topics list")
	}

	// Missing coverage of a requested topic is rejected.
	missing := &generatedTest{Questions: []generatedQuestion{
		{Topic: "Генетика"}, {Topic: "Генетика"},
	}}
	if err := validatePersonalCoverage(missing, topics); err == nil {
		t.Fatal("expected error when a weak topic is not covered")
	}
}

func TestPersonalGenPromptSendsOnlyTopics(t *testing.T) {
	// Cost guard: the weak-topics prompt must contain ONLY the short topic
	// names — never past tests / question payloads (those burned tokens).
	p := personalGenPrompt("Биология", []string{"Генетика", "Микроорганизмы"})
	if !strings.Contains(p, "1. Генетика") || !strings.Contains(p, "2. Микроорганизмы") {
		t.Fatal("prompt must list the weak topics")
	}
	if !strings.Contains(p, "ДОСЛОВНО") {
		t.Fatal("prompt must require verbatim topic tags")
	}
	if len(p) > 1500 {
		t.Fatalf("personal prompt too large: %d bytes", len(p))
	}
}

func TestUnlockRuleConstants(t *testing.T) {
	// Guard the product rule: 15 green + 5 yellow to unlock the next test.
	if models.UnlockGreen != 15 || models.UnlockYellow != 5 {
		t.Fatalf("unlock rule changed: %d green / %d yellow", models.UnlockGreen, models.UnlockYellow)
	}
	if models.TestsPerPage%models.TestsGridColumns != 0 {
		t.Fatal("tests per page must fill full grid rows")
	}
	if models.MaxVisibleTests != 50 {
		t.Fatalf("max visible tests = %d, want 50", models.MaxVisibleTests)
	}
}

package services

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/models"
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

func TestChainDifficultyCurve(t *testing.T) {
	// Product rule: test 1 is really easy, 10 noticeably harder, 30 is a
	// solid ЕНТ level, 100+ are hard. The curve never goes down and every
	// single step is tiny (no sudden walls between neighbouring tests).
	checks := []struct {
		n      int
		lo, hi float64
	}{
		{1, 1.0, 1.5}, {10, 2.4, 2.8}, {30, 3.3, 3.7}, {100, 4.2, 4.6}, {150, 4.6, 5.0}, {200, 4.6, 5.0},
	}
	for _, c := range checks {
		if got := chainDifficultyTarget(c.n); got < c.lo || got > c.hi {
			t.Fatalf("target(%d) = %.1f, want %.1f..%.1f", c.n, got, c.lo, c.hi)
		}
	}
	prev := chainDifficultyTarget(1)
	for n := 2; n <= 250; n++ {
		cur := chainDifficultyTarget(n)
		if cur < prev {
			t.Fatalf("difficulty went DOWN at test %d: %.1f -> %.1f", n, prev, cur)
		}
		if cur-prev > 0.25 {
			t.Fatalf("difficulty jump at test %d: %.1f -> %.1f", n, prev, cur)
		}
		prev = cur
	}
	if chainDifficultyTarget(10) <= chainDifficultyTarget(1) ||
		chainDifficultyTarget(30) <= chainDifficultyTarget(10) ||
		chainDifficultyTarget(100) <= chainDifficultyTarget(30) {
		t.Fatal("difficulty must strictly rise 1 < 10 < 30 < 100")
	}
}

func TestDifficultyMixMatchesTarget(t *testing.T) {
	for n := 1; n <= models.MaxVisibleTests; n++ {
		target := chainDifficultyTarget(n)
		mix := difficultyMix(target)
		total, sum := 0, 0
		for lvl, c := range mix {
			if c < 0 {
				t.Fatalf("test %d: negative count in mix %v", n, mix)
			}
			total += c
			sum += c * (lvl + 1)
		}
		if total != GeneratedQuestionsPerTest {
			t.Fatalf("test %d: mix %v has %d questions", n, mix, total)
		}
		if mean := float64(sum) / float64(total); mean < target-0.06 || mean > target+0.06 {
			t.Fatalf("test %d: mix %v mean %.2f, target %.1f", n, mix, mean, target)
		}
	}
}

func TestValidateChainDifficulty(t *testing.T) {
	mk := func(d int) *generatedTest {
		gt := &generatedTest{Questions: validQuestions()}
		for i := range gt.Questions {
			gt.Questions[i].Difficulty = d
		}
		return gt
	}
	if err := validateChainDifficulty(mk(1), 1); err != nil {
		t.Fatalf("easy test 1 must pass: %v", err)
	}
	if err := validateChainDifficulty(mk(1), 40); err == nil {
		t.Fatal("a beginner-level Тест 40 must be rejected")
	}
	if err := validateChainDifficulty(mk(5), 2); err == nil {
		t.Fatal("an olympiad-level Тест 2 must be rejected")
	}
	if err := validateChainDifficulty(mk(4), 100); err != nil {
		t.Fatalf("level-4 Тест 100 must pass: %v", err)
	}
}

func TestChainPromptDifficultyByPosition(t *testing.T) {
	p1 := chainGenPrompt("Биология", 1, nil, nil)
	p30 := chainGenPrompt("Биология", 30, nil, nil)
	if !strings.Contains(p1, "ПЕРВЫЙ тест") {
		t.Fatal("test 1 prompt must describe the easiest level")
	}
	// Previous test missing: the prompt must NOT pretend to be the first
	// (beginner) test — that silently reset deep tests to level 1.
	if strings.Contains(p30, "ПЕРВЫЙ тест") {
		t.Fatal("test 30 without a previous test must not be generated as the first test")
	}
	if !strings.Contains(p30, "≈ 3.5") || !strings.Contains(p1, "≈ 1.3") {
		t.Fatalf("prompts must carry the target mean difficulty:\n%s\n---\n%s", p1, p30)
	}
	if !strings.Contains(p1, "Шкала сложности") {
		t.Fatal("prompt must define the difficulty scale")
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
	marks := make([]float64, GeneratedQuestionsPerTest)
	for i := range marks {
		marks[i] = float64(i%3) + 0.5*float64(i%2) // averaged levels 0..2.5
	}
	marks[0] = -1 // nobody answered
	p := chainGenPrompt("Биология", 2, prev, marks)
	if len(p) > 6000 {
		t.Fatalf("chain prompt too large: %d bytes", len(p))
	}
	if !strings.Contains(p, "уровень учеников") || !strings.Contains(p, "— ?") || !strings.Contains(p, "— 1.5") {
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
	if err := validatePersonalCoverage(ok, topics, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// A topic outside the requested list is rejected (never train mastered
	// topics — that wastes money).
	foreign := &generatedTest{Questions: []generatedQuestion{
		{Topic: "Генетика"}, {Topic: "Ботаника"},
	}}
	if err := validatePersonalCoverage(foreign, topics, nil); err == nil {
		t.Fatal("expected error for a topic outside the weak-topics list")
	}

	// Missing coverage of a requested topic is rejected.
	missing := &generatedTest{Questions: []generatedQuestion{
		{Topic: "Генетика"}, {Topic: "Генетика"},
	}}
	if err := validatePersonalCoverage(missing, topics, nil); err == nil {
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
	if models.MaxVisibleTests < 100 {
		t.Fatalf("max visible tests = %d, the chain must reach the hard 100+ tests", models.MaxVisibleTests)
	}
}

func TestLanguageSubjectRule(t *testing.T) {
	if r := languageSubjectRule("Биология"); r != "" {
		t.Fatalf("regular subject must have no language rule, got %q", r)
	}
	if r := languageSubjectRule("История Казахстана"); r != "" {
		t.Fatalf("История Казахстана is not a language subject, got %q", r)
	}
	en := chainGenPrompt("Английский язык", 1, nil, nil)
	if !strings.Contains(en, "английском") {
		t.Fatal("English subject prompt must require English content")
	}
	kk := personalGenPrompt("Казахский язык", []string{"Етістік"})
	if !strings.Contains(kk, "казахском") {
		t.Fatal("Kazakh subject prompt must require Kazakh content")
	}
	ru := chainGenPrompt("Русский язык", 1, nil, nil)
	if !strings.Contains(ru, "ТОЛЬКО на русском") {
		t.Fatal("Russian subject prompt must pin the Russian language")
	}
}

func TestChainPromptStemTruncationIsValidUTF8(t *testing.T) {
	prev := []models.Question{{Text: strings.Repeat("щ", 200), Topic: "Тема"}}
	p := chainGenPrompt("Биология", 2, prev, []float64{0})
	if !utf8.ValidString(p) {
		t.Fatal("chain prompt must stay valid UTF-8 after truncating stems")
	}
}

func TestWorkerCountDefaultsAndCap(t *testing.T) {
	cases := []struct {
		cfg  *config.Config
		want int
	}{
		{nil, config.DefaultGenWorkers},
		{&config.Config{}, config.DefaultGenWorkers},
		{&config.Config{GenWorkers: 6}, 6},
		{&config.Config{GenWorkers: 1000}, config.MaxGenWorkers},
	}
	for _, c := range cases {
		g := &GeneratorService{cfg: c.cfg}
		if got := g.workerCount(); got != c.want {
			t.Fatalf("workerCount(%+v) = %d, want %d", c.cfg, got, c.want)
		}
	}
}

func TestNotifyWorkersNeverBlocks(t *testing.T) {
	g := NewGeneratorService(nil, &config.Config{}, nil, nil, nil)
	for i := 0; i < 100; i++ {
		g.notifyWorkers() // buffered size 1: extra signals coalesce
	}
	select {
	case <-g.wake:
	default:
		t.Fatal("wake signal lost")
	}
	(&GeneratorService{}).notifyWorkers() // nil channel: no-op
}

func TestDeepSeekSemaphoreBoundsParallelCalls(t *testing.T) {
	g := NewGeneratorService(nil, &config.Config{GenDeepSeekConcurrency: 2}, nil, nil, nil)
	r1, _ := g.acquireDeepSeek(context.Background())
	r2, _ := g.acquireDeepSeek(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := g.acquireDeepSeek(ctx); err == nil {
		t.Fatal("third paid call must wait for a free slot")
	}
	r1()
	r3, err := g.acquireDeepSeek(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2()
	r3()
}

// R-6: questions handed to CreateGeneratedTest by runJob passed the audit.
func TestToSeedMarksQualityChecked(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	for i, sq := range gt.toSeed() {
		if !sq.QualityChecked {
			t.Fatalf("question %d not marked as quality-checked", i)
		}
	}
}

// TestTopicCatalogValidators (B1): aliases are mapped to the requested /
// canonical topic, topics outside the catalog are limited.
func TestTopicCatalogValidators(t *testing.T) {
	cat := &models.TopicCatalog{
		Titles:  []string{"Генетика", "Клетка"},
		Aliases: map[string]string{"генетика": "генетика", "наследственность": "генетика", "клетка": "клетка"},
	}
	// Personal: an alias of a requested topic is accepted and rewritten.
	gt := &generatedTest{Questions: []generatedQuestion{{Topic: "Наследственность"}, {Topic: "Клетка"}}}
	if err := validatePersonalCoverage(gt, []string{"Генетика", "Клетка"}, cat); err != nil {
		t.Fatalf("alias rejected: %v", err)
	}
	if gt.Questions[0].Topic != "Генетика" {
		t.Fatalf("alias not rewritten: %q", gt.Questions[0].Topic)
	}
	// Personal: a catalog topic that was NOT requested is still rejected.
	gt = &generatedTest{Questions: []generatedQuestion{{Topic: "Генетика"}, {Topic: "Клетка"}}}
	if err := validatePersonalCoverage(gt, []string{"Генетика"}, cat); err == nil {
		t.Fatal("non-requested topic accepted")
	}
	// Chain: aliases mapped to canonical titles, <= 2 new topics allowed.
	gt = &generatedTest{Questions: []generatedQuestion{{Topic: "наследственность"}, {Topic: "Новая 1"}, {Topic: "Новая 2"}}}
	if err := validateChainTopics(gt, cat); err != nil {
		t.Fatal(err)
	}
	if gt.Questions[0].Topic != "Генетика" {
		t.Fatalf("chain alias not mapped: %q", gt.Questions[0].Topic)
	}
	gt.Questions = append(gt.Questions, generatedQuestion{Topic: "Новая 3"})
	if err := validateChainTopics(gt, cat); err == nil {
		t.Fatal("too many topics outside the catalog accepted")
	}
	// Empty catalog accepts everything; prompt lists the titles.
	if err := validateChainTopics(gt, &models.TopicCatalog{}); err != nil {
		t.Fatal(err)
	}
	if p := topicListPrompt(cat); !strings.Contains(p, "Генетика; Клетка") {
		t.Fatalf("prompt: %q", p)
	}
}

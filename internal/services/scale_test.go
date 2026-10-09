package services

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
)

func TestSimilarStemsAndRepeats(t *testing.T) {
	if !similarStems("Что такое фотосинтез?", "что такое  фотосинтез") {
		t.Fatal("normalised equality must match")
	}
	if similarStems("Решите уравнение 2x + 3 = 7", "Решите уравнение 2x + 3 = 9") {
		t.Fatal("different numbers are different questions")
	}
	a := "Какой органоид клетки отвечает за синтез белка на рибосомах эндоплазматической сети"
	b := "Какой органоид клетки отвечает за синтез белка на рибосомах шероховатой эндоплазматической сети"
	if !similarStems(a, b) {
		t.Fatal("a cosmetic rewording of a long stem must be a repeat")
	}
	gt := &generatedTest{Questions: validQuestions()}
	if err := validateNoRepeats(gt, nil); err != nil {
		t.Fatalf("distinct questions: %v", err)
	}
	prev := []string{gt.Questions[3].Text + "?"}
	err := validateNoRepeats(gt, prev)
	if err == nil || rejectClass(err) != rejectRepeat {
		t.Fatalf("a repeat of the previous test must be rejected as %q, got %v", rejectRepeat, err)
	}
}

func TestPrevTopicMarksAndChainWeakTopics(t *testing.T) {
	prev := []models.Question{
		{Topic: "Генетика"}, {Topic: "генетика"}, {Topic: "Клетка"}, {Topic: "Экология"}, {Topic: "Эволюция"},
	}
	marks := []float64{0.2, 0.6, 1.8, -1, 0.9}
	tm := prevTopicMarks(prev, marks)
	if len(tm) != 3 || tm[0].Topic != "Генетика" || math.Abs(tm[0].Mark-0.4) > 1e-9 || tm[0].N != 2 {
		t.Fatalf("topic marks: %+v", tm)
	}
	weak := chainWeakTopics(tm, []string{"Клетка", "ГЕНЕТИКА", "Химия клетки"}, 5)
	want := []string{"Генетика", "Эволюция", "Клетка", "Химия клетки"}
	if strings.Join(weak, "|") != strings.Join(want, "|") {
		t.Fatalf("weak = %v, want %v", weak, want)
	}
	if got := chainWeakTopics(tm, nil, 1); len(got) != 1 {
		t.Fatalf("limit not applied: %v", got)
	}
}

func TestValidateWeakCoverage(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	for i := range gt.Questions {
		gt.Questions[i].Topic = []string{"Генетика", "Клетка", "Прочее"}[i%3]
	}
	if err := validateWeakCoverage(gt, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateWeakCoverage(gt, []string{"генетика", "Клетка"}, nil); err != nil {
		t.Fatalf("both weak topics covered: %v", err)
	}
	err := validateWeakCoverage(gt, []string{"Генетика", "Экология", "Эволюция"}, nil)
	if rejectClass(err) != rejectWeak {
		t.Fatalf("1 of 3 weak topics must be rejected, got %v", err)
	}
	cat := &models.TopicCatalog{Aliases: map[string]string{"экология": "экология", "прочее": "экология"}}
	if err := validateWeakCoverage(gt, []string{"Генетика", "Экология", "Клетка"}, cat); err != nil {
		t.Fatalf("an alias of a weak topic counts: %v", err)
	}
}

func TestChainDifficultyStrengthened(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	// Mean 3.0 for Тест 30 (target 3.5): inside ±0.75.
	for i := range gt.Questions {
		gt.Questions[i].Difficulty = 3
	}
	if err := validateChainDifficulty(gt, 30); err != nil {
		t.Fatal(err)
	}
	// Mean 2.6 (old tolerance 1.0 accepted it) is rejected now.
	for i := range gt.Questions {
		gt.Questions[i].Difficulty = 2
		if i < 12 {
			gt.Questions[i].Difficulty = 3
		}
	}
	if err := validateChainDifficulty(gt, 30); rejectClass(err) != rejectDifficulty {
		t.Fatalf("mean 2.6 for target 3.5 must be a difficulty violation: %v", err)
	}
	// Faked mean: 10×1 + 10×5 = 3.0, but 100% outliers.
	for i := range gt.Questions {
		gt.Questions[i].Difficulty = 1 + 4*(i%2)
	}
	if err := validateChainDifficulty(gt, 25); err == nil || !strings.Contains(err.Error(), "levels away") {
		t.Fatalf("extremes faking the mean must be rejected: %v", err)
	}
	// Progression: easier than the previous test.
	for i := range gt.Questions {
		gt.Questions[i].Difficulty = 2
	}
	if err := validateChainProgression(gt, 20, 3.2); rejectClass(err) != rejectDifficulty {
		t.Fatalf("regression below the previous test must be rejected: %v", err)
	}
	if err := validateChainProgression(gt, 20, 0); err != nil {
		t.Fatalf("unknown previous mean: %v", err)
	}
}

func TestRebalanceAnswerKeys(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	correctText := make([]string, len(gt.Questions))
	for i := range gt.Questions {
		gt.Questions[i].Correct = 0
		correctText[i] = gt.Questions[i].Options[0]
	}
	rebalanceAnswerKeys(gt)
	var count [4]int
	for i, q := range gt.Questions {
		count[q.Correct]++
		if q.Options[q.Correct] != correctText[i] {
			t.Fatalf("question %d: the key text changed", i)
		}
	}
	for p, n := range count {
		if n > 5 {
			t.Fatalf("position %d still has %d keys: %v", p, n, count)
		}
	}
	if err := validateTest(gt); err != nil {
		t.Fatalf("balanced test must validate: %v", err)
	}
}

func TestPlanChainSlots(t *testing.T) {
	for _, n := range []int{1, 10, 30, 100, 200} {
		slots := planChainSlots(n, []string{"Генетика", "Клетка"}, GeneratedQuestionsPerTest, 5)
		if len(slots) != GeneratedQuestionsPerTest {
			t.Fatalf("test %d: %d slots", n, len(slots))
		}
		sum, weak := 0, 0
		for _, s := range slots {
			sum += s.Difficulty
			if s.Topic != "" {
				weak++
			}
		}
		mean := float64(sum) / float64(len(slots))
		if math.Abs(mean-chainDifficultyTarget(n)) > 0.06 {
			t.Fatalf("test %d: planned mean %.2f, target %.1f", n, mean, chainDifficultyTarget(n))
		}
		if weak != 4 {
			t.Fatalf("test %d: %d weak slots, want 4 (2 per weak topic)", n, weak)
		}
		// Every batch of 5 gets a representative spread (not all easy first).
		for b := 0; b < 4; b++ {
			lo, hi := 6, 0
			for _, s := range slots[b*5 : b*5+5] {
				lo, hi = min(lo, s.Difficulty), max(hi, s.Difficulty)
			}
			full := difficultyLevels(difficultyMix(chainDifficultyTarget(n)))
			if full[0] != full[len(full)-1] && lo == hi {
				t.Fatalf("test %d batch %d is flat at %d", n, b, lo)
			}
		}
	}
	if len(planChainSlots(5, nil, 20, 5)) != 20 {
		t.Fatal("no weak topics")
	}
}

func TestPlanPersonalSlots(t *testing.T) {
	topics := []string{"A", "B", "C"}
	slots := planPersonalSlots(topics, 20, 5)
	per := map[string]int{}
	for _, s := range slots {
		per[s.Topic]++
		if s.Difficulty < 2 || s.Difficulty > 4 {
			t.Fatalf("personal difficulty %d", s.Difficulty)
		}
	}
	if len(slots) != 20 || per["A"] != 7 || per["B"] != 7 || per["C"] != 6 {
		t.Fatalf("split %v", per)
	}
}

func TestChooseStrategy(t *testing.T) {
	g := &GeneratorService{cfg: &config.Config{}}
	if s := g.chooseStrategy(&models.GenerationJob{ID: 1, Kind: models.TestKindChain, Attempts: 3}); s != strategyFull {
		t.Fatalf("zero config keeps the legacy single call: %s", s)
	}
	g.cfg.GenStrategy = config.GenStrategyBatch
	if s := g.chooseStrategy(&models.GenerationJob{ID: 1, Kind: models.TestKindChain, Attempts: 1}); s != strategyBatch {
		t.Fatal(s)
	}
	if s := g.chooseStrategy(&models.GenerationJob{ID: 1, Kind: models.JobKindTopicBatch}); s != strategyTopicBatch {
		t.Fatal(s)
	}
	g.cfg.GenStrategy = config.GenStrategyFull
	if s := g.chooseStrategy(&models.GenerationJob{ID: 1, Kind: models.TestKindChain, Attempts: 2}); s != strategyBatch {
		t.Fatalf("a retry escalates to batch: %s", s)
	}
	g.cfg.GenStrategy = config.GenStrategyAB
	g.cfg.GenABBatchPercent = 50
	batch := 0
	for id := int64(1); id <= 1000; id++ {
		j := &models.GenerationJob{ID: id, Kind: models.TestKindPersonal, Attempts: 1}
		s := g.chooseStrategy(j)
		if s != g.chooseStrategy(j) {
			t.Fatal("A/B arm must be stable per job")
		}
		if s == strategyBatch {
			batch++
		}
	}
	if batch < 400 || batch > 600 {
		t.Fatalf("A/B split %d/1000 is not ~50%%", batch)
	}
	g.cfg.GenABBatchPercent = 0
	if s := g.chooseStrategy(&models.GenerationJob{ID: 7, Kind: models.TestKindChain, Attempts: 1}); s != strategyFull {
		t.Fatal(s)
	}
}

func batchReply(qs ...generatedQuestion) string {
	b, _ := json.Marshal(generatedTest{Questions: qs})
	return string(b)
}

func TestMatchBatchPerQuestion(t *testing.T) {
	metrics.Reset()
	g := &GeneratorService{}
	spec := &genSpec{
		kind:      models.TestKindChain,
		slots:     []genSlot{{Topic: "Генетика", Difficulty: 3}, {Difficulty: 3}, {Difficulty: 4}, {Difficulty: 4}},
		prevStems: []string{"Старый вопрос из прошлого теста номер один"},
	}
	mk := func(text, topic string, d int) generatedQuestion {
		return generatedQuestion{Text: text, Options: []string{"один", "два", "три", "четыре"}, Correct: 1, Topic: topic, Difficulty: d}
	}
	group := []int{0, 1, 2, 3}
	raw := batchReply(
		mk("Вопрос по генетике первый", "генетика", 3),
		mk("Старый вопрос из прошлого теста номер один", "Клетка", 3), // repeat → dropped
		mk("Вопрос про клетку", "Клетка", 1),                          // too easy for every free slot → dropped
		mk("Вопрос про экологию", "Экология", 4),
		mk("Вопрос про эволюцию", "Эволюция", 2),
	)
	ctx := withGenRun(context.Background(), &genRun{strategy: strategyBatch, kind: models.TestKindChain})
	got, err := g.matchBatch(ctx, spec, group, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] == nil || got[0].Topic != "Генетика" || got[1] == nil || got[2] == nil {
		t.Fatalf("placed: %+v", got)
	}
	if metrics.Sum(metrics.DifficultyViolations) != 1 || metrics.Sum(metrics.RepeatRejects) != 1 {
		t.Fatalf("metrics: %s", metrics.Summary())
	}
	// Fewer than half usable → the whole reply is rejected with a reason.
	_, err = g.matchBatch(ctx, spec, group, batchReply(mk("Только один вопрос годный", "Генетика", 3)), nil)
	if err == nil || !strings.Contains(err.Error(), "usable") {
		t.Fatalf("expected a rejection, got %v", err)
	}
	if _, err := g.matchBatch(ctx, spec, group, "not json", nil); rejectClass(err) != rejectFormat {
		t.Fatalf("broken JSON: %v", err)
	}
}

func TestBatchPromptCarriesSpecFeedbackAndStablePrefix(t *testing.T) {
	spec := &genSpec{basePrompt: chainGenContext("Биология", 12, nil, nil, []string{"Генетика"}), slots: []genSlot{{Topic: "Генетика", Difficulty: 3}, {Difficulty: 4}}}
	p1 := batchPrompt(spec, []int{0}, nil, "")
	p2 := batchPrompt(spec, []int{1}, []string{"Уже есть вопрос"}, "сложность не по спецификации: 2")
	if !strings.HasPrefix(p1, spec.basePrompt) || !strings.HasPrefix(p2, spec.basePrompt) {
		t.Fatal("every batch prompt must start with the same prefix (prompt caching)")
	}
	if !strings.Contains(p1, "«Генетика»") || !strings.Contains(p1, "difficulty 3") || !strings.Contains(p1, "ровно 1 вопрос") {
		t.Fatalf("spec missing:\n%s", p1)
	}
	if !strings.Contains(p2, "ОТКЛОНЁН") || !strings.Contains(p2, "Уже есть вопрос") {
		t.Fatalf("feedback/avoid list missing:\n%s", p2)
	}
	if !strings.Contains(spec.basePrompt, "СЛАБЫЕ ТЕМЫ") {
		t.Fatal("weak topics must reach the chain prompt")
	}
}

func TestChainPromptTopicAggregates(t *testing.T) {
	prev := []models.Question{{Topic: "Генетика", Text: "Вопрос 1"}, {Topic: "Генетика", Text: "Вопрос 2"}, {Topic: "Клетка", Text: "Вопрос 3", Difficulty: 2}}
	p := chainGenPrompt("Биология", 4, prev, []float64{0, 1, 2})
	if !strings.Contains(p, "Генетика — 0.5") || !strings.Contains(p, "Клетка — 2.0") {
		t.Fatalf("per-topic marks missing:\n%s", p)
	}
	if !strings.HasSuffix(p, chainOutputLine) {
		t.Fatal("the full prompt ends with the output instruction")
	}
}

func TestMetricsPrometheusFormat(t *testing.T) {
	metrics.Reset()
	metrics.Inc(metrics.DifficultyViolations, "step", `groq/x"y`, "kind", "chain")
	metrics.Inc(metrics.DifficultyViolations, "step", `groq/x"y`, "kind", "chain")
	var b strings.Builder
	metrics.WritePrometheus(&b)
	out := b.String()
	if !strings.Contains(out, "# TYPE gen_difficulty_violations_total counter") ||
		!strings.Contains(out, `gen_difficulty_violations_total{step="groq/x\"y",kind="chain"} 2`) {
		t.Fatalf("output:\n%s", out)
	}
	if metrics.Sum(metrics.DifficultyViolations) != 2 {
		t.Fatal("sum")
	}
}

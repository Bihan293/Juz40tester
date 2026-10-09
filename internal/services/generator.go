// Package services — AI test generation pipeline.
//
// NOTE (scale update): by default (GEN_STRATEGY=batch) a test is assembled
// from batches of 5 questions validated one by one (batchgen.go); the
// single-call description below is the "full" strategy, still available
// and A/B-testable (strategy.go). Identical weak-topic inputs are served by
// fingerprint templates (templates.go); the chain validators live in
// chain_validate.go.
//
// Cost discipline (the pipeline was redesigned after burning ~$0.10 per
// test; a generated 20-question test costs roughly $0.002–0.006 now):
//   - ONE model call per test: deepseek-flash (V4.1-Flash) IN THINKING MODE
//     writes the whole test and self-checks the answer keys. Effort is
//     adaptive: chain tests (one per subject, shared by every user) get
//     reasoning_effort=high — the "medium" level, best quality where the
//     cost is amortised across all users; personal weak-topics tests get
//     effort=low (a simpler, topics-only task); every RETRY of a failed
//     job also drops to low — when a harder pass blew the token budget, a
//     shorter thinking pass is what actually fits the budget;
//   - compact prompts: the previous test is passed as topic + short stem +
//     the user's knowledge marks (0/1/2), never as full questions with all
//     answer options; the polish step and the `why` field are gone;
//   - max_tokens is a hard cap that also covers the hidden thinking tokens
//     (8000 output tokens ≈ $0.0096 worst-case at peak flash pricing);
//   - locked chain tests are pre-generated only in off-peak hours (DeepSeek
//     charges half price outside 01:00–04:00 and 06:00–10:00 UTC on
//     weekdays); a test the user can already open is generated URGENTLY,
//     ignoring the window;
//   - the client logs real token usage and an estimated $ cost of every
//     call (plus a running session total) — the spend per test is always
//     visible in the server logs.
//
// A strict local validator checks the JSON contract before anything is
// written to the database.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

const (
	// GeneratedQuestionsPerTest is the fixed size of every AI-generated test.
	GeneratedQuestionsPerTest = 20
	// weakTopicsCount is how many weak topics go into one weak test.
	weakTopicsCount = 5
	// personalTestTitle is the title of every personal weak-topics test.
	personalTestTitle = "🎯 Слабые темы"
	// maxJobAttempts caps retries of a failing generation job.
	maxJobAttempts = 3
	// retryDelay is the backoff applied between job retries.
	retryDelay = 10 * time.Minute
	// jobTimeout bounds a single generation run. It covers the whole
	// provider route (up to 3 Groq steps of groqStepTimeout each, then the
	// DeepSeek fallback, then the repair of flagged questions) and stays
	// below stuckJobTimeout (the heartbeat keeps a live job fresh anyway).
	// The Groq steps are cut so that deepseekGenReserve is always left for
	// the paid fallback + repair (see aiStep.reserve).
	jobTimeout = 14 * time.Minute
	// stuckJobTimeout: a 'running' job idle longer than this is returned to
	// 'pending' (the worker died mid-generation — deploy, restart, OOM).
	stuckJobTimeout = 20 * time.Minute
	// deepseekGenReserve: time guaranteed to the DeepSeek generation step
	// (and the repair round after it) — the free Groq steps never use it.
	deepseekGenReserve = 4 * time.Minute
	// genMaxTokens caps the model output INCLUDING the hidden thinking
	// tokens — the hard cost limiter of one FULL (20-question) generation
	// (strategy "full" and the 10-question topic batches). 20 questions
	// with 4 options need ~3000–3800 visible tokens; 6000 (was 8000) leaves
	// a short thinking pass — a model that needs more is cut off and the
	// next step runs instead of burning the budget. The batch strategy uses
	// genBatchMaxTokens per 5 questions. Worst case of one call at peak
	// flash pricing ≈ $0.0072.
	genMaxTokens = 6000

	// groqGenMinTokens is the smallest output budget a full Groq generation
	// request may run with (was 4500). If the prompt leaves less than that
	// under the free-tier per-request ceiling (TPM 8000), the Groq step is
	// skipped without an HTTP call and the next provider runs; a reply cut
	// off by the cap fails fast as truncated.
	groqGenMinTokens = 3000
	// groqGenMaxWait: the worker is a background process, so it may wait
	// for the per-minute window (TPM 8000 ≈ one test per minute per model)
	// instead of paying DeepSeek. Daily-quota exhaustion never waits.
	groqGenMaxWait = 65 * time.Second
)

// GeneratorService generates tests with DeepSeek and manages the job queue.
type GeneratorService struct {
	ds         *deepseek.Client // nil when DEEPSEEK_API_KEY is not set
	gq         *groq.Client     // nil / disabled when GROQ_API_KEY is not set
	cfg        *config.Config
	gen        *repositories.GenerationRepository
	subjects   *repositories.SubjectRepository
	state      *repositories.StateRepository
	translator *TranslatorService // nil until WithTranslator wires it
	now        func() time.Time   // injectable for tests

	// wake nudges an idle worker the moment a job is enqueued in this
	// instance or a gen_jobs NOTIFY arrives (buffered, size 1 — signals
	// coalesce). The 3-minute ticker is only a fallback (retries, lost
	// notifications).
	wake chan struct{}
	// dsSem bounds paid DeepSeek generation calls in flight across all
	// workers (cfg.GenDeepSeekConcurrency).
	dsSem chan struct{}
	// budget is the DeepSeek daily cap (nil = no cap); used to schedule the
	// retry of a capped job. The hard guard itself is in the DeepSeek client.
	budget *DailyBudget
	// onJobFinished is called in-process after a job was completed or
	// failed (R-7): the bot's generation watchers deliver the result to
	// the waiting users at once instead of waiting for their next poll.
	onJobFinished func(job *models.GenerationJob)
	// noBank disables B3 bank assembly (tests of the personal-generation path only).
	noBank bool
}

// WithJobFinishedHook installs the in-process «job finished» callback (R-7).
func (g *GeneratorService) WithJobFinishedHook(fn func(job *models.GenerationJob)) *GeneratorService {
	g.onJobFinished = fn
	return g
}

// jobFinished runs the hook (never panics the worker).
func (g *GeneratorService) jobFinished(job *models.GenerationJob) {
	if g.onJobFinished == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("generator: job-finished hook panicked: %v", r)
		}
	}()
	g.onJobFinished(job)
}

// WithBudget wires the DeepSeek daily spending cap (R-9).
func (g *GeneratorService) WithBudget(b *DailyBudget) *GeneratorService {
	g.budget = b
	return g
}

// ErrPersonalGenLimit: the user already requested PersonalGenPerUserDay new
// personal weak-topics generations today (R-9).
var ErrPersonalGenLimit = errors.New("daily personal generation limit reached")

// ErrGenQueueBusy: too many personal generations are queued right now
// (GEN_MAX_ACTIVE_PERSONAL backpressure) — the user should retry later.
var ErrGenQueueBusy = errors.New("personal generation queue is full")

// maxActivePersonal is the backpressure limit (0 = none).
func (g *GeneratorService) maxActivePersonal() int {
	if g.cfg == nil {
		return 0
	}
	return g.cfg.GenMaxActivePersonal
}

// personalGenLimit returns the configured per-user daily limit (0 = none).
func (g *GeneratorService) personalGenLimit() int {
	if g.cfg == nil {
		return 0
	}
	return g.cfg.PersonalGenPerUserDay
}

// dayStart is the start of the current calendar day in the bot's calendar
// (Kazakhstan, the same one the 🔥 streak uses).
func dayStart(now time.Time) time.Time {
	y, m, d := now.In(models.StreakLocation).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, models.StreakLocation)
}

func NewGeneratorService(ds *deepseek.Client, cfg *config.Config, gen *repositories.GenerationRepository, subjects *repositories.SubjectRepository, state *repositories.StateRepository) *GeneratorService {
	dsConc := config.DefaultGenDeepSeekConcurrency
	if cfg != nil && cfg.GenDeepSeekConcurrency > 0 {
		dsConc = cfg.GenDeepSeekConcurrency
	}
	return &GeneratorService{
		ds: ds, cfg: cfg, gen: gen, subjects: subjects, state: state,
		now:   time.Now,
		wake:  make(chan struct{}, 1),
		dsSem: make(chan struct{}, dsConc),
	}
}

// Wake wakes an idle worker; called by the LISTEN gen_jobs listener (R-5b).
func (g *GeneratorService) Wake() { g.notifyWorkers() }

// notifyWorkers wakes one idle worker without blocking (no-op when a wake
// is already pending or the service was built without a channel in tests).
func (g *GeneratorService) notifyWorkers() {
	if g == nil || g.wake == nil {
		return
	}
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// workerCount is the configured size of the generation worker pool.
func (g *GeneratorService) workerCount() int {
	n := config.DefaultGenWorkers
	if g.cfg != nil && g.cfg.GenWorkers > 0 {
		n = g.cfg.GenWorkers
	}
	return min(n, config.MaxGenWorkers)
}

// acquireDeepSeek takes a slot of the paid-generation semaphore (or fails
// with ctx). A nil semaphore (tests building the struct directly) is
// unbounded.
func (g *GeneratorService) acquireDeepSeek(ctx context.Context) (release func(), err error) {
	if g.dsSem == nil {
		return func() {}, nil
	}
	select {
	case g.dsSem <- struct{}{}:
		return func() { <-g.dsSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WithTranslator wires the Kazakh translation cache. Cloned personal tests
// get fresh question rows (new ids), so without this the Kazakh translation
// cache of the clone source would be orphaned and every Kazakh-speaking user
// of the clone would pay for a NEW DeepSeek translation of identical text.
func (g *GeneratorService) WithTranslator(t *TranslatorService) *GeneratorService {
	g.translator = t
	return g
}

// WithGroq wires the free Groq provider (GPT-OSS 120B primary, Qwen 3.8
// 27B secondary). DeepSeek then becomes the paid last-resort fallback.
func (g *GeneratorService) WithGroq(gq *groq.Client) *GeneratorService {
	if gq != nil && gq.Enabled() {
		g.gq = gq
	}
	return g
}

// Enabled reports whether AI generation is configured (any provider).
func (g *GeneratorService) Enabled() bool { return g != nil && (g.ds != nil || g.gq != nil) }

// ---------------------------------------------------------------------------
// Prompts (kept deliberately compact — prompt tokens are billed too)
// ---------------------------------------------------------------------------

// generatedQuestion is the JSON contract used with the model. No `why`
// field: explanations double the output tokens for zero gameplay value.
type generatedQuestion struct {
	Text       string   `json:"question"`
	Options    []string `json:"options"`       // exactly 4
	Correct    int      `json:"correct_index"` // 0..3
	Topic      string   `json:"topic"`
	Difficulty int      `json:"difficulty"` // 1..5
}

type generatedTest struct {
	Questions []generatedQuestion `json:"questions"`
}

const genSystemPrompt = `Ты — автор тестов ЕНТ/УБТ (Казахстан) для школьников 9–11 классов. Пишешь на русском (если в задании не указан другой язык предмета), в официальном стиле ЕНТ: точные однозначные формулировки, без двойных отрицаний, строго по школьной программе.

Требования к каждому вопросу:
1. Ровно один правильный ответ; 4 варианта; дистракторы правдоподобные (типичные ошибки учеников), не абсурдные.
2. Правильный ответ выбирается ТОЛЬКО знанием правила/факта, никогда — по внешнему виду. Все 4 варианта одной структуры, близкой длины, в одном формате записи (все предложения, все числа с одинаковыми единицами, все слова одной части речи и т.п.).
3. Пунктуация и орфография: если спрашиваешь, ГДЕ нужен знак (тире, запятая, двоеточие, дефис, кавычки) или буква — НИ В ОДНОМ варианте он не проставлен (во всех вариантах одинаково оставь пропуск «_» или вообще не ставь знак). Если спрашиваешь, где знак поставлен ВЕРНО/НЕВЕРНО — знак есть во ВСЕХ вариантах. Недопустимо, чтобы нужный знак/буква/пропуск был только в одном варианте.
   Плохо: «В каком предложении нужно тире? A) Наступила зима. B) Книга лежит на столе. C) Я люблю русский язык. D) Москва — столица России.» (тире есть только в D).
   Хорошо: «В каком предложении на месте пропуска нужно тире? A) Москва _ столица России. B) Зимой _ здесь очень холодно. C) Книга _ лежит на столе. D) Он _ мой старый друг.»
4. Никаких подсказок: без пометок «(верно)»/«✓», без вариантов «все ответы верны»/«нет правильного ответа», без дословного повтора ответа из текста вопроса, без совпадающих вариантов, без грамматического согласования вопроса только с одним вариантом.
5. Позиции правильных ответов равномерно по 0–3 в рамках теста.
6. topic — короткая тема (1–4 слова), difficulty — 1–5. Называй темы стандартно, как разделы школьной программы (например «Фотосинтез», «Квадратные уравнения»), одну и ту же тему — всегда одинаково: по темам считается статистика ученика.
7. Перед ответом мысленно проверь каждый вопрос: correct_index указывает на действительно правильный вариант, и угадать его, не зная темы, нельзя.

Формат — строго JSON, без пояснений и markdown:
{"questions":[{"question":"...","options":["...","...","...","..."],"correct_index":0,"topic":"...","difficulty":2}]}`

// chainGenPrompt builds the user prompt for the next chain test. The model
// sees the PREVIOUS test as compact lines "тема — короткая суть вопроса —
// уровень учеников" (0=не знают, 1=в процессе, 2=закреплено; for a shared
// chain test this is the AVERAGE over every student who answered the
// question, see GeneratorService.questionMarks) and must hit the target
// difficulty of THIS chain position, target the weak topics and never repeat
// questions.
func chainGenPrompt(subjectName string, testNumber int, prev []models.Question, marks []float64) string {
	return chainGenContext(subjectName, testNumber, prev, marks, nil) + chainOutputLine
}

// chainOutputLine is the output instruction of a FULL (one-call) chain
// prompt; the batch strategy replaces it with the per-batch spec.
var chainOutputLine = "\nВыдай строго JSON по схеме, ровно " + fmt.Sprint(GeneratedQuestionsPerTest) + " вопросов."

// chainGenContext is the chain prompt WITHOUT the output instruction: the
// stable prefix shared by every call of one job (prompt caching).
//
// Besides the previous test (per question: topic — stem — average mark of
// all students) it carries the per-TOPIC aggregate of the previous test and
// the weak topics the test must train (weak topics of the previous test
// merged with the subject-wide weak topics of the active students), and
// demands a harder test without repeats.
func chainGenContext(subjectName string, testNumber int, prev []models.Question, marks []float64, weak []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Предмет: «%s». Составь Тест №%d из ровно %d вопросов.\n\n", subjectName, testNumber, GeneratedQuestionsPerTest)
	b.WriteString(languageSubjectRule(subjectName))
	b.WriteString(difficultyScale)
	target := chainDifficultyTarget(testNumber)
	mix := difficultyMix(target)

	switch {
	case testNumber <= 1:
		b.WriteString("Это ПЕРВЫЙ тест цепочки — самый лёгкий: базовые определения и факты, фундаментальные темы программы 9–11 классов — то, без чего нельзя начинать подготовку к ЕНТ/УБТ. Ученик должен почувствовать, что у него получается.\n")
	case len(prev) == 0:
		// The previous test is not available (removed / not generated). This
		// is NOT the first test: the difficulty still follows the chain
		// position — test 30 must never fall back to the beginner level.
		fmt.Fprintf(&b, "Это Тест №%d длинной цепочки (прошлый тест недоступен): уровень соответствует позиции в цепочке, а не началу подготовки. Охвати разные разделы программы.\n", testNumber)
	default:
		fmt.Fprintf(&b, "Прошлый Тест №%d", testNumber-1)
		if avg := meanDifficulty(prev); avg > 0 {
			fmt.Fprintf(&b, " (средняя сложность %.1f)", avg)
		}
		b.WriteString(" — тема — суть вопроса — уровень учеников (0=не знают, 1=в процессе, 2=закреплено):\n")
		for i, q := range prev {
			// Cut by RUNES, not bytes: a byte slice cut in the middle of a
			// Cyrillic/Kazakh letter produced invalid UTF-8 in the prompt.
			stem := q.Text
			if r := []rune(stem); len(r) > 80 {
				stem = string(r[:80]) + "…"
			}
			mark := "?"
			if i < len(marks) && marks[i] >= 0 {
				mark = strconv.FormatFloat(marks[i], 'f', -1, 64)
			}
			fmt.Fprintf(&b, "%d. [%s] %s — %s\n", i+1, q.Topic, stem, mark)
		}
		if tm := prevTopicMarks(prev, marks); len(tm) > 0 {
			b.WriteString("Средний уровень учеников по темам прошлого теста: ")
			parts := make([]string, 0, len(tm))
			for _, m := range tm {
				parts = append(parts, fmt.Sprintf("%s — %.1f", m.Topic, m.Mark))
			}
			b.WriteString(strings.Join(parts, "; "))
			b.WriteString(".\n")
		}
		fmt.Fprintf(&b, "\nТвой Тест №%d ОБЯЗАН:\n", testNumber)
		b.WriteString("— быть немного сложнее прошлого: сложность растёт плавно от теста к тесту, без резких скачков, и НЕ НИЖЕ прошлого теста;\n")
		b.WriteString("— подтягивать слабые места: темы с уровнем ниже 1.5 повтори через НОВЫЕ формулировки и другие аспекты, хорошо усвоенные темы почти не трогай — вместо них бери новые разделы программы;\n")
		b.WriteString("— НЕ ПОВТОРЯТЬ ни одного вопроса прошлого теста: другие формулировки, подтемы, числа и примеры (повторы отбраковываются автоматически).\n")
	}
	if len(weak) > 0 {
		fmt.Fprintf(&b, "\nСЛАБЫЕ ТЕМЫ учеников (по прошлому тесту и общей статистике предмета) — ОБЯЗАТЕЛЬНО включи вопросы минимум по %d из них, поле topic — дословно: %s.\n",
			min(weakCoverageNeed, len(weak)), strings.Join(weak, "; "))
	}
	fmt.Fprintf(&b, "\nСЛОЖНОСТЬ Теста №%d: средняя difficulty ≈ %.1f. Распределение 20 вопросов по уровням: %s. Поле difficulty каждого вопроса ставь честно по шкале выше (тест с другой средней сложностью отклоняется автоматически).\n",
		testNumber, target, mixString(mix))
	return b.String()
}

// difficultyScale defines what each difficulty level MEANS, so the model's
// "difficulty" tags (and the target mix) are comparable from test to test.
const difficultyScale = `Шкала сложности (difficulty):
1 — узнавание: прямое определение, термин или факт из учебника;
2 — понимание: простое применение одного правила/формулы в один шаг;
3 — стандартное задание ЕНТ: применение в 2 шага, сравнение, типичная ловушка;
4 — трудное задание ЕНТ: многошаговое рассуждение, сочетание двух тем, нестандартная формулировка;
5 — самые трудные задания ЕНТ: длинная цепочка рассуждений, тонкие исключения, анализ данных.

`

// difficultyCurve: anchor points (chain position -> target MEAN difficulty
// of the test). Between anchors the target is linearly interpolated, beyond
// the last anchor it stays flat. The curve is long on purpose: test 1 is
// really easy, test 10 noticeably harder, test 30 is a solid ЕНТ level,
// tests 100+ are hard. Every step between neighbours is tiny (≤ 0.18), so a
// student never meets a sudden wall.
var difficultyCurve = []struct {
	n      int
	target float64
}{
	{1, 1.3}, {5, 2.0}, {10, 2.6}, {20, 3.1}, {30, 3.5},
	{50, 3.9}, {100, 4.4}, {150, 4.7}, {200, 4.8},
}

// chainDifficultyTarget returns the target mean difficulty (1..5) of chain
// test number n.
func chainDifficultyTarget(n int) float64 {
	if n <= difficultyCurve[0].n {
		return difficultyCurve[0].target
	}
	for i := 1; i < len(difficultyCurve); i++ {
		a, b := difficultyCurve[i-1], difficultyCurve[i]
		if n <= b.n {
			t := a.target + (b.target-a.target)*float64(n-a.n)/float64(b.n-a.n)
			return math.Round(t*10) / 10
		}
	}
	return difficultyCurve[len(difficultyCurve)-1].target
}

// difficultyMix splits the 20 questions of a test over the levels 1..5 so
// that the mean is (close to) target. Mostly the two neighbouring levels;
// where possible a couple of warm-up questions one level lower and a couple
// of stretch questions one level higher (the mean is preserved).
func difficultyMix(target float64) [5]int {
	var mix [5]int
	total := GeneratedQuestionsPerTest
	if target <= 1 {
		mix[0] = total
		return mix
	}
	if target >= 5 {
		mix[4] = total
		return mix
	}
	lo := int(math.Floor(target)) // 1..4
	hi := lo + 1
	nHi := int(math.Round(float64(total) * (target - float64(lo))))
	nLo := total - nHi
	mix[lo-1], mix[hi-1] = nLo, nHi
	// Spread: 2 warm-up questions one level below lo and 2 stretch questions
	// one level above hi — only when both sides exist (keeps the mean).
	if lo > 1 && hi < 5 && nLo >= 4 && nHi >= 4 {
		mix[lo-1] -= 2
		mix[lo-2] += 2
		mix[hi-1] -= 2
		mix[hi] += 2
	}
	return mix
}

// mixString renders a difficulty mix for the prompt: "5 вопросов уровня 1, …".
func mixString(mix [5]int) string {
	parts := make([]string, 0, 5)
	for lvl, n := range mix {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d — уровня %d", n, lvl+1))
		}
	}
	return strings.Join(parts, ", ")
}

// meanDifficulty is the average difficulty tag of the questions (0 if none).
func meanDifficulty(qs []models.Question) float64 {
	sum, n := 0, 0
	for _, q := range qs {
		if q.Difficulty >= 1 && q.Difficulty <= 5 {
			sum += q.Difficulty
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Round(float64(sum)/float64(n)*10) / 10
}

// chainDifficultyTolerance: how far the mean difficulty of a generated chain
// test may drift from the target before the reply is rejected (the model
// ignored the level instruction — e.g. wrote a beginner test for Тест 40).
// Tightened from 1.0 to 0.75: with 1.0 a Тест 30 (target 3.5) written
// entirely at level 3 with a few 2s still passed as «2.6».
const chainDifficultyTolerance = 0.75

// validateChainDifficulty checks that the mean difficulty of a generated
// chain test is close to the target of its chain position, and that the
// requested distribution was not faked by extremes (≤ 20% of the questions
// may be ≥ 2 levels away from the target).
func validateChainDifficulty(gt *generatedTest, testNumber int) error {
	if len(gt.Questions) == 0 {
		return nil
	}
	mean := genMeanDifficulty(gt)
	target := chainDifficultyTarget(testNumber)
	if math.Abs(mean-target) > chainDifficultyTolerance {
		return rejectf(rejectDifficulty, "difficulty: mean %.2f is too far from the target %.1f of test %d (tolerance ±%.2f)", mean, target, testNumber, chainDifficultyTolerance)
	}
	outliers := 0
	for _, q := range gt.Questions {
		if math.Abs(float64(q.Difficulty)-target) >= chainDifficultyOutlierGap {
			outliers++
		}
	}
	if float64(outliers) > chainDifficultyMaxOutliers*float64(len(gt.Questions)) {
		return rejectf(rejectDifficulty, "difficulty: %d of %d questions are ≥ %.0f levels away from the target %.1f of test %d", outliers, len(gt.Questions), chainDifficultyOutlierGap, target, testNumber)
	}
	return nil
}

// languageSubjectRule returns the extra instruction for LANGUAGE subjects:
// their tests are written in the studied language itself (an English test
// in English, a Kazakh-language test in Kazakh) — exactly like the real
// ЕНТ/УБТ. Such tests are never machine-translated afterwards, so the
// original language is the one the student sees. Empty for other subjects.
func languageSubjectRule(subjectName string) string {
	switch models.SubjectContentLang(subjectName) {
	case models.ContentLangEN:
		return "ЯЗЫК ТЕСТА: это предмет «Английский язык». Весь тест — вопросы, варианты ответов — пиши ТОЛЬКО на английском языке, в формате ЕНТ по английскому (grammar, vocabulary, reading). Поле topic — короткое название темы на английском (например «Present Perfect»).\n\n"
	case models.ContentLangDE:
		return "ЯЗЫК ТЕСТА: это предмет «Немецкий язык». Вопросы и варианты ответов пиши ТОЛЬКО на немецком языке. Поле topic — на немецком.\n\n"
	case models.ContentLangFR:
		return "ЯЗЫК ТЕСТА: это предмет «Французский язык». Вопросы и варианты ответов пиши ТОЛЬКО на французском языке. Поле topic — на французском.\n\n"
	case models.ContentLangKK:
		return "ЯЗЫК ТЕСТА: это предмет «Казахский язык/литература». Весь тест — вопросы, варианты ответов и поле topic — пиши ТОЛЬКО на казахском языке (қазақ тілінде), по программе казахского языка для ЕНТ/ҰБТ.\n\n"
	case models.ContentLangRU:
		return "ЯЗЫК ТЕСТА: это предмет «Русский язык/литература». Тест проверяет знание русского языка — пиши ТОЛЬКО на русском; варианты ответов — слова, формы, правила русского языка. Тест не переводится на другие языки.\n\n"
	}
	return ""
}

// personalGenPrompt builds the user prompt for a weak-topics test. ONLY the
// short weak-topic names (a couple of words each, e.g. «Генетика»,
// «Микроорганизмы») are sent — never whole past tests with 40–60 questions:
// the prompt stays tiny and cheap, and the model must tag every question
// with one of THESE topics verbatim (validated server-side afterwards).
func personalGenPrompt(subjectName string, topics []string) string {
	return personalGenContext(subjectName, topics) + personalOutputLine
}

// personalOutputLine is the output instruction of a full personal prompt.
const personalOutputLine = "\nВыдай строго JSON по схеме."

// personalGenContext is the personal prompt without the output instruction
// (stable prefix of every batch call of the job).
func personalGenContext(subjectName string, topics []string) string {
	var b strings.Builder
	b.WriteString(languageSubjectRule(subjectName))
	fmt.Fprintf(&b, "Предмет: «%s». Составь тренировочный тест из ровно %d вопросов ТОЛЬКО по этим слабым темам ученика:\n", subjectName, GeneratedQuestionsPerTest)
	for i, t := range topics {
		fmt.Fprintf(&b, "%d. %s\n", i+1, t)
	}
	b.WriteString("\nТребования:\n")
	b.WriteString("— вопросы равномерно покрывают КАЖДУЮ из перечисленных тем (примерно поровну на тему), лишних тем нет;\n")
	b.WriteString("— поле topic каждого вопроса ДОСЛОВНО равно одной из перечисленных тем (скопируй строку из списка);\n")
	b.WriteString("— от базовых аспектов к сложным (difficulty 2–4): закрыть пробел, а не завалить;\n")
	b.WriteString("— уровень и формат реального ЕНТ/УБТ для 9–11 классов, без повторов.\n")
	return b.String()
}

// normalizeTopic canonicalises a topic string for comparison (the model may
// differ in case or surrounding whitespace even when told to copy verbatim).
func normalizeTopic(t string) string { return models.NormalizeTopic(t) }

// validatePersonalCoverage enforces the weak-topics contract server-side:
// every question must be tagged with one of the REQUESTED topics and every
// requested topic must be covered — otherwise the test would silently train
// topics the user already knows (wasted money) or miss actual gaps.
//
// B1: with a topic catalog, a spelling that is an alias of a requested topic
// (same topic_key) is accepted too and rewritten to the requested topic.
func validatePersonalCoverage(gt *generatedTest, topics []string, catalog *models.TopicCatalog) error {
	allowed := make(map[string]string, len(topics)) // norm -> requested spelling
	byKey := map[string]string{}                    // topic_key -> requested norm
	for _, t := range topics {
		n := normalizeTopic(t)
		allowed[n] = t
		if k, ok := catalog.Resolve(t); ok {
			byKey[k] = n
		}
	}
	covered := make(map[string]bool, len(topics))
	for i := range gt.Questions {
		q := &gt.Questions[i]
		n := normalizeTopic(q.Topic)
		if _, ok := allowed[n]; !ok {
			k, found := catalog.Resolve(q.Topic)
			req, mapped := byKey[k]
			if !found || !mapped {
				return fmt.Errorf("question %d: topic %q is not in the weak-topics list", i+1, q.Topic)
			}
			n = req
			q.Topic = allowed[req]
		}
		covered[n] = true
	}
	if len(covered) != len(allowed) {
		return fmt.Errorf("only %d of %d weak topics covered", len(covered), len(allowed))
	}
	return nil
}

// maxNewChainTopics: how many topics OUTSIDE the catalog one generated chain
// test may introduce (B1). The chain must still reach new sections of the
// programme, so a few new topics are allowed (they are added to the catalog
// when the test is stored); a reply that ignores the list is rejected.
const maxNewChainTopics = 2

// maxPromptTopics bounds the catalog list sent in a prompt (tokens).
const maxPromptTopics = 80

// validateChainTopics maps the topics of a chain test through the catalog
// (an alias is rewritten to the canonical title) and rejects a reply with
// more than maxNewChainTopics topics outside it. An empty catalog (new
// subject) accepts everything — the first tests seed it.
func validateChainTopics(gt *generatedTest, catalog *models.TopicCatalog) error {
	if catalog == nil || len(catalog.Aliases) == 0 {
		return nil
	}
	titles := make(map[string]string, len(catalog.Titles)) // key -> title
	for _, t := range catalog.Titles {
		titles[normalizeTopic(t)] = t
	}
	unknown := map[string]bool{}
	for i := range gt.Questions {
		q := &gt.Questions[i]
		k, ok := catalog.Resolve(q.Topic)
		if !ok {
			if n := normalizeTopic(q.Topic); n != "" {
				unknown[n] = true
			}
			continue
		}
		if t, ok := titles[k]; ok {
			q.Topic = t
		}
	}
	if len(unknown) > maxNewChainTopics {
		return fmt.Errorf("%d topics outside the subject topic list (max %d)", len(unknown), maxNewChainTopics)
	}
	return nil
}

// topicListPrompt is the prompt block with the catalog topics of a subject.
func topicListPrompt(catalog *models.TopicCatalog) string {
	if catalog == nil || len(catalog.Titles) == 0 {
		return ""
	}
	titles := catalog.Titles
	if len(titles) > maxPromptTopics {
		titles = titles[:maxPromptTopics]
	}
	var b strings.Builder
	b.WriteString("\nСПРАВОЧНИК ТЕМ предмета — поле topic каждого вопроса ДОСЛОВНО копируй из этого списка: ")
	b.WriteString(strings.Join(titles, "; "))
	fmt.Fprintf(&b, ". Новую тему (не больше %d на тест) вводи, только если раздела программы нет в списке.\n", maxNewChainTopics)
	return b.String()
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// validateTest enforces the JSON contract before anything reaches the DB.
func validateTest(gt *generatedTest) error {
	return validateQuestions(gt, GeneratedQuestionsPerTest)
}

// validateQuestions is validateTest for a reply of exactly want questions
// (a test has GeneratedQuestionsPerTest, a topic_batch TopicBatchSize).
func validateQuestions(gt *generatedTest, want int) error {
	if len(gt.Questions) != want {
		return fmt.Errorf("need %d questions, got %d", want, len(gt.Questions))
	}
	seen := make(map[string]bool, len(gt.Questions))
	positions := map[int]int{}
	for i := range gt.Questions {
		q := &gt.Questions[i]
		q.Text = strings.TrimSpace(q.Text)
		q.Topic = strings.TrimSpace(q.Topic)
		if len(q.Text) < 8 {
			return fmt.Errorf("question %d: text too short", i+1)
		}
		if len(q.Options) != 4 {
			return fmt.Errorf("question %d: need 4 options, got %d", i+1, len(q.Options))
		}
		for j := range q.Options {
			q.Options[j] = strings.TrimSpace(q.Options[j])
			if q.Options[j] == "" {
				return fmt.Errorf("question %d: empty option %d", i+1, j+1)
			}
		}
		if q.Correct < 0 || q.Correct > 3 {
			return fmt.Errorf("question %d: correct_index %d out of range", i+1, q.Correct)
		}
		normalizeOptionFormat(q.Text, q.Options)
		if q.Difficulty < 1 || q.Difficulty > 5 {
			q.Difficulty = 3 // sane fallback instead of rejecting the whole test
		}
		key := strings.ToLower(q.Text)
		if seen[key] {
			return fmt.Errorf("question %d: duplicate question text", i+1)
		}
		seen[key] = true
		positions[q.Correct]++
	}
	// Reject degenerate key distributions (all answers on one position etc.).
	for _, n := range positions {
		if n > want/2 {
			return fmt.Errorf("unbalanced answer key distribution: %v", positions)
		}
	}
	return nil
}

func (gt *generatedTest) toSeed() []models.SeedQuestion {
	out := make([]models.SeedQuestion, 0, len(gt.Questions))
	for _, q := range gt.Questions {
		var opts [4]string
		copy(opts[:], q.Options)
		out = append(out, models.SeedQuestion{
			Text: q.Text, Options: opts, Correct: q.Correct,
			Topic: q.Topic, Difficulty: q.Difficulty,
			// toSeed is only called after repairFlagged + validateTest in
			// runJob: every question has passed the quality audit.
			QualityChecked: true,
		})
	}
	return out
}

// parseTestJSON extracts the JSON object from a model reply (tolerates
// accidental markdown fences and leading/trailing prose) and validates it.
func parseTestJSON(raw string) (*generatedTest, error) {
	return parseQuestionsJSON(raw, GeneratedQuestionsPerTest)
}

// parseQuestionsJSON is parseTestJSON for a reply of exactly want questions.
func parseQuestionsJSON(raw string, want int) (*generatedTest, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in model reply")
	}
	var gt generatedTest
	if err := json.Unmarshal([]byte(raw[start:end+1]), &gt); err != nil {
		return nil, fmt.Errorf("decode model JSON: %w", err)
	}
	if err := validateQuestions(&gt, want); err != nil {
		return nil, err
	}
	return &gt, nil
}

// ---------------------------------------------------------------------------
// Scheduling (off-peak vs urgent)
// ---------------------------------------------------------------------------

// pregenMinDelay: a non-urgent (pre-generated) chain job never starts
// earlier than this after it was queued.
const pregenMinDelay = 2 * time.Hour

// PregenAhead reports whether locked chain tests are pre-generated one test
// ahead in the off-peak window (GEN_PREGEN_AHEAD=1).
func (g *GeneratorService) PregenAhead() bool {
	return g != nil && g.cfg != nil && g.cfg.GenPregenAhead > 0
}

// deferredUntil returns when a non-urgent newly enqueued job may start:
// immediately during off-peak hours, otherwise at the next off-peak window
// start — the cheaper API pricing window (see config.IsOffPeak).
func (g *GeneratorService) deferredUntil() time.Time {
	return g.cfg.NextOffPeakStart(g.now())
}

// ---------------------------------------------------------------------------
// Public API used by the quiz service / handlers
// ---------------------------------------------------------------------------

// EnsureChainTest queues generation of chain test number n (idempotent).
// urgent = true when the user can already open this test — such jobs bypass
// the off-peak deferral and are picked up by the worker immediately, and an
// ALREADY-QUEUED deferred (non-urgent) job for the same test is upgraded to
// urgent on the spot (EnqueueChainJob's conflict update). Without that
// upgrade a test that became openable after its job had been deferred to the
// off-peak window kept the user waiting for hours in ⏳ «Минуточку...».
// Locked tests (urgent = false) are pre-generated only in the off-peak
// window. Returns the job status if it is already queued/running.
func (g *GeneratorService) EnsureChainTest(ctx context.Context, subjectID int64, testNumber int, urgent bool, ownerUserID int64) (queued bool, err error) {
	if !g.Enabled() {
		return false, nil
	}
	notBefore := time.Now()
	if !urgent {
		// Pre-generation (GEN_PREGEN_AHEAD): the cheaper off-peak window,
		// but never earlier than pregenMinDelay — the previous test should
		// collect some knowledge marks first.
		notBefore = g.deferredUntil()
		if earliest := g.now().Add(pregenMinDelay); notBefore.Before(earliest) {
			notBefore = earliest
		}
	}
	inserted, err := g.gen.EnqueueChainJob(ctx, subjectID, testNumber, notBefore, urgent, ownerUserID)
	if err != nil {
		return false, err
	}
	if inserted && urgent {
		log.Printf("generator: queued URGENT chain job: subject %d test %d", subjectID, testNumber)
	}
	if urgent {
		g.notifyWorkers()
	}
	return g.gen.HasPendingOrRunningChainJob(ctx, subjectID, testNumber)
}

// reviveChainTest re-queues the generation of a chain test whose job died
// (status='failed' after exhausting retries) or got stuck while the queue
// was broken. It is triggered by the user tapping the ⏳ button — previously
// that tap only showed a toast and changed NOTHING, so a failed job left the
// test in «Минуточку...» forever (nobody ever re-enqueued it). If an active
// job already exists it is simply made urgent; a missing test with no active
// job gets its failed job revived (or a fresh urgent job). The tap is a
// no-op for tests that already exist.
//
// A failed job is revived only once its backoff has elapsed
// (repositories.ChainReviveBaseBackoff): a test whose generation fails
// every time must not start a new round of paid attempts on every tap. In
// that case nothing is queued and retryAt tells when a retry is possible
// (zero: a job is queued/running, or generation is disabled).
//
// Unexported on purpose: it performs NO unlock check. The only entry point
// is QuizService.ReviveChainTest, which refuses test numbers above the
// user's unlockedMax — so no caller can start a paid generation of a locked
// test by bypassing that guard.
//
// A5: the caller (QuizService.ChainTestOrRevive) has already verified that
// the test does not exist, so no second chain lookup is done here.
func (g *GeneratorService) reviveChainTest(ctx context.Context, subjectID int64, testNumber int, ownerUserID int64) (retryAt time.Time) {
	if !g.Enabled() || testNumber < 1 || testNumber > models.MaxVisibleTests {
		return time.Time{}
	}
	revived, err := g.gen.ReviveChainJob(ctx, subjectID, testNumber, ownerUserID)
	if err != nil {
		log.Printf("generator: revive chain job subject %d test %d: %v", subjectID, testNumber, err)
		return time.Time{}
	}
	if revived {
		log.Printf("generator: revived failed chain job: subject %d test %d (urgent)", subjectID, testNumber)
	}
	// Belt and braces: the normal enqueue path (insert or upgrade to urgent).
	inserted, err := g.gen.EnqueueChainJob(ctx, subjectID, testNumber, time.Now(), true, ownerUserID)
	if err != nil {
		log.Printf("generator: ensure after revive subject %d test %d: %v", subjectID, testNumber, err)
		return time.Time{}
	}
	if inserted {
		log.Printf("generator: queued URGENT chain job: subject %d test %d", subjectID, testNumber)
	}
	g.notifyWorkers()
	if revived || inserted {
		return time.Time{}
	}
	if active, err := g.gen.HasPendingOrRunningChainJob(ctx, subjectID, testNumber); err != nil || active {
		return time.Time{}
	}
	at, err := g.gen.ChainRetryAt(ctx, subjectID, testNumber)
	if err != nil {
		log.Printf("generator: retry time of chain job subject %d test %d: %v", subjectID, testNumber, err)
		return time.Time{}
	}
	if !at.IsZero() {
		log.Printf("generator: chain test subject %d #%d keeps failing — next attempt not before %s", subjectID, testNumber, at.Format(time.RFC3339))
	}
	return at
}

// EnsurePersonalTest returns the user's own weak-topics test of the subject,
// or queues its generation. Personal tests are needed by a waiting user, so
// they are always URGENT. The second return value is true while the user
// has to wait for generation.
//
// Token economy: before paying for an AI generation we look for a test
// generated for the SAME weak-topics fingerprint. Two users with identical
// weakness profiles get identical questions — the second one receives a
// fresh CLONE of the already-generated test (no model call at all).
func (g *GeneratorService) EnsurePersonalTest(ctx context.Context, userID, subjectID int64) (test *models.Test, pending bool, topics []string, err error) {
	return g.ensurePersonalTest(ctx, userID, subjectID, false)
}

// EnsurePersonalTestPaid is EnsurePersonalTest for a PAID weak-topics
// order (Telegram Stars): the per-user daily generation limit and the
// personal-queue backpressure do not apply — the user has paid for this
// one test, refusing it would only lead to a refund.
func (g *GeneratorService) EnsurePersonalTestPaid(ctx context.Context, userID, subjectID int64) (test *models.Test, pending bool, topics []string, err error) {
	return g.ensurePersonalTest(ctx, userID, subjectID, true)
}

func (g *GeneratorService) ensurePersonalTest(ctx context.Context, userID, subjectID int64, paid bool) (test *models.Test, pending bool, topics []string, err error) {
	weak, err := g.gen.WeakTopicStats(ctx, userID, subjectID, weakTopicsCount)
	if err != nil {
		return nil, false, nil, err
	}
	if len(weak) == 0 {
		return nil, false, nil, nil // no weak topics yet
	}
	keys := make([]string, len(weak))
	topics = make([]string, len(weak))
	for i, w := range weak {
		keys[i], topics[i] = w.Key, w.Topic
	}
	test, err = g.gen.FindPersonalTest(ctx, subjectID, userID)
	if err != nil {
		return nil, false, nil, err
	}
	if test != nil {
		return test, false, topics, nil
	}
	// B3: assemble the test from the question bank — audited questions of
	// the weak topics the user has not seen (or still has 🔴). One tests
	// row + links, no new questions, no AI call. Not enough questions —
	// topic batches / a personal generation below keep the bot working.
	var banked *models.Test
	var missing []repositories.BankShortage
	if !g.noBank {
		banked, missing, err = g.gen.AssembleBankPersonalTest(ctx, subjectID, userID, keys, topics, GeneratedQuestionsPerTest, personalTestTitle)
	}
	if err != nil {
		log.Printf("generator: bank assembly for user %d subject %d: %v", userID, subjectID, err)
	} else if banked != nil {
		log.Printf("generator: personal test %d for user %d assembled from the bank (no AI call)", banked.ID, userID)
		return banked, false, topics, nil
	} else if len(missing) > 0 {
		log.Printf("generator: bank short for user %d subject %d on %d topic(s): %v", userID, subjectID, len(missing), missing)
	}
	// Fingerprint template: a test was already GENERATED for exactly this
	// weak-topic set (another user) — clone it right away: identical
	// content, new question rows, no AI call, no waiting, no daily-limit
	// charge. Works even when AI generation is disabled.
	if t := g.clonePersonalForUser(ctx, userID, subjectID, keys); t != nil {
		return t, false, topics, nil
	}
	if !g.Enabled() {
		return nil, false, topics, nil
	}
	// B4b: a short bank queues topic_batch jobs ONLY for the short topics
	// (the unique index dedupes concurrent requests). The user waits while
	// every short topic has an active batch; the watcher re-runs the
	// assembly when a batch finishes. A topic whose batch has just finished
	// (cooldown) or is not in the catalog can not be filled that way — then
	// a personal generation below is the fallback.
	if len(missing) > 0 {
		short := repositories.ShortageKeys(missing)
		created, active, berr := g.gen.EnqueueShortTopicBatches(ctx, subjectID, short, repositories.TopicBatchCooldown)
		if berr != nil {
			log.Printf("generator: topic batches for user %d subject %d: %v", userID, subjectID, berr)
		} else {
			if created > 0 {
				log.Printf("generator: queued %d topic_batch job(s) for subject %d: %v", created, subjectID, short)
				g.notifyWorkers()
			}
			if len(active) == len(short) {
				return nil, true, topics, nil
			}
		}
	}

	// R-9: a NEW paid-capable generation (not a clone, not an already
	// queued job) counts against the per-user daily limit. The count lives
	// in the DB (generation_jobs), so it holds across instances/restarts.
	if limit := g.personalGenLimit(); limit > 0 && !paid {
		already, err := g.gen.HasPendingOrRunningPersonalJob(ctx, subjectID, userID)
		if err != nil {
			return nil, false, nil, err
		}
		if already {
			return nil, true, topics, nil // the queued job is not a new request
		}
		n, err := g.gen.PersonalJobsSince(ctx, userID, dayStart(g.now()))
		if err != nil {
			return nil, false, nil, err
		}
		if n >= limit {
			log.Printf("generator: user %d hit the daily personal generation limit (%d)", userID, limit)
			return nil, false, topics, ErrPersonalGenLimit
		}
	}
	// Backpressure (thousands of users vs a free AI quota of a few hundred
	// tests a day): beyond GEN_MAX_ACTIVE_PERSONAL queued/running personal
	// generations a new one is refused instead of growing the queue (and
	// the wait of everybody in it) without bound. An already queued job of
	// this user was handled above.
	if maxActive := g.maxActivePersonal(); maxActive > 0 && !paid {
		n, err := g.gen.CountActiveJobs(ctx, models.TestKindPersonal)
		if err != nil {
			return nil, false, nil, err
		}
		if n >= maxActive {
			already, err := g.gen.HasPendingOrRunningPersonalJob(ctx, subjectID, userID)
			if err != nil {
				return nil, false, nil, err
			}
			if !already {
				metrics.Inc(metrics.QueueBackpressure, "kind", models.TestKindPersonal)
				log.Printf("generator: personal queue full (%d active) — user %d asked to retry later", n, userID)
				return nil, false, topics, ErrGenQueueBusy
			}
			return nil, true, topics, nil
		}
	}
	if err := g.gen.EnqueuePersonalJob(ctx, subjectID, userID); err != nil {
		return nil, false, nil, err
	}
	g.notifyWorkers()
	// A5: no second HasPendingOrRunningPersonalJob — after a successful
	// enqueue a pending/running job exists (inserted now, or the active one
	// the ON CONFLICT hit on idx_genjobs_personal_unique).
	return nil, true, topics, nil
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

// RunWorker processes the generation queue until ctx is cancelled.
//
// It starts a pool of cfg.GenWorkers generation workers (GEN_WORKERS,
// default 4) plus ONE separate low-priority quality-sweep goroutine, and
// returns only when all of them have stopped. Every worker claims jobs with
// ClaimNextJob (FOR UPDATE SKIP LOCKED + status flip to 'running' in the same
// transaction), so a job is never executed by two workers — neither inside
// this instance nor across instances during a zero-downtime deploy.
//
// Each worker loop is wrapped in a recover: if it ever panics, the worker
// must NOT die — a dead pool leaves every queued test hanging in
// ⏳ «Минуточку...» forever. The loop is restarted after a short pause
// (backoff against crash-looping). Only a context cancellation (shutdown)
// stops the workers for good.
func (g *GeneratorService) RunWorker(ctx context.Context) {
	if !g.Enabled() {
		return
	}
	route := []string{}
	if g.gq != nil {
		route = append(route, "groq "+groq.ModelGPTOSS120B, "groq "+groq.ModelQwen27B)
	}
	if g.ds != nil {
		route = append(route, "deepseek "+g.ds.ReasonerModel()+" (paid fallback)")
	}
	n := g.workerCount()
	log.Printf("generator: starting %d worker(s) + quality sweep, provider route: %s", n, strings.Join(route, " → "))
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			g.superviseLoop(ctx, fmt.Sprintf("generator worker %d", id), func(ctx context.Context) bool {
				return g.runWorkerLoop(ctx, id)
			})
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.superviseLoop(ctx, "quality sweep", g.runSweepLoop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		g.superviseLoop(ctx, "stuck-job reaper", g.runReaperLoop)
	}()
	wg.Wait()
	log.Println("generator: all workers stopped")
}

// superviseLoop runs loop until ctx is cancelled, restarting it after a 5s
// pause whenever it returns because of a recovered panic.
func (g *GeneratorService) superviseLoop(ctx context.Context, name string, loop func(context.Context) (panicked bool)) {
	for {
		panicked := loop(ctx)
		if ctx.Err() != nil {
			return
		}
		if panicked {
			log.Printf("%s: loop recovered from a panic — restarting in 5s", name)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// workerPollEvery is the FALLBACK poll of an idle worker (R-5b): workers
// are woken immediately by notifyWorkers — on a local enqueue and on a
// gen_jobs NOTIFY received by the database listener (Wake) — so the poll
// only picks up retries whose backoff (not_before) has expired and covers a
// lost notification. An idle queue is therefore queried at most once per
// workerPollEvery per worker.
const workerPollEvery = 3 * time.Minute

// runWorkerLoop is one worker's loop. It reports (via the return value)
// whether it ended because of a panic (true — the caller restarts it) or
// because the context was cancelled (false — clean shutdown). executeJob
// additionally recovers panics per-job, so a panic normally never escapes
// it; the loop-level recover is the last line of defence.
//
// On every wake-up (signal or fallback tick) the worker DRAINS the queue:
// it claims and runs jobs back to back until nothing is due, instead of
// handling one job per tick.
func (g *GeneratorService) runWorkerLoop(ctx context.Context, id int) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("generator worker %d: PANIC recovered: %v", id, r)
			panicked = true
		}
	}()
	// Only worker 1 runs the fallback timer (one idle poll per
	// workerPollEvery for the whole pool); a successful claim passes the
	// wake signal on to the other workers.
	var tick <-chan time.Time
	if id == 1 {
		ticker := time.NewTicker(workerPollEvery)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		g.drainQueue(ctx)
		select {
		case <-ctx.Done():
			log.Printf("generator worker %d stopped", id)
			return false
		case <-g.wake:
		case <-tick:
		}
	}
}

// drainQueue claims and executes due jobs one after
// another until the queue has nothing due (or ctx is cancelled). After a
// successful claim it passes the wake signal on, so another idle worker
// also looks at the queue — a burst of N jobs is spread over the pool
// within milliseconds instead of one job per fallback tick.
func (g *GeneratorService) drainQueue(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := g.claimDue(ctx)
		if err != nil || job == nil {
			return
		}
		g.notifyWorkers()
		g.executeJob(ctx, job)
	}
}

// qualitySweepEvery: how often stored questions are audited (local, free)
// and the flagged ones rewritten.
const qualitySweepEvery = 5 * time.Minute

// runSweepLoop is the dedicated low-priority quality-sweep goroutine. It is
// separate from the generation workers, so a long sweep (bounded by
// jobTimeout) never delays a user's test; and it is skipped (and aborted
// between repair batches) while urgent user jobs are due or running, so it
// never competes with them for the Groq quota.
func (g *GeneratorService) runSweepLoop(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("quality sweep: PANIC recovered: %v", r)
			panicked = true
		}
	}()
	sweep := time.NewTicker(qualitySweepEvery)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-sweep.C:
			if g.generationQueueBusy(ctx) {
				log.Printf("quality sweep: postponed — the generation queue is not empty")
				continue
			}
			g.qualitySweepTick(ctx)
		}
	}
}

// urgentWorkPending reports whether user-facing jobs are due or running.
// A database error counts as "busy" (skip this sweep tick, retry later).
func (g *GeneratorService) urgentWorkPending(ctx context.Context) bool {
	if g == nil || g.gen == nil {
		return false
	}
	busy, err := g.gen.HasUrgentWork(ctx)
	if err != nil {
		log.Printf("quality sweep: check urgent work: %v", err)
		return true
	}
	return busy
}

// generationQueueBusy reports whether any generation job is due or
// running: the sweep starts only when the queue is empty. A database error
// counts as "busy" (skip this tick).
func (g *GeneratorService) generationQueueBusy(ctx context.Context) bool {
	if g == nil || g.gen == nil {
		return false
	}
	busy, err := g.gen.HasActiveJobs(ctx)
	if err != nil {
		log.Printf("quality sweep: check generation queue: %v", err)
		return true
	}
	return busy
}

// qualitySweepTick runs one bounded sweep over not-yet-audited questions.
//
// During a zero-downtime deploy two instances run at once; both sweeping
// the same unchecked questions would pay twice for the same AI repairs. The
// sweep therefore runs under a cluster-wide advisory lock (try-lock): the
// instance that does not get it simply skips this tick.
func (g *GeneratorService) qualitySweepTick(ctx context.Context) {
	sctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	ran, n, err := g.runQualitySweepExclusive(sctx)
	if err != nil {
		log.Printf("quality sweep: %v", err)
	}
	if !ran {
		log.Printf("quality sweep: skipped — another instance holds the sweep lock")
		return
	}
	if n > 0 {
		log.Printf("quality sweep: %d stored question(s) rewritten", n)
	}
}

// runQualitySweepExclusive runs RunQualitySweep under the quality-sweep
// advisory lock. ran = false when another instance is sweeping right now.
func (g *GeneratorService) runQualitySweepExclusive(ctx context.Context) (ran bool, n int, err error) {
	ran, err = g.gen.TryQualitySweepLock(ctx, func(lctx context.Context) error {
		var serr error
		n, serr = g.RunQualitySweep(lctx)
		return serr
	})
	return ran, n, err
}

// reaperInterval is how often reapStuckJobs runs (cfg.ReaperInterval).
func (g *GeneratorService) reaperInterval() time.Duration {
	if g.cfg != nil && g.cfg.ReaperInterval > 0 {
		return g.cfg.ReaperInterval
	}
	return config.DefaultReaperInterval
}

// runReaperLoop runs the stuck-job reaper once at start and then on its own
// ticker (R-5c), not on every worker wake-up: an idle queue costs one cheap
// UPDATE (served by the partial index on running jobs) per interval.
func (g *GeneratorService) runReaperLoop(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("stuck-job reaper: PANIC recovered: %v", r)
			panicked = true
		}
	}()
	g.reapStuckJobs(ctx)
	t := time.NewTicker(g.reaperInterval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			g.reapStuckJobs(ctx)
		}
	}
}

// reapStuckJobs returns 'running' jobs whose worker died (deploy/restart/OOM)
// back to 'pending', so a crashed generation never leaves a test hanging in
// ⏳ «Минуточку...» forever.
// A job that already exhausted maxJobAttempts is parked as failed instead of
// being re-queued (a job that crashes the process must not loop forever).
func (g *GeneratorService) reapStuckJobs(ctx context.Context) {
	n, failed, err := g.gen.ResetStuckRunningJobs(ctx, stuckJobTimeout, maxJobAttempts)
	if err != nil {
		log.Printf("generator: reap stuck jobs: %v", err)
		return
	}
	if n > 0 {
		log.Printf("generator: re-queued %d stuck running job(s)", n)
	}
	if failed > 0 {
		log.Printf("generator: parked %d stuck job(s) as failed (attempts exhausted)", failed)
	}
}

// executeJob runs one claimed job and records its outcome: done, failed
// (retry with backoff) or — when ctx was cancelled by a shutdown — released
// back to the queue without spending an attempt.
func (g *GeneratorService) executeJob(ctx context.Context, job *models.GenerationJob) {
	log.Printf("generator: running job %d (kind=%s subject=%d test=%d owner=%d urgent=%v, attempt %d)",
		job.ID, job.Kind, job.SubjectID, job.TestNumber, job.OwnerUserID, job.Urgent, job.Attempts)

	// A 'running' job that stops being touched for stuckJobTimeout is
	// re-queued by the reaper. The heartbeat keeps updated_at fresh while the
	// (minutes-long) model call is in flight — otherwise the reaper could
	// re-queue a job that is actually alive and healthy.
	heartbeatDone := make(chan struct{})
	go g.jobHeartbeat(ctx, job.ID, heartbeatDone)
	defer close(heartbeatDone)

	// Per-job outcome (strategy, AI calls, rejects, difficulty violations,
	// tokens) — filled by runJob/runSteps, recorded below for A/B analysis.
	run := &genRun{kind: job.Kind}
	started := g.now()
	testID, runErr := func() (testID int64, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("job %d panicked: %v", job.ID, r)
			}
		}()
		jobCtx, cancel := context.WithTimeout(withGenRun(ctx, run), jobTimeout)
		defer cancel()
		return g.runJob(jobCtx, job)
	}()
	if run.strategy == "" {
		run.strategy = "none" // resolved without generation (test already existed)
	}
	if runErr != nil && ctx.Err() != nil {
		// Shutdown (SIGTERM / deploy) interrupted the job: it did not fail.
		// Hand it back to the queue right away (pending, not_before = now(),
		// attempts NOT increased) so the next instance picks it up instantly
		// instead of waiting for the stuck-job reaper. ctx is already
		// cancelled, so a short detached context is used for the update.
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		if rerr := g.gen.ReleaseJob(rctx, job.ID); rerr != nil {
			log.Printf("generator: release job %d on shutdown: %v", job.ID, rerr)
		} else {
			log.Printf("generator: job %d interrupted by shutdown — returned to the queue", job.ID)
		}
		return
	}
	// The outcome is recorded with a context detached from the worker's:
	// a shutdown that lands right after a SUCCESSFUL run used to make
	// CompleteJob fail with «context canceled» — the job stayed 'running'
	// until the stuck-job reaper re-queued it and spent another attempt.
	bctx, bcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bcancel()
	if runErr != nil && errors.Is(runErr, deepseek.ErrBudgetExceeded) {
		// R-9: every free provider failed and the paid fallback is capped
		// for today. Not a real failure — park the job (attempt not
		// counted) and retry later: the free Groq quota may come back, and
		// the DeepSeek budget resets at the next UTC day.
		until := g.now().Add(retryDelay)
		if g.budget != nil {
			if next := g.budget.NextDay(); next.Before(until) || g.gq == nil {
				until = next
			}
		}
		log.Printf("generator: job %d deferred until %s — DeepSeek daily cap reached and no free provider succeeded", job.ID, until.Format(time.RFC3339))
		if derr := g.gen.DeferJob(bctx, job.ID, until); derr != nil {
			log.Printf("generator: defer job %d: %v", job.ID, derr)
		}
		return
	}
	g.recordOutcome(bctx, run, runErr == nil, g.now().Sub(started))
	if runErr != nil {
		log.Printf("generator: job %d failed: %v", job.ID, runErr)
		if ferr := g.gen.FailJob(bctx, job.ID, runErr, retryDelay, maxJobAttempts); ferr != nil {
			log.Printf("generator: fail job %d: %v", job.ID, ferr)
		}
		g.jobFinished(job)
		return
	}
	if err := g.gen.CompleteJob(bctx, job.ID, testID); err != nil {
		log.Printf("generator: complete job %d: %v", job.ID, err)
	}
	repositories.InvalidateChainCache(job.SubjectID)
	g.jobFinished(job)
	if job.Kind == models.JobKindTopicBatch {
		log.Printf("generator: job %d done -> bank topic %q", job.ID, job.TopicKey)
	} else {
		log.Printf("generator: job %d done -> test %d", job.ID, testID)
	}
	g.queueChainTranslation(bctx, job, testID)
}

// queueChainTranslation queues the Kazakh translation of a freshly generated
// CHAIN test right away (R-4): chain tests are shared by every student of
// the subject, so Kazakh-speaking students will open it anyway — with the
// translation done in advance they never see «⏳ Перевод готовится…».
// Language subjects are never translated. Failures are only logged: the
// translation is then queued on the first Kazakh open.
func (g *GeneratorService) queueChainTranslation(ctx context.Context, job *models.GenerationJob, testID int64) {
	if job.Kind != models.TestKindChain || testID <= 0 || g.translator == nil || !g.translator.Enabled() {
		return
	}
	subject, err := g.subjects.GetByID(ctx, job.SubjectID)
	if err != nil {
		log.Printf("generator: queue kk translation of test %d: subject %d: %v", testID, job.SubjectID, err)
		return
	}
	if models.IsLanguageSubject(subject.Name) {
		return
	}
	if err := g.translator.RequestTranslation(ctx, testID); err != nil {
		log.Printf("generator: queue kk translation of test %d: %v", testID, err)
		return
	}
	log.Printf("generator: queued kk translation of chain test %d", testID)
}

// claimDue claims the next due job. Errors are logged and returned (the
// worker stops draining and retries on the next wake-up / fallback tick) —
// a transient database hiccup must never stop queue processing. A job
// executed by executeJob NEVER propagates a panic: a crashed job is marked
// failed (retryable) and the worker keeps draining the queue.
func (g *GeneratorService) claimDue(ctx context.Context) (*models.GenerationJob, error) {
	job, err := g.gen.ClaimNextJob(ctx)
	if err != nil {
		log.Printf("generator: claim job: %v", err)
		return nil, err
	}
	return job, nil
}

// jobHeartbeat refreshes the job's updated_at every 2 minutes while the job
// runs, so the stuck-job reaper never mistakes a slow-but-alive generation
// for a dead one (and never double-generates a paid test).
func (g *GeneratorService) jobHeartbeat(ctx context.Context, jobID int64, done <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.gen.TouchRunningJob(ctx, jobID); err != nil {
				log.Printf("generator: heartbeat job %d: %v", jobID, err)
			}
		}
	}
}

// runJob executes the full pipeline for one job and returns the test id.
func (g *GeneratorService) runJob(ctx context.Context, job *models.GenerationJob) (int64, error) {
	subject, err := g.subjects.GetByID(ctx, job.SubjectID)
	if err != nil {
		return 0, err
	}
	if job.Kind == models.JobKindTopicBatch {
		// B4: bank questions on one catalog topic, no test row (test_id 0).
		if r := genRunFrom(ctx); r != nil {
			r.strategy = strategyTopicBatch
		}
		return 0, g.runTopicBatch(ctx, job, subject.Name)
	}
	// B1: topic catalog of the subject — the model picks topics from it, the
	// validators map aliases to canonical titles.
	catalog, err := g.gen.TopicCatalog(ctx, job.SubjectID)
	if err != nil {
		return 0, err
	}

	spec := &genSpec{kind: job.Kind, subjectName: subject.Name, catalog: catalog}
	var title, fingerprint string
	var ownerUserID int64
	var topicKeys []string

	switch job.Kind {
	case models.TestKindChain:
		spec.testNumber = job.TestNumber
		// Never pay for a test that already exists. The enqueue path is
		// guarded, but a retried job may have actually succeeded on a
		// previous attempt (crash between the DB write and the job update).
		if existing, err := g.findChainTest(ctx, job.SubjectID, spec.testNumber); err != nil {
			return 0, err
		} else if existing != nil {
			log.Printf("generator: chain test %d of subject %d already exists (id %d) — job %d resolved with no AI call", spec.testNumber, job.SubjectID, existing.ID, job.ID)
			return existing.ID, nil
		}
		title = fmt.Sprintf("Тест %d", spec.testNumber)
		var prev []models.Question
		var marks []float64
		var prevTestID int64
		if spec.testNumber > 1 {
			prevTest, err := g.findChainTest(ctx, job.SubjectID, spec.testNumber-1)
			if err != nil {
				return 0, err
			}
			if prevTest != nil {
				prevTestID = prevTest.ID
				prev, err = g.subjects.TestQuestions(ctx, prevTest.ID)
				if err != nil {
					return 0, err
				}
				// Chain tests are SHARED by every student of the subject, so
				// the marks are the average level of ALL students who answered
				// each question — not of the one student who happened to
				// trigger the generation first.
				marks, err = g.questionMarks(ctx, prev)
				if err != nil {
					return 0, err
				}
			}
		}
		spec.prevStems = questionStems(prev)
		// Repeats are checked locally (no prompt tokens) against the older
		// chain tests too — the prompt shows only the previous test, so a
		// question of Тест n-2…n-20 used to come back unnoticed.
		older, err := g.olderChainStems(ctx, job.SubjectID, spec.testNumber)
		if err != nil {
			return 0, err
		}
		spec.prevStems = append(spec.prevStems, older...)
		spec.prevMean = meanDifficulty(prev)
		// Weak topics: of the previous test (average mark per topic) +
		// subject-wide (aggregated user_topic_stats of active students).
		spec.weak = chainWeakTopics(prevTopicMarks(prev, marks), g.subjectWeakTitles(ctx, job.SubjectID), chainWeakLimit)
		for _, w := range spec.weak {
			if k, ok := catalog.Resolve(w); ok {
				topicKeys = append(topicKeys, k)
			} else {
				topicKeys = append(topicKeys, models.NormalizeTopic(w))
			}
		}
		fingerprint = models.ChainFingerprint(job.SubjectID, spec.testNumber, prevTestID, topicKeys)
		spec.basePrompt = chainGenContext(subject.Name, spec.testNumber, prev, marks, spec.weak) + topicListPrompt(catalog)

	case models.TestKindPersonal:
		ownerUserID = job.OwnerUserID
		if ownerUserID <= 0 {
			return 0, fmt.Errorf("personal job %d without owner_user_id", job.ID)
		}
		// The user may already HAVE their personal test (created after this
		// job was queued — e.g. the previous attempt succeeded but the crash
		// came before the job was marked done). A paid regeneration would
		// also hit the (subject, owner) unique index. Resolve for free.
		if existing, err := g.gen.FindPersonalTest(ctx, job.SubjectID, ownerUserID); err != nil {
			return 0, err
		} else if existing != nil {
			log.Printf("generator: user %d already has personal test %d in subject %d — job %d resolved with no AI call", ownerUserID, existing.ID, job.SubjectID, job.ID)
			return existing.ID, nil
		}
		weak, err := g.gen.WeakTopicStats(ctx, ownerUserID, job.SubjectID, weakTopicsCount)
		if err != nil {
			return 0, err
		}
		if len(weak) == 0 {
			return 0, fmt.Errorf("user %d has no weak topics in subject %d", ownerUserID, job.SubjectID)
		}
		for _, w := range weak {
			spec.personalTopics = append(spec.personalTopics, w.Topic)
			topicKeys = append(topicKeys, w.Key)
		}
		title = personalTestTitle
		fingerprint = models.WeakTopicsFingerprint(job.SubjectID, topicKeys)
		spec.basePrompt = personalGenContext(subject.Name, spec.personalTopics)

	default:
		return 0, fmt.Errorf("unknown job kind %q", job.Kind)
	}

	// Fingerprint templates: the same input was already generated (for
	// another user / a previous run of this chain slot) — clone it, no AI.
	if id, ok := g.cloneFromTemplate(ctx, job, fingerprint, title, ownerUserID); ok {
		return id, nil
	}

	strategy := g.chooseStrategy(job)
	run := &genRun{strategy: strategy, kind: job.Kind}
	if r := genRunFrom(ctx); r != nil {
		run = r
		run.strategy, run.kind = strategy, job.Kind
	} else {
		ctx = withGenRun(ctx, run)
	}
	if job.ID > 0 {
		if err := g.gen.SetJobStrategy(ctx, job.ID, strategy); err != nil {
			log.Printf("generator: job %d: record strategy: %v", job.ID, err)
		}
	}
	log.Printf("generator: job %d (%s) strategy=%s weak=%v", job.ID, job.Kind, strategy, append(spec.weak, spec.personalTopics...))

	var final *generatedTest
	task := fmt.Sprintf("gen %s job %d", job.Kind, job.ID)
	if strategy == strategyBatch {
		if job.Kind == models.TestKindChain {
			spec.slots = planChainSlots(spec.testNumber, spec.weak, GeneratedQuestionsPerTest, g.batchSize())
		} else {
			spec.slots = planPersonalSlots(spec.personalTopics, GeneratedQuestionsPerTest, g.batchSize())
		}
		final, err = g.generateBatched(ctx, job, spec)
		if err != nil {
			return 0, fmt.Errorf("generate (batch): %w", err)
		}
		// The batches were validated question by question; the whole test
		// still has to pass the test-level contract below.
	} else {
		final, err = g.generateFull(ctx, job, spec, task)
		if err != nil {
			return 0, err
		}
	}

	// Post-validation: every flagged question is rewritten by the model and
	// re-audited before the test is stored; a test that still contains a
	// giveaway question is NEVER shown to a student (the job is retried).
	if err := g.repairFlagged(ctx, task, subject.Name, final); err != nil {
		return 0, fmt.Errorf("quality: %w", err)
	}
	// The rewrites keep topics, but re-check the whole contract anyway
	// (duplicates across questions, key balance, weak-topic coverage,
	// difficulty, repeats of the previous test).
	if err := g.validateFinal(final, spec); err != nil {
		noteReject(ctx, "final", err)
		if strategy == strategyBatch && job.ID > 0 {
			// Do not let a retry reuse a combination that fails as a whole.
			_ = g.gen.DeleteJobBatches(ctx, job.ID)
		}
		return 0, fmt.Errorf("after repair: %w", err)
	}

	// Collect the topic list of the final test for weak-topic analysis.
	topicSet := map[string]bool{}
	var testTopics []string
	for _, q := range final.Questions {
		if q.Topic != "" && !topicSet[q.Topic] {
			topicSet[q.Topic] = true
			testTopics = append(testTopics, q.Topic)
		}
	}
	sort.Strings(testTopics)

	test := &models.Test{
		SubjectID:         job.SubjectID,
		TestNumber:        spec.testNumber,
		Title:             title,
		Kind:              job.Kind,
		Topics:            testTopics,
		OwnerUserID:       ownerUserID,
		TopicsFingerprint: fingerprint,
	}
	if job.Kind == models.TestKindPersonal {
		test.TestNumber = 0 // assigned by the DB: next personal number
	}
	seed := final.toSeed()
	stored, err := g.gen.CreateGeneratedTest(ctx, test, seed)
	if err != nil {
		return 0, err
	}
	g.saveTemplate(ctx, job, fingerprint, spec.testNumber, testTopics, seed, stored.ID, ownerUserID, strategy)
	return stored.ID, nil
}

// generateFull is the original one-call strategy: the model writes all 20
// questions; the reply is validated as a whole (and rejected as a whole).
// A rejected reply's reason is fed back to the next provider step.
func (g *GeneratorService) generateFull(ctx context.Context, job *models.GenerationJob, spec *genSpec, task string) (*generatedTest, error) {
	var prompt string
	if job.Kind == models.TestKindChain {
		prompt = spec.basePrompt + chainOutputLine
	} else {
		prompt = spec.basePrompt + personalOutputLine
	}
	var feedback string
	messages := func() []deepseek.Message {
		m := []deepseek.Message{
			{Role: "system", Content: genSystemPrompt},
			{Role: "user", Content: prompt},
		}
		if feedback != "" {
			m[1].Content += "\nВНИМАНИЕ: предыдущий вариант этого теста ОТКЛОНЁН проверкой: " + feedback + ". Напиши тест заново и исправь именно это."
		}
		return m
	}
	var final *generatedTest
	validate := func(raw string) error {
		gt, err := parseTestJSON(raw)
		if err != nil {
			return err
		}
		if err := g.validateReply(gt, spec); err != nil {
			return err
		}
		// Quality gate: a reply where many questions give the answer away
		// by the options' format (only the key has the dash, the key is the
		// only filled-in gap, duplicate options, «все ответы верны»…) is
		// sloppy as a whole — let the next provider write it from scratch.
		// A few flagged questions are rewritten afterwards (targeted repair).
		if n := countHard(auditGenerated(gt)); n > maxHardFlaggedPerReply {
			return rejectf(rejectQuality, "quality audit: %d of %d questions reveal the answer or have broken options", n, len(gt.Questions))
		}
		final = gt
		return nil
	}
	if _, _, err := runStepsFeedback(ctx, task, g.generationStepsDyn(messages, job.Kind, job.Attempts), validate,
		func(e error) { feedback = e.Error() }); err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}
	return final, nil
}

// validateReply runs the kind-specific contract on a model reply (full
// strategy: at every step, not only at the end).
func (g *GeneratorService) validateReply(gt *generatedTest, spec *genSpec) error {
	switch spec.kind {
	case models.TestKindPersonal:
		// Weak-topics tests carry a strict contract: only the requested
		// topics, all of them covered — a sloppy reply never reaches the DB.
		if err := validatePersonalCoverage(gt, spec.personalTopics, spec.catalog); err != nil {
			return rejectf(rejectTopics, "personal test: %v", err)
		}
	case models.TestKindChain:
		// Chain tests must match the difficulty of their chain position:
		// a model that writes a beginner test for Тест 40 (or an olympiad
		// for Тест 2) is rejected and the next provider writes it.
		if err := validateChainDifficulty(gt, spec.testNumber); err != nil {
			return err
		}
		if err := validateChainProgression(gt, spec.testNumber, spec.prevMean); err != nil {
			return err
		}
		if err := validateNoRepeats(gt, spec.prevStems); err != nil {
			return err
		}
		if err := validateChainTopics(gt, spec.catalog); err != nil {
			return rejectf(rejectTopics, "%v", err)
		}
		if err := validateWeakCoverage(gt, spec.weak, spec.catalog); err != nil {
			return err
		}
	}
	return nil
}

// validateFinal is the test-level contract of the assembled/repaired test.
func (g *GeneratorService) validateFinal(gt *generatedTest, spec *genSpec) error {
	if err := validateTest(gt); err != nil {
		return rejectf(rejectFormat, "%v", err)
	}
	// Repairs may introduce a near-duplicate or move a topic; re-check
	// every rule of the kind.
	return g.validateReply(gt, spec)
}

// subjectWeakTitles returns the subject-wide weak topics (titles) of the
// active students, cached briefly per subject (one aggregate query per
// subject per few minutes, however many chain jobs run).
func (g *GeneratorService) subjectWeakTitles(ctx context.Context, subjectID int64) []string {
	if g.gen == nil {
		return nil
	}
	if v, ok := subjectWeakCache.get(subjectID); ok {
		return v
	}
	rows, err := g.gen.SubjectWeakTopics(ctx, subjectID, subjectWeakWindow, subjectWeakMinUsers, chainWeakLimit)
	if err != nil {
		log.Printf("generator: subject %d weak topics: %v", subjectID, err)
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	subjectWeakCache.put(subjectID, out)
	return out
}

// generationSteps returns the ordered provider route for one generation
// with fixed messages (topic batches, tests).
func (g *GeneratorService) generationSteps(messages []deepseek.Message, kind string, attempts int) []aiStep {
	return g.generationStepsDyn(func() []deepseek.Message { return messages }, kind, attempts)
}

// generationStepsDyn returns the provider route; messages is evaluated when
// a step runs (so a rejected reply's feedback reaches the next step).
//
// Route (audit: at most TWO Groq attempts, then the paid DeepSeek):
//
//	chain, first attempt: GPT-OSS 120B (medium) → Qwen 3.8 27B → DeepSeek (high)
//	chain retry / personal / topic batch: GPT-OSS 120B (low) → Qwen → DeepSeek (low)
//
// Qwen is the second attempt (not GPT-OSS at a lower effort): it has its
// own free quota and fails differently, so a bad GPT-OSS reply is not
// followed by a near-identical one.
func (g *GeneratorService) generationStepsDyn(messages func() []deepseek.Message, kind string, attempts int) []aiStep {
	retry := attempts > 1
	var steps []aiStep
	if g.gq != nil {
		ossEffort := groq.EffortLow
		if kind == models.TestKindChain && !retry {
			ossEffort = groq.EffortMedium
		}
		oss := groqFullStep(g.gq, groq.ModelGPTOSS120B, ossEffort, 0, 0, messages)
		qw := groqFullStep(g.gq, groq.ModelQwen27B, groq.EffortNone, 0.7, 0.8, messages) // instruct mode: no reasoning tokens
		steps = append(steps, oss, qw)
	}
	if g.ds != nil {
		effort := deepseek.ThinkingEffortHigh
		if kind == models.TestKindPersonal || kind == models.JobKindTopicBatch || retry {
			effort = deepseek.ThinkingEffortLow
		}
		ds := g.ds
		steps = append(steps, aiStep{
			name:    "deepseek/" + ds.ReasonerModel() + "(" + effort + ")",
			reserve: deepseekGenReserve,
			run: func(ctx context.Context) (string, error) {
				// Bound the paid calls in flight across the worker pool:
				// more workers must not mean a proportional DeepSeek burst.
				release, err := g.acquireDeepSeek(ctx)
				if err != nil {
					return "", err
				}
				defer release()
				return ds.GenerateJSON(ctx, messages(), genMaxTokens, effort)
			},
		})
	}
	return steps
}

// groqFullStep is a Groq step of a full (20/10-question) generation.
func groqFullStep(gc *groq.Client, model, effort string, temp, topP float64, messages func() []deepseek.Message) aiStep {
	name := "groq/" + model
	if effort != "" {
		name += "(" + effort + ")"
	}
	return aiStep{name: name, timeout: groqStepTimeout, run: func(ctx context.Context) (string, error) {
		res, err := gc.ChatJSON(ctx, groq.Request{
			Model:       model,
			Messages:    toGroqMessages(messages()),
			MaxTokens:   genMaxTokens,
			MinTokens:   groqGenMinTokens,
			Effort:      effort,
			Temperature: temp,
			TopP:        topP,
			Schema:      testJSONSchema,
			SchemaName:  "ent_test",
			MaxWait:     groqGenMaxWait,
		})
		if err != nil {
			return "", err
		}
		noteTokens(ctx, res.PromptTokens, res.CompletionTokens)
		return res.Content, nil
	}}
}

// testJSONSchema is the Structured Outputs schema of a generated test
// (strict mode: every field required, no extra properties). Groq's
// constrained decoding then guarantees a parseable reply; the semantic
// checks (count, balance, duplicates, topics) stay in validateTest.
var testJSONSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"questions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question":      map[string]any{"type": "string"},
					"options":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"correct_index": map[string]any{"type": "integer"},
					"topic":         map[string]any{"type": "string"},
					"difficulty":    map[string]any{"type": "integer"},
				},
				"required":             []string{"question", "options", "correct_index", "topic", "difficulty"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"questions"},
	"additionalProperties": false,
}

// questionMarks maps each question of the previous chain test to the
// AVERAGE knowledge level of every student who answered it (0=🔴 не знают,
// 1=🟡 в процессе, 2=🟢 закреплено), rounded to 0.1; -1 = nobody answered.
func (g *GeneratorService) questionMarks(ctx context.Context, questions []models.Question) ([]float64, error) {
	marks := make([]float64, len(questions))
	if len(questions) == 0 {
		return marks, nil
	}
	ids := make([]int64, len(questions))
	for i, q := range questions {
		ids[i] = q.ID
	}
	avg, err := g.subjects.AverageQuestionStatuses(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, q := range questions {
		if v, ok := avg[q.ID]; ok {
			marks[i] = math.Round(v*10) / 10
		} else {
			marks[i] = -1
		}
	}
	return marks, nil
}

// chainRepeatWindow: a new chain test may not repeat a question of any of
// the previous chainRepeatWindow chain tests (the previous one included).
const chainRepeatWindow = 20

// olderChainStems returns the question stems of chain tests
// testNumber-chainRepeatWindow … testNumber-2 (the previous test is loaded
// separately, with its knowledge marks, for the prompt).
func (g *GeneratorService) olderChainStems(ctx context.Context, subjectID int64, testNumber int) ([]string, error) {
	if testNumber <= 2 {
		return nil, nil
	}
	tests, err := g.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range tests {
		n := tests[i].TestNumber
		if n >= testNumber-1 || n < testNumber-chainRepeatWindow {
			continue
		}
		qs, err := g.subjects.TestQuestions(ctx, tests[i].ID)
		if err != nil {
			return nil, err
		}
		out = append(out, questionStems(qs)...)
	}
	return out, nil
}

// findChainTest locates the chain test by number (nil if not generated yet).
func (g *GeneratorService) findChainTest(ctx context.Context, subjectID int64, number int) (*models.Test, error) {
	tests, err := g.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	for i := range tests {
		if tests[i].TestNumber == number {
			return &tests[i], nil
		}
	}
	return nil, nil
}

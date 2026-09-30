// Package services — AI test generation pipeline.
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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Bihan293/Juz40-test2/internal/config"
	"github.com/Bihan293/Juz40-test2/internal/deepseek"
	"github.com/Bihan293/Juz40-test2/internal/groq"
	"github.com/Bihan293/Juz40-test2/internal/models"
	"github.com/Bihan293/Juz40-test2/internal/repositories"
)

const (
	// GeneratedQuestionsPerTest is the fixed size of every AI-generated test.
	GeneratedQuestionsPerTest = 20
	// weakTopicsCount is how many weak topics go into one weak test.
	weakTopicsCount = 5
	// maxJobAttempts caps retries of a failing generation job.
	maxJobAttempts = 3
	// retryDelay is the backoff applied between job retries.
	retryDelay = 10 * time.Minute
	// jobTimeout bounds a single generation run. It covers the whole
	// provider route (Groq steps, a possible wait for the per-minute Groq
	// window, then the DeepSeek fallback) and stays well below
	// stuckJobTimeout (the heartbeat keeps a live job fresh anyway).
	jobTimeout = 8 * time.Minute
	// stuckJobTimeout: a 'running' job idle longer than this is returned to
	// 'pending' (the worker died mid-generation — deploy, restart, OOM).
	stuckJobTimeout = 10 * time.Minute
	// genMaxTokens caps the model output INCLUDING the hidden thinking
	// tokens — the hard cost limiter of one generation. 20 questions with
	// 4 options each need ~3000–3800 visible tokens, leaving ~4000 for the
	// thinking pass; the worst-case spend of one call at peak flash pricing
	// is ~$0.0096.
	genMaxTokens = 8000

	// groqGenMinTokens is the smallest output budget a Groq generation
	// request may run with: 20 Russian questions need ~3000–3800 visible
	// tokens plus some reasoning. If the prompt leaves less than that under
	// the free-tier per-request ceiling (TPM 8000), the Groq step is skipped
	// without an HTTP call and the next provider runs.
	groqGenMinTokens = 4500
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
}

func NewGeneratorService(ds *deepseek.Client, cfg *config.Config, gen *repositories.GenerationRepository, subjects *repositories.SubjectRepository, state *repositories.StateRepository) *GeneratorService {
	return &GeneratorService{
		ds: ds, cfg: cfg, gen: gen, subjects: subjects, state: state,
		now: time.Now,
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

// carryTranslations copies the cached Kazakh translations of a clone source
// test onto the clone's fresh question ids. Pure DB work — zero API cost.
// Failures are logged and ignored: the worst case is the translation being
// produced on first Kazakh open of the clone (the previous behaviour).
func (g *GeneratorService) carryTranslations(ctx context.Context, srcTestID, dstTestID int64) {
	if g.translator == nil || !g.translator.Enabled() || dstTestID == srcTestID {
		return
	}
	dstIDs, err := g.translator.QuestionIDsForTest(ctx, dstTestID)
	if err != nil || len(dstIDs) == 0 {
		return
	}
	n, err := g.translator.CopyTranslationsToTest(ctx, srcTestID, dstIDs)
	if err != nil {
		log.Printf("generator: carry kk translations %d -> %d: %v", srcTestID, dstTestID, err)
		return
	}
	if n > 0 {
		log.Printf("generator: carried %d cached kk translation(s) to cloned test %d (no AI call)", n, dstTestID)
	}
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

const genSystemPrompt = `Ты — автор тестов ЕНТ/УБТ (Казахстан) для школьников 9–11 классов. Пишешь на русском, в официальном стиле ЕНТ: точные однозначные формулировки, без двойных отрицаний, строго по школьной программе.

Требования к каждому вопросу:
1. Ровно один правильный ответ; 4 варианта; дистракторы правдоподобные (типичные ошибки), не абсурдные.
2. Варианты однородны по длине и форме; правильный не выделяется.
3. Позиции правильных ответов равномерно по 0–3 в рамках теста.
4. topic — короткая тема (1–4 слова), difficulty — 1–5.
5. Перед ответом мысленно проверь каждый correct_index: он обязан указывать на действительно правильный вариант.

Формат — строго JSON, без пояснений и markdown:
{"questions":[{"question":"...","options":["...","...","...","..."],"correct_index":0,"topic":"...","difficulty":2}]}`

// chainGenPrompt builds the user prompt for the next chain test. The model
// sees the PREVIOUS test as compact lines "тема — короткая суть вопроса —
// уровни ученика" (0=не знает, 1=в процессе, 2=закреплено) and must raise
// the difficulty gently, target the weak topics and never repeat questions.
func chainGenPrompt(subjectName string, testNumber int, prev []models.Question, marks []int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Предмет: «%s». Составь Тест №%d из ровно %d вопросов.\n\n", subjectName, testNumber, GeneratedQuestionsPerTest)

	if testNumber <= 1 || len(prev) == 0 {
		b.WriteString("Это ПЕРВЫЙ тест цепочки: базовый уровень (difficulty 1–2), фундаментальные темы программы 9–11 классов — то, без чего нельзя начинать подготовку к ЕНТ/УБТ.\n")
	} else {
		fmt.Fprintf(&b, "Прошлый Тест №%d (тема — суть вопроса — уровень ученика: 0=не знает, 1=в процессе, 2=закреплено):\n", testNumber-1)
		for i, q := range prev {
			stem := q.Text
			if len(stem) > 80 {
				stem = stem[:80] + "…"
			}
			mark := 0
			if i < len(marks) {
				mark = marks[i]
			}
			fmt.Fprintf(&b, "%d. [%s] %s — %d\n", i+1, q.Topic, stem, mark)
		}
		fmt.Fprintf(&b, "\nТвой Тест №%d ОБЯЗАН:\n", testNumber)
		b.WriteString("— быть лишь НЕМНОГО сложнее прошлого (difficulty " + difficultyBand(testNumber) + "): сложность растёт очень плавно от теста к тесту, без скачков до олимпиадного уровня;\n")
		b.WriteString("— подтягивать слабые места: темы с отметками 0–1 повтори через НОВЫЕ формулировки и другие аспекты, темы с 2 почти не трогай;\n")
		b.WriteString("— НЕ ПОВТОРЯТЬ ни одного вопроса прошлого теста: другие формулировки, подтемы, числа и примеры.\n")
	}
	b.WriteString("\nВыдай строго JSON по схеме, ровно " + fmt.Sprint(GeneratedQuestionsPerTest) + " вопросов.")
	return b.String()
}

// difficultyBand maps the chain position to the target difficulty band.
// The ramp is deliberately slow: even the 10th test stays at school level,
// the top band is reached only deep into the chain.
func difficultyBand(testNumber int) string {
	switch {
	case testNumber <= 2:
		return "1–2"
	case testNumber <= 4:
		return "2–3"
	case testNumber <= 7:
		return "3"
	case testNumber <= 12:
		return "3–4"
	default:
		return "4–5"
	}
}

// personalGenPrompt builds the user prompt for a weak-topics test. ONLY the
// short weak-topic names (a couple of words each, e.g. «Генетика»,
// «Микроорганизмы») are sent — never whole past tests with 40–60 questions:
// the prompt stays tiny and cheap, and the model must tag every question
// with one of THESE topics verbatim (validated server-side afterwards).
func personalGenPrompt(subjectName string, topics []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Предмет: «%s». Составь тренировочный тест из ровно %d вопросов ТОЛЬКО по этим слабым темам ученика:\n", subjectName, GeneratedQuestionsPerTest)
	for i, t := range topics {
		fmt.Fprintf(&b, "%d. %s\n", i+1, t)
	}
	b.WriteString("\nТребования:\n")
	b.WriteString("— вопросы равномерно покрывают КАЖДУЮ из перечисленных тем (примерно поровну на тему), лишних тем нет;\n")
	b.WriteString("— поле topic каждого вопроса ДОСЛОВНО равно одной из перечисленных тем (скопируй строку из списка);\n")
	b.WriteString("— от базовых аспектов к сложным (difficulty 2–4): закрыть пробел, а не завалить;\n")
	b.WriteString("— уровень и формат реального ЕНТ/УБТ для 9–11 классов, без повторов.\n")
	b.WriteString("\nВыдай строго JSON по схеме.")
	return b.String()
}

// normalizeTopic canonicalises a topic string for comparison (the model may
// differ in case or surrounding whitespace even when told to copy verbatim).
func normalizeTopic(t string) string {
	return strings.ToLower(strings.Join(strings.Fields(t), " "))
}

// topicsFingerprint is the stable sha256 of the sorted weak-topic set. Two
// users with the SAME weak topics get the SAME fingerprint — and therefore
// share one generated test (a clone, zero AI cost for the second user).
func topicsFingerprint(topics []string) string {
	norm := make([]string, 0, len(topics))
	seen := map[string]bool{}
	for _, t := range topics {
		n := normalizeTopic(t)
		if n != "" && !seen[n] {
			seen[n] = true
			norm = append(norm, n)
		}
	}
	sort.Strings(norm)
	sum := sha256.Sum256([]byte(strings.Join(norm, "\n")))
	return hex.EncodeToString(sum[:])
}

// validatePersonalCoverage enforces the weak-topics contract server-side:
// every question must be tagged with one of the REQUESTED topics and every
// requested topic must be covered — otherwise the test would silently train
// topics the user already knows (wasted money) or miss actual gaps.
func validatePersonalCoverage(gt *generatedTest, topics []string) error {
	allowed := make(map[string]bool, len(topics))
	for _, t := range topics {
		allowed[normalizeTopic(t)] = true
	}
	covered := make(map[string]bool, len(topics))
	for i := range gt.Questions {
		n := normalizeTopic(gt.Questions[i].Topic)
		if !allowed[n] {
			return fmt.Errorf("question %d: topic %q is not in the weak-topics list", i+1, gt.Questions[i].Topic)
		}
		covered[n] = true
	}
	if len(covered) != len(allowed) {
		return fmt.Errorf("only %d of %d weak topics covered", len(covered), len(allowed))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// validateTest enforces the JSON contract before anything reaches the DB.
func validateTest(gt *generatedTest) error {
	if len(gt.Questions) != GeneratedQuestionsPerTest {
		return fmt.Errorf("need %d questions, got %d", GeneratedQuestionsPerTest, len(gt.Questions))
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
		if n > GeneratedQuestionsPerTest/2 {
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
		})
	}
	return out
}

// parseTestJSON extracts the JSON object from a model reply (tolerates
// accidental markdown fences and leading/trailing prose) and validates it.
func parseTestJSON(raw string) (*generatedTest, error) {
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
	if err := validateTest(&gt); err != nil {
		return nil, err
	}
	return &gt, nil
}

// ---------------------------------------------------------------------------
// Scheduling (off-peak vs urgent)
// ---------------------------------------------------------------------------

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
func (g *GeneratorService) EnsureChainTest(ctx context.Context, subjectID int64, testNumber int, urgent bool, ownerUserID ...int64) (queued bool, err error) {
	if !g.Enabled() {
		return false, nil
	}
	notBefore := time.Now()
	if !urgent {
		notBefore = g.deferredUntil()
	}
	inserted, err := g.gen.EnqueueChainJob(ctx, subjectID, testNumber, notBefore, urgent, ownerUserID...)
	if err != nil {
		return false, err
	}
	if inserted && urgent {
		log.Printf("generator: queued URGENT chain job: subject %d test %d", subjectID, testNumber)
	}
	return g.gen.HasPendingOrRunningChainJob(ctx, subjectID, testNumber)
}

// ReviveChainTest re-queues the generation of a chain test whose job died
// (status='failed' after exhausting retries) or got stuck while the queue
// was broken. It is triggered by the user tapping the ⏳ button — previously
// that tap only showed a toast and changed NOTHING, so a failed job left the
// test in «Минуточку...» forever (nobody ever re-enqueued it). If an active
// job already exists it is simply made urgent; a missing test with no active
// job gets a fresh urgent job (the unique index treats 'failed' rows as
// free slots). The tap is a no-op for tests that already exist.
func (g *GeneratorService) ReviveChainTest(ctx context.Context, subjectID int64, testNumber int, ownerUserID int64) {
	if !g.Enabled() || testNumber < 1 || testNumber > models.MaxVisibleTests {
		return
	}
	// Already generated — nothing to revive (a stale button at worst).
	if existing, err := g.findChainTest(ctx, subjectID, testNumber); err != nil {
		log.Printf("generator: revive lookup subject %d test %d: %v", subjectID, testNumber, err)
		return
	} else if existing != nil {
		return
	}
	revived, err := g.gen.ReviveChainJob(ctx, subjectID, testNumber, ownerUserID)
	if err != nil {
		log.Printf("generator: revive chain job subject %d test %d: %v", subjectID, testNumber, err)
		return
	}
	if revived {
		log.Printf("generator: revived failed chain job: subject %d test %d (urgent)", subjectID, testNumber)
	}
	// Belt and braces: the normal enqueue path (insert or upgrade to urgent).
	if _, err := g.EnsureChainTest(ctx, subjectID, testNumber, true, ownerUserID); err != nil {
		log.Printf("generator: ensure after revive subject %d test %d: %v", subjectID, testNumber, err)
	}
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
	topics, err = g.gen.WeakTopics(ctx, userID, subjectID, weakTopicsCount)
	if err != nil {
		return nil, false, nil, err
	}
	if len(topics) == 0 {
		return nil, false, nil, nil // no weak topics yet
	}
	test, err = g.gen.FindPersonalTest(ctx, subjectID, userID)
	if err != nil {
		return nil, false, nil, err
	}
	if test != nil {
		return test, false, topics, nil
	}
	if !g.Enabled() {
		return nil, false, topics, nil
	}

	fp := topicsFingerprint(topics)
	// Reuse path: someone else (or a previous run) already generated a test
	// for exactly this weak-topics set — clone it instead of calling the AI.
	shared, err := g.gen.FindPersonalTestByFingerprint(ctx, subjectID, fp)
	if err != nil {
		return nil, false, nil, err
	}
	if shared != nil {
		qs, err := g.gen.PersonalTestQuestions(ctx, shared.ID)
		if err != nil {
			return nil, false, nil, err
		}
		cloned, err := g.gen.ClonePersonalTest(ctx, shared, qs, userID)
		if err != nil {
			return nil, false, nil, err
		}
		g.carryTranslations(ctx, shared.ID, cloned.ID)
		log.Printf("generator: cloned personal test %d -> %d for user %d (fingerprint %s, no AI call)", shared.ID, cloned.ID, userID, fp[:12])
		return cloned, false, topics, nil
	}

	if err := g.gen.EnqueuePersonalJob(ctx, subjectID, userID, fp); err != nil {
		return nil, false, nil, err
	}
	pending, err = g.gen.HasPendingOrRunningPersonalJob(ctx, subjectID, userID)
	if err != nil {
		return nil, false, nil, err
	}
	return nil, pending, topics, nil
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

// RunWorker processes the generation queue until ctx is cancelled. One job
// at a time is enough: generation is rare and expensive.
//
// The loop itself is wrapped in a recover: if processOne ever panics, the
// worker must NOT die — a dead worker leaves every queued test hanging in
// ⏳ «Минуточку...» forever. The loop is restarted after a short pause
// (backoff against crash-looping) and keeps draining the queue. Only a
// context cancellation (shutdown) stops the worker for good.
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
	log.Printf("generator worker started, provider route: %s", strings.Join(route, " → "))
	for {
		panicked := g.runWorkerLoop(ctx)
		if ctx.Err() != nil {
			return
		}
		if panicked {
			log.Printf("generator worker: loop recovered from a panic — restarting in 5s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// runWorkerLoop is the worker's ticker loop. It reports (via the return
// value) whether it ended because of a panic (true — the caller restarts
// it) or because the context was cancelled (false — clean shutdown).
// processOne additionally recovers panics per-job, so a panic normally never
// escapes it; the loop-level recover is the last line of defence.
func (g *GeneratorService) runWorkerLoop(ctx context.Context) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("generator worker: PANIC recovered: %v", r)
			panicked = true
		}
	}()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Println("generator worker stopped")
			return false
		case <-ticker.C:
			g.processOne(ctx)
		}
	}
}

// reapStuckJobs returns 'running' jobs whose worker died (deploy/restart/OOM)
// back to 'pending', so a crashed generation never leaves a test hanging in
// ⏳ «Минуточку...» forever.
func (g *GeneratorService) reapStuckJobs(ctx context.Context) {
	n, err := g.gen.ResetStuckRunningJobs(ctx, stuckJobTimeout)
	if err != nil {
		log.Printf("generator: reap stuck jobs: %v", err)
		return
	}
	if n > 0 {
		log.Printf("generator: re-queued %d stuck running job(s)", n)
	}
}

// processOne reaps stuck jobs and runs at most one due job. It NEVER
// propagates a panic: a crashed job is marked failed (retryable), the
// worker stays alive and keeps draining the queue. A propagated panic used
// to kill the whole worker goroutine — with nothing left to consume the
// queue, every waiting test hung in ⏳ «Минуточку...» for good.
func (g *GeneratorService) processOne(ctx context.Context) {
	job, err := g.claimDue(ctx)
	if err != nil || job == nil {
		return
	}
	log.Printf("generator: running job %d (kind=%s subject=%d test=%d owner=%d urgent=%v, attempt %d)",
		job.ID, job.Kind, job.SubjectID, job.TestNumber, job.OwnerUserID, job.Urgent, job.Attempts)

	// A 'running' job that stops being touched for stuckJobTimeout is
	// re-queued by the reaper. The heartbeat keeps updated_at fresh while the
	// (minutes-long) model call is in flight — otherwise the reaper could
	// re-queue a job that is actually alive and healthy.
	heartbeatDone := make(chan struct{})
	go g.jobHeartbeat(ctx, job.ID, heartbeatDone)
	defer close(heartbeatDone)

	testID, runErr := func() (testID int64, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("job %d panicked: %v", job.ID, r)
			}
		}()
		jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
		defer cancel()
		return g.runJob(jobCtx, job)
	}()
	if runErr != nil {
		log.Printf("generator: job %d failed: %v", job.ID, runErr)
		if ferr := g.gen.FailJob(ctx, job.ID, runErr, retryDelay, maxJobAttempts); ferr != nil {
			log.Printf("generator: fail job %d: %v", job.ID, ferr)
		}
		return
	}
	if err := g.gen.CompleteJob(ctx, job.ID, testID); err != nil {
		log.Printf("generator: complete job %d: %v", job.ID, err)
	}
	log.Printf("generator: job %d done -> test %d", job.ID, testID)
}

// claimDue reaps dead jobs and claims the next one. Errors are logged and
// swallowed (nil job): the worker ticks again in 20 seconds — a transient
// database hiccup must never stop queue processing.
func (g *GeneratorService) claimDue(ctx context.Context) (*models.GenerationJob, error) {
	g.reapStuckJobs(ctx)
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

	var prompt string
	var title string
	var testNumber int
	var ownerUserID int64
	var promptTopics []string // weak topics of a personal job (for validation)
	kind := job.Kind

	switch kind {
	case models.TestKindChain:
		testNumber = job.TestNumber
		// Never pay for a test that already exists. The enqueue path is
		// guarded, but a retried job may have actually succeeded on a
		// previous attempt (crash between the DB write and the job update).
		if existing, err := g.findChainTest(ctx, job.SubjectID, testNumber); err != nil {
			return 0, err
		} else if existing != nil {
			log.Printf("generator: chain test %d of subject %d already exists (id %d) — job %d resolved with no AI call", testNumber, job.SubjectID, existing.ID, job.ID)
			return existing.ID, nil
		}
		title = fmt.Sprintf("Тест %d", testNumber)
		var prev []models.Question
		var marks []int
		if testNumber > 1 {
			prevTest, err := g.findChainTest(ctx, job.SubjectID, testNumber-1)
			if err != nil {
				return 0, err
			}
			if prevTest != nil {
				prev, err = g.subjects.TestQuestions(ctx, prevTest.ID)
				if err != nil {
					return 0, err
				}
				if job.OwnerUserID > 0 {
					marks, err = g.questionMarks(ctx, job.OwnerUserID, prev)
					if err != nil {
						return 0, err
					}
				}
			}
		}
		prompt = chainGenPrompt(subject.Name, testNumber, prev, marks)

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
		topics, err := g.gen.WeakTopics(ctx, ownerUserID, job.SubjectID, weakTopicsCount)
		if err != nil {
			return 0, err
		}
		if len(topics) == 0 {
			return 0, fmt.Errorf("user %d has no weak topics in subject %d", ownerUserID, job.SubjectID)
		}
		// Before paying for a model call, check the fingerprint cache once
		// more — the same weak-topics set may have been generated for another
		// user (or by a concurrent job) while this job waited in the queue.
		if shared, err := g.gen.FindPersonalTestByFingerprint(ctx, job.SubjectID, topicsFingerprint(topics)); err != nil {
			return 0, err
		} else if shared != nil {
			qs, err := g.gen.PersonalTestQuestions(ctx, shared.ID)
			if err != nil {
				return 0, err
			}
			cloned, err := g.gen.ClonePersonalTest(ctx, shared, qs, ownerUserID)
			if err != nil {
				return 0, err
			}
			g.carryTranslations(ctx, shared.ID, cloned.ID)
			log.Printf("generator: personal job %d resolved by cloning test %d -> %d (no AI call)", job.ID, shared.ID, cloned.ID)
			return cloned.ID, nil
		}
		testNumber = 0 // assigned by the DB: next free personal number (9000+)
		title = "🎯 Слабые темы"
		promptTopics = topics
		prompt = personalGenPrompt(subject.Name, topics)

	default:
		return 0, fmt.Errorf("unknown job kind %q", kind)
	}

	// --- Provider route (cheapest first, every reply strictly validated):
	//   1. Groq GPT-OSS 120B — free tier, reasoning medium for shared chain
	//      tests, low for personal tests and retries;
	//   2. Groq GPT-OSS 120B at low effort (a medium pass that blew the
	//      token budget usually fits at low) — chain tests only;
	//   3. Groq Qwen 3.8 27B — its own independent free quota bucket;
	//   4. DeepSeek flash thinking — the paid last resort (old behaviour).
	// A Groq step whose free-tier quota is exhausted is skipped instantly.
	messages := []deepseek.Message{
		{Role: "system", Content: genSystemPrompt},
		{Role: "user", Content: prompt},
	}
	var final *generatedTest
	validate := func(raw string) error {
		gt, err := parseTestJSON(raw)
		if err != nil {
			return err
		}
		// Weak-topics tests carry a strict contract: only the requested
		// topics, all of them covered — a sloppy reply never reaches the DB.
		if kind == models.TestKindPersonal {
			if err := validatePersonalCoverage(gt, promptTopics); err != nil {
				return fmt.Errorf("personal test: %w", err)
			}
		}
		final = gt
		return nil
	}
	task := fmt.Sprintf("gen %s job %d", kind, job.ID)
	if _, _, err := runSteps(ctx, task, g.generationSteps(messages, kind, job.Attempts), validate); err != nil {
		return 0, fmt.Errorf("generate: %w", err)
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
		SubjectID:   job.SubjectID,
		TestNumber:  testNumber,
		Title:       title,
		Kind:        kind,
		Topics:      testTopics,
		OwnerUserID: ownerUserID,
	}
	// Personal tests are stamped with the weak-topics fingerprint so the next
	// user with the SAME weakness profile reuses this test (clone, no AI call).
	if kind == models.TestKindPersonal {
		test.TopicsFingerprint = topicsFingerprint(promptTopics)
	}
	stored, err := g.gen.CreateGeneratedTest(ctx, test, final.toSeed())
	if err != nil {
		return 0, err
	}
	return stored.ID, nil
}

// generationSteps returns the ordered provider route for one generation.
func (g *GeneratorService) generationSteps(messages []deepseek.Message, kind string, attempts int) []aiStep {
	retry := attempts > 1
	var steps []aiStep
	if g.gq != nil {
		gm := toGroqMessages(messages)
		base := groq.Request{
			Messages:   gm,
			MaxTokens:  genMaxTokens,
			MinTokens:  groqGenMinTokens,
			Schema:     testJSONSchema,
			SchemaName: "ent_test",
			MaxWait:    groqGenMaxWait,
		}
		oss := base
		oss.Model = groq.ModelGPTOSS120B
		if kind == models.TestKindChain && !retry {
			oss.Effort = groq.EffortMedium
			steps = append(steps, groqStep(g.gq, oss))
			oss.Effort = groq.EffortLow
			steps = append(steps, groqStep(g.gq, oss))
		} else {
			oss.Effort = groq.EffortLow
			steps = append(steps, groqStep(g.gq, oss))
		}
		qw := base
		qw.Model = groq.ModelQwen27B
		qw.Effort = groq.EffortNone // instruct mode: no reasoning tokens, whole budget for JSON
		qw.Temperature = 0.7
		qw.TopP = 0.8
		steps = append(steps, groqStep(g.gq, qw))
	}
	if g.ds != nil {
		effort := deepseek.ThinkingEffortHigh
		if kind == models.TestKindPersonal || retry {
			effort = deepseek.ThinkingEffortLow
		}
		ds := g.ds
		steps = append(steps, aiStep{
			name: "deepseek/" + ds.ReasonerModel() + "(" + effort + ")",
			run: func(ctx context.Context) (string, error) {
				return ds.GenerateJSON(ctx, messages, genMaxTokens, effort)
			},
		})
	}
	return steps
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

// questionMarks maps each question to the user's knowledge level
// (0=🔴 не знает, 1=🟡 в процессе, 2=🟢 закреплено).
func (g *GeneratorService) questionMarks(ctx context.Context, userID int64, questions []models.Question) ([]int, error) {
	marks := make([]int, len(questions))
	if len(questions) == 0 {
		return marks, nil
	}
	ids := make([]int64, len(questions))
	for i, q := range questions {
		ids[i] = q.ID
	}
	statuses, err := g.subjects.QuestionStatuses(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	for i, q := range questions {
		marks[i] = statuses[q.ID]
	}
	return marks, nil
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

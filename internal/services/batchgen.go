package services

// Batch generation (audit item 4): a test is assembled from small batches
// instead of one 20-question call.
//
//   - The test is planned as SLOTS — one per question, each with a target
//     difficulty (the chain mix of its position) and, for weak-topic slots,
//     a required topic. A batch call gets an exact per-question spec.
//   - Every returned question is validated ON ITS OWN (format, slot topic,
//     slot difficulty ±1, no repeat of the previous test or of the questions
//     already in this test, quality audit). Good questions are kept even if
//     their neighbours are bad; only the missing slots are asked again.
//   - A rejected batch reply is retried by the next provider step WITH the
//     rejection reason (feedback) — models ignore instructions most often
//     on a blind retry.
//   - Accepted questions are persisted per slot (gen_job_batches): a job
//     retried after a failure, timeout or deploy reuses them.
//   - Prompt caching: the long prefix (system prompt + subject + scale +
//     previous test + topic list) is byte-identical across the batches of a
//     job; only the short batch spec at the END differs, so Groq/DeepSeek
//     prefix caching serves the prefix from cache (cheaper, and on Groq
//     cached tokens do not count toward the rate limits).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/models"
)

const (
	// genBatchMaxTokens caps one batch call (5 questions ≈ 900–1100
	// visible tokens + the reasoning pass).
	genBatchMaxTokens = 3500
	// groqBatchMinTokens: a batch request whose output budget would shrink
	// below this is skipped without an HTTP call (next provider).
	groqBatchMinTokens = 2000
	// groqBatchStepTimeout bounds one Groq batch step (limiter wait + HTTP):
	// a slow/bad Groq reply is abandoned quickly in favour of the next step.
	groqBatchStepTimeout = 90 * time.Second
	// groqBatchMaxWait bounds the local rate-limiter wait of a batch call.
	groqBatchMaxWait = 45 * time.Second
	// dsBatchMaxTokens: max_tokens of one PAID DeepSeek batch call (thinking
	// + ~1000 visible tokens). The Groq batch budget stays genBatchMaxTokens;
	// DeepSeek is the last provider, so its reply must not be cut off by the
	// cap (prod: «truncated at max_tokens, 0 visible chars» — the thinking
	// ate the whole budget). Only produced tokens are billed. Was 3500.
	dsBatchMaxTokens = 8000
	// deepseekBatchCallTimeout bounds one paid batch call INCLUDING its
	// truncation retry and transient-error retries. It starts after the
	// paid-call semaphore slot is acquired (was 2 min including the queue).
	deepseekBatchCallTimeout = 5 * time.Minute
	// batchMaxRounds: how many times the still-missing slots are re-asked.
	batchMaxRounds = 4
	// batchRescueMaxMissing / batchRescueRounds / batchRescueSize: when
	// only a few slots are still empty after batchMaxRounds, they are asked
	// again in small batches with relaxed checks (rescue) instead of
	// failing the whole 20-question test.
	batchRescueMaxMissing = 6
	batchRescueRounds     = 2
	batchRescueSize       = 2
	// batchRescueDifficultySlack: the per-question difficulty slack of a
	// rescue round (the test-level difficulty contract still applies).
	batchRescueDifficultySlack = 2
	// maxHardFlaggedPerBatchReply: a batch reply with more giveaway
	// questions than this is sloppy as a whole (the rest are repaired).
	maxHardFlaggedPerBatchReply = 2
	// batchDifficultySlack: allowed |difficulty - slot difficulty|.
	batchDifficultySlack = 1
)

// genSlot is one planned question of a test.
type genSlot struct {
	Topic      string // required topic ("" = free choice from the catalog)
	Difficulty int    // target level 1..5
}

// genSpec is everything the batch generator needs about one test.
type genSpec struct {
	kind        string
	subjectName string
	testNumber  int
	basePrompt  string // stable prefix (prompt caching); no output instruction
	slots       []genSlot
	prevStems   []string
	weak        []string
	prevMean    float64
	catalog     *models.TopicCatalog
	// personalTopics: the requested weak topics of a personal test.
	personalTopics []string

	mu        sync.Mutex
	newTopics map[string]bool // chain topics outside the catalog accepted so far
}

func (g *GeneratorService) batchSize() int {
	if g.cfg != nil && g.cfg.GenBatchSize >= 2 {
		return g.cfg.GenBatchSize
	}
	return config.DefaultGenBatchSize
}

func (g *GeneratorService) batchParallel() int {
	if g.cfg != nil && g.cfg.GenBatchParallel >= 1 {
		return g.cfg.GenBatchParallel
	}
	return config.DefaultGenBatchParallel
}

// difficultyLevels expands a mix into an ascending list of levels.
func difficultyLevels(mix [5]int) []int {
	var out []int
	for lvl, n := range mix {
		for i := 0; i < n; i++ {
			out = append(out, lvl+1)
		}
	}
	return out
}

// interleave reorders items so that consecutive chunks of size `chunk`
// each get a representative spread (item i goes to chunk i % nChunks).
func interleave[T any](items []T, chunk int) []T {
	if chunk <= 0 || len(items) <= chunk {
		return items
	}
	nChunks := (len(items) + chunk - 1) / chunk
	out := make([]T, 0, len(items))
	for c := 0; c < nChunks; c++ {
		for i := c; i < len(items); i += nChunks {
			out = append(out, items[i])
		}
	}
	return out
}

// planChainSlots plans the 20 questions of chain test n: the difficulty mix
// of its position and up to 40% weak-topic slots (each weak topic about
// twice), spread over the difficulty levels; batches of batchSize then get
// a representative mix of levels each.
func planChainSlots(testNumber int, weak []string, total, batchSize int) []genSlot {
	levels := difficultyLevels(difficultyMix(chainDifficultyTarget(testNumber)))
	for len(levels) < total { // defensive: total != GeneratedQuestionsPerTest
		levels = append(levels, int(math.Round(chainDifficultyTarget(testNumber))))
	}
	levels = levels[:total]
	slots := make([]genSlot, total)
	for i := range slots {
		slots[i].Difficulty = levels[i]
	}
	if len(weak) > 0 {
		nWeak := min(len(weak)*2, total*2/5)
		for j := 0; j < nWeak; j++ {
			slots[j*total/nWeak].Topic = weak[j%len(weak)]
		}
	}
	return interleave(slots, batchSize)
}

// planPersonalSlots plans a weak-topics test: the topics split as evenly as
// possible (the weakest topics get the remainder), levels 2→4 per topic
// ("close the gap, not overwhelm").
func planPersonalSlots(topics []string, total, batchSize int) []genSlot {
	if len(topics) == 0 {
		return nil
	}
	per, rest := total/len(topics), total%len(topics)
	cycle := []int{2, 3, 3, 4, 2, 3}
	var slots []genSlot
	for i, t := range topics {
		n := per
		if i < rest {
			n++
		}
		for k := 0; k < n; k++ {
			slots = append(slots, genSlot{Topic: t, Difficulty: cycle[k%len(cycle)]})
		}
	}
	return interleave(slots, batchSize)
}

// batchPrompt is the user prompt of one batch call: the stable prefix +
// the per-question spec of this batch + what is already in the test.
func batchPrompt(spec *genSpec, group []int, avoid []string, feedback string) string {
	var b strings.Builder
	b.WriteString(spec.basePrompt)
	fmt.Fprintf(&b, "\n\nТЕСТ СОБИРАЕТСЯ ЧАСТЯМИ. В ЭТОМ ВЫЗОВЕ напиши ровно %d вопрос(ов) — по одному на каждую строку, в этом порядке:\n", len(group))
	for k, si := range group {
		s := spec.slots[si]
		if s.Topic != "" {
			fmt.Fprintf(&b, "%d) тема: «%s» (поле topic — дословно эта строка), difficulty %d\n", k+1, s.Topic, s.Difficulty)
		} else {
			fmt.Fprintf(&b, "%d) тема: любая из справочника/программы (другая, чем у уже написанных вопросов), difficulty %d\n", k+1, s.Difficulty)
		}
	}
	if len(avoid) > 0 {
		b.WriteString("Уже вошли в этот тест — НЕ повторяй и не перефразируй их:\n")
		for _, a := range avoid {
			if r := []rune(a); len(r) > 70 {
				a = string(r[:70]) + "…"
			}
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	if feedback != "" {
		fmt.Fprintf(&b, "ВНИМАНИЕ: предыдущий ответ ОТКЛОНЁН проверкой: %s. Исправь именно это; difficulty каждого вопроса — строго как в спецификации (±1).\n", feedback)
	}
	fmt.Fprintf(&b, "Выдай строго JSON {\"questions\":[...]} ровно с %d вопросами.", len(group))
	return b.String()
}

// parseLooseJSON extracts the questions of a model reply without the count
// check (batch replies are validated question by question).
func parseLooseJSON(raw string) (*generatedTest, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, rejectf(rejectFormat, "no JSON object found in model reply")
	}
	var gt generatedTest
	if err := json.Unmarshal([]byte(raw[start:end+1]), &gt); err != nil {
		return nil, rejectf(rejectFormat, "decode model JSON: %v", err)
	}
	return &gt, nil
}

// cleanQuestion applies the per-question part of the JSON contract.
// ok=false: the question is unusable.
func cleanQuestion(q *generatedQuestion) bool {
	q.Text = strings.TrimSpace(q.Text)
	q.Topic = strings.TrimSpace(q.Topic)
	if len(q.Text) < 8 || len(q.Options) != 4 || q.Correct < 0 || q.Correct > 3 {
		return false
	}
	for j := range q.Options {
		q.Options[j] = strings.TrimSpace(q.Options[j])
		if q.Options[j] == "" {
			return false
		}
	}
	normalizeOptionFormat(q.Text, q.Options)
	return q.Difficulty >= 1 && q.Difficulty <= 5
}

// noteQuestionDrop counts one question dropped from a batch reply.
func noteQuestionDrop(ctx context.Context, class string) {
	r := genRunFrom(ctx)
	strategy, kind := "", ""
	if r != nil {
		strategy, kind = r.strategy, r.kind
		switch class {
		case rejectDifficulty:
			r.diffViolation.Add(1)
		case rejectRepeat:
			r.repeats.Add(1)
		}
	}
	switch class {
	case rejectDifficulty:
		metrics.Inc(metrics.DifficultyViolations, "step", "batch-question", "strategy", strategy, "kind", kind)
	case rejectRepeat:
		metrics.Inc(metrics.RepeatRejects, "strategy", strategy, "kind", kind)
	}
}

// acceptTopic checks/maps the topic of a question for a slot. For a free
// chain slot the catalog title is used; a topic outside the catalog is
// accepted only while the test's new-topic budget lasts.
func (spec *genSpec) acceptTopic(q *generatedQuestion, slot genSlot) bool {
	if slot.Topic != "" {
		if !topicMatches(q.Topic, slot.Topic, spec.catalog) {
			return false
		}
		q.Topic = slot.Topic
		return true
	}
	if q.Topic == "" {
		return false
	}
	if spec.catalog == nil || len(spec.catalog.Aliases) == 0 {
		return true
	}
	if k, ok := spec.catalog.Resolve(q.Topic); ok {
		for _, t := range spec.catalog.Titles {
			if models.NormalizeTopic(t) == k {
				q.Topic = t
				break
			}
		}
		return true
	}
	spec.mu.Lock()
	defer spec.mu.Unlock()
	n := models.NormalizeTopic(q.Topic)
	if spec.newTopics == nil {
		spec.newTopics = map[string]bool{}
	}
	if spec.newTopics[n] {
		return true
	}
	if len(spec.newTopics) >= maxNewChainTopics {
		return false
	}
	spec.newTopics[n] = true
	return true
}

// acceptTopicRelaxed is acceptTopic of a RESCUE round (only a few slots
// left): a required-topic slot of a personal test also takes a question on
// ANY of the requested weak topics, and a weak-topic slot of a chain test
// also takes a question on any acceptable free topic. The test-level
// contract (all personal topics covered, weak coverage of the chain) is
// still checked on the assembled test.
func (spec *genSpec) acceptTopicRelaxed(q *generatedQuestion, slot genSlot) bool {
	if spec.acceptTopic(q, slot) {
		return true
	}
	if slot.Topic == "" {
		return false
	}
	if len(spec.personalTopics) > 0 {
		for _, t := range spec.personalTopics {
			if topicMatches(q.Topic, t, spec.catalog) {
				q.Topic = t
				return true
			}
		}
		return false
	}
	return spec.acceptTopic(q, genSlot{Difficulty: slot.Difficulty})
}

// matchBatch validates a batch reply question by question and assigns the
// usable questions to the slots of the group. The reply as a whole is
// rejected when fewer than half of the slots got a usable question (or it
// is sloppy: too many giveaway questions) — the reason goes back to the
// model as feedback.
//
// relaxed (rescue round): difficulty slack batchRescueDifficultySlack and
// acceptTopicRelaxed.
func (g *GeneratorService) matchBatch(ctx context.Context, spec *genSpec, group []int, raw string, avoid []string, relaxed bool) (map[int]*generatedQuestion, error) {
	slack := batchDifficultySlack
	accept := spec.acceptTopic
	if relaxed {
		slack = batchRescueDifficultySlack
		accept = spec.acceptTopicRelaxed
	}
	gt, err := parseLooseJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(gt.Questions) == 0 {
		return nil, rejectf(rejectFormat, "reply has no questions")
	}
	out := make(map[int]*generatedQuestion, len(group))
	reasons := map[string]int{}
	var accepted []string
	hard := 0
	for i := range gt.Questions {
		q := gt.Questions[i]
		if !cleanQuestion(&q) {
			reasons[rejectFormat]++
			continue
		}
		if repeatsAny(q.Text, spec.prevStems) >= 0 {
			reasons[rejectRepeat]++
			noteQuestionDrop(ctx, rejectRepeat)
			continue
		}
		if repeatsAny(q.Text, avoid) >= 0 || repeatsAny(q.Text, accepted) >= 0 {
			reasons[rejectRepeat]++
			noteQuestionDrop(ctx, rejectRepeat)
			continue
		}
		placed := false
		diffMiss, topicMiss := false, false
		// Two passes: an exact-difficulty slot first, then one within the
		// slack — a greedy first fit wasted exact slots on near matches.
		for pass := 0; pass <= slack && !placed; pass++ {
			for _, si := range group {
				if out[si] != nil {
					continue
				}
				slot := spec.slots[si]
				cand := q
				if d := abs(cand.Difficulty - slot.Difficulty); d != pass {
					if d > slack {
						diffMiss = true
					}
					continue
				}
				// Required-topic slots only take their topic; a question on a
				// required topic may still fill a free slot.
				if !accept(&cand, slot) {
					topicMiss = true
					continue
				}
				out[si] = &cand
				accepted = append(accepted, cand.Text)
				if auditQuestion(cand.Text, cand.Options, cand.Correct).HasHard() {
					hard++
				}
				placed = true
				break
			}
		}
		if !placed {
			switch {
			case diffMiss:
				reasons[rejectDifficulty]++
				noteQuestionDrop(ctx, rejectDifficulty)
			case topicMiss:
				reasons[rejectTopics]++
			default:
				reasons["extra"]++
			}
		}
	}
	if hard > maxHardFlaggedPerBatchReply {
		return nil, rejectf(rejectQuality, "quality audit: %d questions reveal the answer or have broken options", hard)
	}
	need := (len(group) + 1) / 2
	if len(out) < need {
		return nil, rejectf(dominantReason(reasons), "only %d of %d questions usable (%s)", len(out), len(group), reasonString(reasons))
	}
	return out, nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func dominantReason(r map[string]int) string {
	best, n := rejectFormat, 0
	for k, v := range r {
		if k != "extra" && v > n {
			best, n = k, v
		}
	}
	return best
}

func reasonString(r map[string]int) string {
	names := map[string]string{
		rejectFormat:     "сломан формат",
		rejectRepeat:     "повтор вопроса",
		rejectDifficulty: "сложность не по спецификации",
		rejectTopics:     "тема не по спецификации",
		"extra":          "лишние вопросы",
	}
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", names[k], r[k]))
	}
	return strings.Join(parts, ", ")
}

// batchSteps is the provider route of one batch call: at most TWO Groq
// attempts (then the paid DeepSeek fallback). Chain batches always start on
// GPT-OSS 120B (quality of the shared chain); other batches alternate the
// first model by `alt`, so parallel batches use the two independent free
// quotas at once. messages is evaluated at step run time (feedback).
func (g *GeneratorService) batchSteps(messages func() []deepseek.Message, kind string, attempts, alt int) []aiStep {
	retry := attempts > 1
	var steps []aiStep
	if g.gq != nil {
		ossEffort := groq.EffortLow
		if kind == models.TestKindChain && !retry {
			ossEffort = groq.EffortMedium
		}
		oss := groqDynStep(g.gq, groq.ModelGPTOSS120B, ossEffort, 0, 0, messages)
		qw := groqDynStep(g.gq, groq.ModelQwen27B, groq.EffortNone, 0.7, 0.8, messages)
		if kind != models.TestKindChain && alt%2 == 1 {
			steps = append(steps, qw, oss)
		} else {
			steps = append(steps, oss, qw)
		}
	}
	if g.ds != nil {
		effort := deepseek.ThinkingEffortLow
		if kind == models.TestKindChain && !retry {
			effort = deepseek.ThinkingEffortHigh
		}
		ds := g.ds
		steps = append(steps, aiStep{
			name: "deepseek/" + ds.ReasonerModel() + "(" + effort + ")",
			// No step timeout: waiting for a semaphore slot must not eat
			// the call's time — the timeout starts after the slot.
			run: func(ctx context.Context) (string, error) {
				release, err := g.acquireDeepSeek(ctx)
				if err != nil {
					return "", err
				}
				defer release()
				cctx, cancel := context.WithTimeout(ctx, deepseekBatchCallTimeout)
				defer cancel()
				return ds.GenerateJSON(cctx, messages(), dsBatchMaxTokens, effort)
			},
		})
	}
	return steps
}

// groqDynStep is a Groq batch step whose messages are built at run time.
func groqDynStep(gc *groq.Client, model, effort string, temp, topP float64, messages func() []deepseek.Message) aiStep {
	name := "groq/" + model
	if effort != "" {
		name += "(" + effort + ")"
	}
	return aiStep{name: name, timeout: groqBatchStepTimeout, run: func(ctx context.Context) (string, error) {
		res, err := gc.ChatJSON(ctx, groq.Request{
			Model:       model,
			Messages:    toGroqMessages(messages()),
			MaxTokens:   genBatchMaxTokens,
			MinTokens:   groqBatchMinTokens,
			Effort:      effort,
			Temperature: temp,
			TopP:        topP,
			Schema:      testJSONSchema,
			SchemaName:  "ent_test_batch",
			MaxWait:     groqBatchMaxWait,
		})
		if err != nil {
			return "", err
		}
		noteTokens(ctx, res.PromptTokens, res.CompletionTokens)
		return res.Content, nil
	}}
}

// runBatch asks the providers for the questions of one group of slots.
func (g *GeneratorService) runBatch(ctx context.Context, job *models.GenerationJob, spec *genSpec, group []int, avoid []string, alt int, relaxed bool) (map[int]*generatedQuestion, string, error) {
	var feedback string
	var result map[int]*generatedQuestion
	messages := func() []deepseek.Message {
		return []deepseek.Message{
			{Role: "system", Content: genSystemPrompt},
			{Role: "user", Content: batchPrompt(spec, group, avoid, feedback)},
		}
	}
	validate := func(raw string) error {
		got, err := g.matchBatch(ctx, spec, group, raw, avoid, relaxed)
		if err != nil {
			return err
		}
		result = got
		return nil
	}
	task := fmt.Sprintf("gen %s job %d batch %v", job.Kind, job.ID, group)
	if relaxed {
		task += " (rescue)"
	}
	_, provider, err := runStepsFeedback(ctx, task, g.batchSteps(messages, spec.kind, job.Attempts, alt), validate,
		func(e error) { feedback = e.Error() })
	return result, provider, err
}

func seedToGenerated(sq models.SeedQuestion) generatedQuestion {
	return generatedQuestion{Text: sq.Text, Options: sq.Options[:], Correct: sq.Correct, Topic: sq.Topic, Difficulty: sq.Difficulty}
}

func generatedToSeed(q *generatedQuestion) models.SeedQuestion {
	var o [4]string
	copy(o[:], q.Options)
	return models.SeedQuestion{Text: q.Text, Options: o, Correct: q.Correct, Topic: q.Topic, Difficulty: q.Difficulty}
}

// generateBatched assembles the test of spec from batches (see the file
// comment). Returns the questions in slot order.
func (g *GeneratorService) generateBatched(ctx context.Context, job *models.GenerationJob, spec *genSpec) (*generatedTest, error) {
	n := len(spec.slots)
	if n == 0 {
		return nil, fmt.Errorf("batch: empty plan")
	}
	filled := make([]*generatedQuestion, n)
	stems := func() []string {
		var out []string
		for _, q := range filled {
			if q != nil {
				out = append(out, q.Text)
			}
		}
		return out
	}

	// Reuse the batches a previous attempt of this job already paid for.
	if job.ID > 0 && g.gen != nil {
		stored, err := g.gen.JobBatches(ctx, job.ID)
		if err != nil {
			log.Printf("generator: job %d: load stored batches: %v", job.ID, err)
		}
		reused := 0
		for si, qs := range stored {
			if si < 0 || si >= n || len(qs) != 1 {
				continue
			}
			q := seedToGenerated(qs[0])
			slot := spec.slots[si]
			if !cleanQuestion(&q) || abs(q.Difficulty-slot.Difficulty) > batchDifficultySlack || !spec.acceptTopic(&q, slot) ||
				repeatsAny(q.Text, spec.prevStems) >= 0 || repeatsAny(q.Text, stems()) >= 0 {
				continue // the plan changed (e.g. new weak topics) — regenerate
			}
			filled[si] = &q
			reused++
		}
		if reused > 0 {
			metrics.Add(metrics.BatchReuse, float64(reused), "kind", job.Kind)
			log.Printf("generator: job %d: reusing %d question(s) from a previous attempt", job.ID, reused)
		}
	}

	bs, par := g.batchSize(), g.batchParallel()
	alt := 0
	// budgetHit: a batch failed because the paid fallback hit the daily
	// DeepSeek cap. The error is surfaced (wrapped) when the test can not be
	// completed, so executeJob DEFERS the job instead of failing it — the
	// same R-9 behaviour as the full strategy (whose runSteps error already
	// wraps ErrBudgetExceeded).
	var budgetHit atomic.Bool
	missingSlots := func() []int {
		var missing []int
		for i, q := range filled {
			if q == nil {
				missing = append(missing, i)
			}
		}
		return missing
	}
	// runRound asks the providers for the missing slots in groups of size
	// (up to par groups in parallel).
	runRound := func(missing []int, size int, relaxed bool) {
		var groups [][]int
		for i := 0; i < len(missing); i += size {
			groups = append(groups, missing[i:min(i+size, len(missing))])
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, par)
		for _, grp := range groups {
			grp := grp
			myAlt := alt
			alt++
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				mu.Lock()
				avoid := stems()
				mu.Unlock()
				got, provider, err := g.runBatch(ctx, job, spec, grp, avoid, myAlt, relaxed)
				if err != nil {
					if errors.Is(err, deepseek.ErrBudgetExceeded) {
						budgetHit.Store(true)
					}
					log.Printf("generator: job %d: batch %v failed: %v", job.ID, grp, err)
					return
				}
				mu.Lock()
				fresh := map[int]models.SeedQuestion{}
				for si, q := range got {
					// Parallel batches can not see each other: re-check
					// duplicates against everything placed meanwhile.
					if filled[si] != nil || repeatsAny(q.Text, stems()) >= 0 {
						continue
					}
					filled[si] = q
					fresh[si] = generatedToSeed(q)
				}
				mu.Unlock()
				if job.ID > 0 && g.gen != nil && len(fresh) > 0 {
					if err := g.gen.SaveJobSlots(ctx, job.ID, fresh, provider); err != nil {
						log.Printf("generator: job %d: persist batch: %v", job.ID, err)
					}
				}
			}()
		}
		wg.Wait()
	}
	for round := 0; round < batchMaxRounds && ctx.Err() == nil; round++ {
		missing := missingSlots()
		if len(missing) == 0 {
			break
		}
		runRound(missing, bs, false)
	}
	// Rescue: only a few of the questions are still missing — the test must
	// not fail as a whole because of them. Ask again in small batches (a
	// small reply is never truncated and needs only one usable question)
	// with relaxed per-question checks.
	for round := 0; round < batchRescueRounds && ctx.Err() == nil; round++ {
		missing := missingSlots()
		if len(missing) == 0 || len(missing) > batchRescueMaxMissing {
			break
		}
		log.Printf("generator: job %d: rescue round %d for %d missing question(s)", job.ID, round+1, len(missing))
		metrics.Add(metrics.BatchRescue, float64(len(missing)), "kind", job.Kind)
		runRound(missing, batchRescueSize, true)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gt := &generatedTest{}
	missing := 0
	for _, q := range filled {
		if q == nil {
			missing++
			continue
		}
		gt.Questions = append(gt.Questions, *q)
	}
	if missing > 0 {
		if budgetHit.Load() {
			return nil, fmt.Errorf("batch: %d of %d questions could not be generated: %w", missing, n, deepseek.ErrBudgetExceeded)
		}
		return nil, fmt.Errorf("batch: %d of %d questions could not be generated", missing, n)
	}
	rebalanceAnswerKeys(gt)
	return gt, nil
}

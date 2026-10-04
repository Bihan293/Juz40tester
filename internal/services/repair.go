// Package services — targeted repair of flagged questions.
//
// When auditQuestion flags a question, we do NOT throw away the whole
// 20-question test (that would multiply the AI spend). Instead only the bad
// questions are sent back to the model with the concrete reasons, and the
// model rewrites them (same topic, same difficulty). Every rewritten
// question must pass both the structural checks and the quality audit
// again; otherwise another round is tried. The same routine fixes legacy
// questions already stored in the database (RunQualitySweep).
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

const (
	// maxHardFlaggedPerReply: a reply with more giveaway questions than this
	// is considered sloppy as a whole — the next provider writes the test
	// from scratch instead of patching half of it.
	maxHardFlaggedPerReply = 6
	// repairRounds bounds the targeted rewrite loop.
	repairRounds = 2
	// repairMaxTokens: up to ~6 questions per call — far below a full test.
	repairMaxTokens = 4000
	// repairBatch caps the number of questions in one repair call.
	repairBatch = 6
)

const repairSystemPrompt = `Ты — редактор тестов ЕНТ/УБТ. Тебе дают вопросы, забракованные проверкой качества, и причины брака. Перепиши КАЖДЫЙ вопрос так, чтобы правильный ответ можно было выбрать ТОЛЬКО зная правило/факт, а не по внешнему виду вариантов.

Обязательно:
1. Тема (topic) и уровень (difficulty) — те же, что у исходного вопроса; ровно 4 варианта, ровно один правильный.
2. Все 4 варианта однородны: одинаковая структура, близкая длина, один и тот же формат записи.
3. Вопросы на пунктуацию/орфографию: если спрашивается, ГДЕ нужен знак (тире, запятая, двоеточие, дефис и т.п.) или буква — во ВСЕХ вариантах знак/буква НЕ проставлены (место дано одинаково, например пропуском «_» в каждом варианте). Если спрашивается, где знак поставлен ВЕРНО — знак есть во ВСЕХ вариантах, но в разных местах/ситуациях. Никогда не бывает так, что знак есть только в одном варианте.
4. Никаких подсказок: без пометок «(верно)», без вариантов «все ответы верны»/«нет правильного ответа», без дословного повтора ответа из текста вопроса, без совпадающих вариантов.
5. Дистракторы — правдоподобные типичные ошибки.

Ответ — строго JSON в том же порядке, что вопросы на входе:
{"questions":[{"question":"...","options":["...","...","...","..."],"correct_index":0,"topic":"...","difficulty":2}]}`

// repairItem is one question sent to the model for a rewrite.
type repairItem struct {
	Q       generatedQuestion
	Reasons string
}

func repairPrompt(subjectName string, items []repairItem) string {
	var b strings.Builder
	b.WriteString(languageSubjectRule(subjectName))
	fmt.Fprintf(&b, "Предмет: «%s». Перепиши %d вопрос(ов):\n\n", subjectName, len(items))
	for i, it := range items {
		raw, _ := json.Marshal(it.Q)
		fmt.Fprintf(&b, "%d. %s\nПричины брака: %s\n\n", i+1, raw, it.Reasons)
	}
	fmt.Fprintf(&b, "Выдай строго JSON: ровно %d вопрос(ов), в том же порядке.", len(items))
	return b.String()
}

// repairSteps is the provider route of a repair call (free Groq first).
func (g *GeneratorService) repairSteps(messages []deepseek.Message) []aiStep {
	var steps []aiStep
	if g.gq != nil {
		base := groq.Request{
			Messages:   toGroqMessages(messages),
			MaxTokens:  repairMaxTokens,
			MinTokens:  1500,
			Schema:     testJSONSchema,
			SchemaName: "ent_repair",
			MaxWait:    groqGenMaxWait,
		}
		oss := base
		oss.Model = groq.ModelGPTOSS120B
		oss.Effort = groq.EffortLow
		steps = append(steps, groqStep(g.gq, oss))
		qw := base
		qw.Model = groq.ModelQwen27B
		qw.Effort = groq.EffortNone
		qw.Temperature = 0.7
		qw.TopP = 0.8
		steps = append(steps, groqStep(g.gq, qw))
	}
	if g.ds != nil {
		ds := g.ds
		steps = append(steps, aiStep{
			name:    "deepseek/" + ds.ReasonerModel() + "(low)",
			reserve: deepseekRepairReserve,
			run: func(ctx context.Context) (string, error) {
				return ds.GenerateJSON(ctx, messages, repairMaxTokens, deepseek.ThinkingEffortLow)
			},
		})
	}
	return steps
}

// checkRewrite validates one rewritten question against its original:
// structure, same topic (forced), and a clean quality audit.
func checkRewrite(orig generatedQuestion, q *generatedQuestion) error {
	q.Text = strings.TrimSpace(q.Text)
	if len(q.Text) < 8 {
		return fmt.Errorf("text too short")
	}
	if len(q.Options) != 4 {
		return fmt.Errorf("need 4 options, got %d", len(q.Options))
	}
	for j := range q.Options {
		q.Options[j] = strings.TrimSpace(q.Options[j])
		if q.Options[j] == "" {
			return fmt.Errorf("empty option %d", j+1)
		}
	}
	if q.Correct < 0 || q.Correct > 3 {
		return fmt.Errorf("correct_index %d out of range", q.Correct)
	}
	normalizeOptionFormat(q.Text, q.Options)
	// The topic is part of the weak-topics contract and of the progress
	// analytics — it is never allowed to drift during a repair.
	q.Topic = orig.Topic
	if q.Difficulty < 1 || q.Difficulty > 5 {
		q.Difficulty = orig.Difficulty
	}
	if rep := auditQuestion(q.Text, q.Options, q.Correct); rep.HasHard() {
		return fmt.Errorf("still flagged: %s", rep.Reasons())
	}
	return nil
}

// rewriteQuestions asks the model to rewrite the given questions. It returns
// the accepted rewrites keyed by the item index; items whose rewrite is
// still bad are simply missing from the map.
func (g *GeneratorService) rewriteQuestions(ctx context.Context, task, subjectName string, items []repairItem) (map[int]generatedQuestion, error) {
	if len(items) == 0 {
		return nil, nil
	}
	messages := []deepseek.Message{
		{Role: "system", Content: repairSystemPrompt},
		{Role: "user", Content: repairPrompt(subjectName, items)},
	}
	accepted := map[int]generatedQuestion{}
	validate := func(raw string) error {
		var gt generatedTest
		start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
		if start < 0 || end <= start {
			return fmt.Errorf("no JSON object in repair reply")
		}
		if err := json.Unmarshal([]byte(raw[start:end+1]), &gt); err != nil {
			return fmt.Errorf("decode repair JSON: %w", err)
		}
		if len(gt.Questions) != len(items) {
			return fmt.Errorf("repair: need %d questions, got %d", len(items), len(gt.Questions))
		}
		ok := map[int]generatedQuestion{}
		for i := range gt.Questions {
			q := gt.Questions[i]
			if err := checkRewrite(items[i].Q, &q); err != nil {
				log.Printf("ai[%s]: rewrite %d rejected: %v", task, i+1, err)
				continue
			}
			ok[i] = q
		}
		if len(ok) == 0 {
			return fmt.Errorf("repair: no rewrite passed the quality audit")
		}
		accepted = ok
		return nil
	}
	if _, _, err := runSteps(ctx, task, g.repairSteps(messages), validate); err != nil {
		return nil, err
	}
	return accepted, nil
}

// repairFlagged rewrites the flagged questions of a freshly generated test
// in place. Hard issues MUST be fixed (an error is returned otherwise — the
// job is retried and the test never reaches a student); soft issues are
// fixed when the model manages to, and tolerated otherwise.
func (g *GeneratorService) repairFlagged(ctx context.Context, task, subjectName string, gt *generatedTest) error {
	for round := 1; round <= repairRounds; round++ {
		reports := auditGenerated(gt)
		if len(reports) == 0 {
			return nil
		}
		idx := make([]int, 0, len(reports))
		for i := range reports {
			idx = append(idx, i)
		}
		// Hard issues first, then soft — the batch cap keeps calls small.
		sort.Slice(idx, func(a, b int) bool {
			ha, hb := reports[idx[a]].HasHard(), reports[idx[b]].HasHard()
			if ha != hb {
				return ha
			}
			return idx[a] < idx[b]
		})
		for start := 0; start < len(idx); start += repairBatch {
			end := start + repairBatch
			if end > len(idx) {
				end = len(idx)
			}
			batch := idx[start:end]
			items := make([]repairItem, len(batch))
			for k, qi := range batch {
				items[k] = repairItem{Q: gt.Questions[qi], Reasons: reports[qi].Reasons()}
				log.Printf("quality[%s]: q%d flagged (round %d): %s", task, qi+1, round, reports[qi].Reasons())
			}
			fixed, err := g.rewriteQuestions(ctx, fmt.Sprintf("%s repair r%d", task, round), subjectName, items)
			if err != nil {
				log.Printf("quality[%s]: repair round %d failed: %v", task, round, err)
				continue
			}
			for k, q := range fixed {
				gt.Questions[batch[k]] = q
			}
		}
	}
	reports := auditGenerated(gt)
	if n := countHard(reports); n > 0 {
		var parts []string
		for i, r := range reports {
			if r.HasHard() {
				parts = append(parts, fmt.Sprintf("q%d: %s", i+1, r.Reasons()))
			}
		}
		sort.Strings(parts)
		return fmt.Errorf("%d question(s) still fail the quality audit after repair: %s", n, strings.Join(parts, " | "))
	}
	if len(reports) > 0 {
		log.Printf("quality[%s]: %d question(s) keep soft warnings (accepted)", task, len(reports))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Background sweep over questions already stored in the database
// ---------------------------------------------------------------------------

const (
	// sweepScanLimit: questions audited locally per sweep tick (free, no AI).
	sweepScanLimit = 300
	// sweepMaxQuestionAttempts: after this many failed repairs a question is
	// left as is (logged) — the sweep must not spend money on it forever.
	sweepMaxQuestionAttempts = 3
	// sweepBusyPostpone: a flagged question that is on screen in an
	// unfinished attempt is skipped by the sweep for this long (no AI call).
	sweepBusyPostpone = 6 * time.Hour
	// deepseekRepairReserve: time guaranteed to the paid DeepSeek repair
	// step — the free Groq repair steps are cut to leave it.
	deepseekRepairReserve = 3 * time.Minute
)

// RunQualitySweep audits stored questions that have not been checked yet.
// Clean questions are just marked as checked (pure local work). Questions
// with HARD issues (the answer is visible from the options' format, duplicate
// options, catch-all options…) are rewritten by the model and updated in
// place, so legacy tests generated before the audit existed are fixed too.
// Returns how many questions were repaired.
func (g *GeneratorService) RunQualitySweep(ctx context.Context) (int, error) {
	if g == nil || g.gen == nil {
		return 0, nil
	}
	qs, err := g.gen.UncheckedQuestions(ctx, sweepScanLimit)
	if err != nil {
		return 0, err
	}
	if len(qs) == 0 {
		return 0, nil
	}
	var clean []int64
	bySubject := map[int64][]models.Question{}
	reasons := map[int64]string{}
	for _, q := range qs {
		rep := auditQuestion(q.Text, q.Options(), letterIndex(q.CorrectAnswer))
		if !rep.HasHard() {
			clean = append(clean, q.ID)
			continue
		}
		reasons[q.ID] = rep.Reasons()
		bySubject[q.SubjectID] = append(bySubject[q.SubjectID], q)
	}
	if err := g.gen.MarkQuestionsChecked(ctx, clean); err != nil {
		return 0, err
	}
	if len(bySubject) == 0 || !g.Enabled() {
		if len(bySubject) > 0 {
			log.Printf("quality sweep: %d flagged question(s) wait for an AI provider", len(reasons))
		}
		return 0, nil
	}

	repaired := 0
	subjectIDs := make([]int64, 0, len(bySubject))
	for id := range bySubject {
		subjectIDs = append(subjectIDs, id)
	}
	sort.Slice(subjectIDs, func(a, b int) bool { return subjectIDs[a] < subjectIDs[b] })
	for _, sid := range subjectIDs {
		subject, err := g.subjects.GetByID(ctx, sid)
		if err != nil {
			return repaired, err
		}
		// Questions on screen in an unfinished attempt are filtered out
		// BEFORE the paid rewrite (ReplaceQuestionContent would refuse them
		// and the model output would be thrown away) and postponed, so the
		// sweep does not pay for them again on every tick.
		list := make([]models.Question, 0, len(bySubject[sid]))
		for _, q := range bySubject[sid] {
			busy, err := g.gen.IsQuestionBusy(ctx, q.ID)
			if err != nil {
				return repaired, err
			}
			if busy {
				if err := g.gen.PostponeQualityCheck(ctx, q.ID, sweepBusyPostpone); err != nil {
					return repaired, err
				}
				log.Printf("quality sweep: question %d is in an active attempt — postponed for %s", q.ID, sweepBusyPostpone)
				continue
			}
			list = append(list, q)
		}
		for start := 0; start < len(list); start += repairBatch {
			if ctx.Err() != nil {
				return repaired, ctx.Err()
			}
			// Low priority: stop paying for repairs (and eating the Groq
			// quota) as soon as a user is waiting for a generation. The
			// remaining questions stay unchecked for the next sweep.
			if g.urgentWorkPending(ctx) {
				log.Printf("quality sweep: yielding to urgent user generations — %d repair(s) left for later", len(list)-start)
				return repaired, nil
			}
			end := start + repairBatch
			if end > len(list) {
				end = len(list)
			}
			batch := list[start:end]
			items := make([]repairItem, len(batch))
			for k, q := range batch {
				items[k] = repairItem{Q: generatedQuestion{
					Text: q.Text, Options: q.Options(), Correct: letterIndex(q.CorrectAnswer),
					Topic: q.Topic, Difficulty: q.Difficulty,
				}, Reasons: reasons[q.ID]}
				log.Printf("quality sweep: question %d flagged: %s", q.ID, reasons[q.ID])
			}
			fixed, ferr := g.rewriteQuestions(ctx, fmt.Sprintf("sweep subject %d", sid), subject.Name, items)
			if ferr != nil {
				log.Printf("quality sweep: repair failed: %v", ferr)
			}
			for k, q := range batch {
				nq, ok := fixed[k]
				if !ok {
					if err := g.gen.NoteQualityAttempt(ctx, q.ID, sweepMaxQuestionAttempts); err != nil {
						return repaired, err
					}
					continue
				}
				var opts [4]string
				copy(opts[:], nq.Options)
				err := g.gen.ReplaceQuestionContent(ctx, q.ID, models.SeedQuestion{
					Text: nq.Text, Options: opts, Correct: nq.Correct, Topic: q.Topic, Difficulty: nq.Difficulty,
				})
				if errors.Is(err, repositories.ErrQuestionBusy) {
					// Became busy during the rewrite (rare race) — the paid
					// attempt counts, and the question is postponed.
					if err := g.gen.NoteQualityAttempt(ctx, q.ID, sweepMaxQuestionAttempts); err != nil {
						return repaired, err
					}
					if err := g.gen.PostponeQualityCheck(ctx, q.ID, sweepBusyPostpone); err != nil {
						return repaired, err
					}
					log.Printf("quality sweep: question %d became busy during rewrite — postponed", q.ID)
					continue
				}
				if errors.Is(err, repositories.ErrQuestionShared) {
					// R-8b: shared by several clones — never rewrite in place.
					if err := g.gen.MarkQuestionsChecked(ctx, []int64{q.ID}); err != nil {
						return repaired, err
					}
					log.Printf("quality sweep: question %d is shared by several tests — left as is", q.ID)
					continue
				}
				if err != nil {
					return repaired, err
				}
				repaired++
				log.Printf("quality sweep: question %d rewritten", q.ID)
			}
		}
	}
	return repaired, nil
}

func letterIndex(letter string) int {
	switch strings.ToUpper(strings.TrimSpace(letter)) {
	case "A":
		return 0
	case "B":
		return 1
	case "C":
		return 2
	case "D":
		return 3
	}
	return -1
}

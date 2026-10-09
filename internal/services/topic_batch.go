package services

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// topicBatchPromptExisting bounds how many already banked question texts of
// the topic are listed in the prompt (so the model does not repeat them).
const topicBatchPromptExisting = 30

// maxHardFlaggedPerBatch is maxHardFlaggedPerReply scaled to the batch size.
var maxHardFlaggedPerBatch = maxHardFlaggedPerReply * repositories.TopicBatchSize / GeneratedQuestionsPerTest

// topicBatchPrompt builds the user prompt of a topic_batch job (B4a): ~10
// questions on ONE catalog topic, the title taken from the topic catalog.
func topicBatchPrompt(subjectName, topicTitle string, existing []string) string {
	var b strings.Builder
	b.WriteString(languageSubjectRule(subjectName))
	fmt.Fprintf(&b, "Предмет: «%s». Составь ровно %d вопросов ТОЛЬКО по одной теме: «%s».\n", subjectName, repositories.TopicBatchSize, topicTitle)
	b.WriteString("\nТребования:\n")
	fmt.Fprintf(&b, "— поле topic каждого вопроса ДОСЛОВНО равно «%s»;\n", topicTitle)
	b.WriteString("— разные аспекты темы, от базовых к сложным (difficulty 2–4);\n")
	b.WriteString("— уровень и формат реального ЕНТ/УБТ для 9–11 классов, без повторов;\n")
	b.WriteString("— правильные ответы распределены по позициям примерно поровну.\n")
	if len(existing) > 0 {
		b.WriteString("\nЭти вопросы по теме уже есть в банке — НЕ повторяй их и не перефразируй:\n")
		for i, t := range existing {
			fmt.Fprintf(&b, "%d. %s\n", i+1, t)
		}
	}
	b.WriteString("\nВыдай строго JSON по схеме.")
	return b.String()
}

// pinTopic sets the topic of every question to the catalog title.
func pinTopic(gt *generatedTest, title string) {
	for i := range gt.Questions {
		gt.Questions[i].Topic = title
	}
}

// runTopicBatch (B4a) generates TopicBatchSize questions on one catalog
// topic and stores them in the bank (no test row). Same model (DeepSeek
// flash, thinking high), limits, daily DeepSeek budget and quality audit
// as the test jobs; banked
// duplicates (same text within the topic) are skipped by SaveBankQuestions.
func (g *GeneratorService) runTopicBatch(ctx context.Context, job *models.GenerationJob, subjectName string) error {
	if job.TopicKey == "" {
		return fmt.Errorf("topic_batch job %d without topic_key", job.ID)
	}
	title, existing, err := g.gen.TopicBankInfo(ctx, job.SubjectID, job.TopicKey, topicBatchPromptExisting)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(existing))
	for _, t := range existing {
		seen[strings.ToLower(strings.TrimSpace(t))] = true
	}

	messages := []deepseek.Message{
		{Role: "system", Content: genSystemPrompt},
		{Role: "user", Content: topicBatchPrompt(subjectName, title, existing)},
	}
	var final *generatedTest
	validate := func(raw string) error {
		gt, err := parseQuestionsJSON(raw, repositories.TopicBatchSize)
		if err != nil {
			return err
		}
		pinTopic(gt, title)
		if n := countHard(auditGenerated(gt)); n > maxHardFlaggedPerBatch {
			return fmt.Errorf("quality audit: %d of %d questions reveal the answer or have broken options", n, len(gt.Questions))
		}
		final = gt
		return nil
	}
	task := fmt.Sprintf("gen %s job %d", job.Kind, job.ID)
	if _, _, err := runSteps(ctx, task, g.genSteps(func() []deepseek.Message { return messages }, genMaxTokens, 0, true), validate); err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if err := g.repairFlagged(ctx, task, subjectName, final); err != nil {
		return fmt.Errorf("quality: %w", err)
	}
	if err := validateQuestions(final, repositories.TopicBatchSize); err != nil {
		return fmt.Errorf("after repair: %w", err)
	}
	pinTopic(final, title)

	seed := make([]models.SeedQuestion, 0, len(final.Questions))
	for _, sq := range final.toSeed() {
		if seen[strings.ToLower(strings.TrimSpace(sq.Text))] {
			continue
		}
		seed = append(seed, sq)
	}
	if len(seed) == 0 {
		log.Printf("generator: topic_batch job %d: every question already in the bank of %q", job.ID, job.TopicKey)
		return nil
	}
	ids, err := g.gen.SaveBankQuestions(ctx, job.SubjectID, job.TopicKey, seed)
	if err != nil {
		return err
	}
	log.Printf("generator: topic_batch job %d stored %d bank question(s) on %q", job.ID, len(ids), job.TopicKey)
	return nil
}

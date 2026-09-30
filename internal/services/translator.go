// Package services — Kazakh translation pipeline for test content.
//
// Cost discipline (the whole point of this service): a question is
// translated EXACTLY ONCE, cached in question_translations and reused by
// every user forever — there is no per-user translation and no repeated
// API call. The Russian question row always stays the master version: when
// a Kazakh user opens a test whose questions have no cached translation
// yet, the whole test is translated in ONE DeepSeek Flash call (low effort
// — translation is mechanical) and stored. If the very same test is
// already cached, zero API calls are made.
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Bihan293/Juz40-test2/internal/deepseek"
	"github.com/Bihan293/Juz40-test2/internal/models"
	"github.com/Bihan293/Juz40-test2/internal/repositories"
)

const (
	// translateMaxTokens caps one translation call (20 questions x
	// question+4 options, plus the low-effort thinking tokens). Kazakh text
	// is comparable in length to Russian, so 8000 tokens is generous.
	translateMaxTokens = 8000
)

// translationSystemPrompt keeps the model strictly in translator mode: the
// answer key is NEVER sent (it is copied from the master row server-side),
// so a sloppy reply can never corrupt the correct answers.
const translationSystemPrompt = `Ты — профессиональный переводчик тестов ЕНТ/УБТ с русского на казахский язык. Переводи точно, официальным академическим стилем казахского языка, без добавлений и сокращений. Формулы, числа, имена собственные и единицы измерения не меняй.

Формат — строго JSON, без пояснений и markdown:
{"translations":[{"id":1,"question":"...","options":["...","...","...","..."],"topic":"..."}]}`

// translationQuestion is one question handed to the model (no answer key).
type translationQuestion struct {
	ID       int      `json:"id"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Topic    string   `json:"topic"`
}

// translatedQuestion is one translated question in the model reply.
type translatedQuestion struct {
	ID       int      `json:"id"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Topic    string   `json:"topic"`
}

type translationResponse struct {
	Translations []translatedQuestion `json:"translations"`
}

// TranslatorService translates test content into Kazakh and caches the
// result. It is the ONLY component that calls the DeepSeek API for
// translation.
type TranslatorService struct {
	ds   *deepseek.Client // nil when DEEPSEEK_API_KEY is not set
	repo *repositories.TranslationRepository

	// inFlight serialises translation of the same test so two concurrent
	// users never pay for the same translation twice.
	mu       sync.Mutex
	inFlight map[int64]*sync.Mutex
}

func NewTranslatorService(ds *deepseek.Client, repo *repositories.TranslationRepository) *TranslatorService {
	return &TranslatorService{ds: ds, repo: repo, inFlight: map[int64]*sync.Mutex{}}
}

// Enabled reports whether translation is configured.
func (t *TranslatorService) Enabled() bool { return t != nil && t.ds != nil && t.repo != nil }

// testLock returns the per-test translation mutex.
func (t *TranslatorService) testLock(testID int64) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.inFlight[testID]
	if !ok {
		m = &sync.Mutex{}
		t.inFlight[testID] = m
	}
	return m
}

// TranslateTest returns the Kazakh version of the given Russian questions of
// ONE test: questionID -> translated question. The correct answers are
// copied from the master questions (never translated). Cached rows are used
// as is; only missing questions go through the model — in a single call.
//
// The result may be incomplete when the model call fails: the caller must
// fall back to the Russian master text for missing questions.
func (t *TranslatorService) TranslateTest(ctx context.Context, testID int64, questions []models.Question) (map[int64]*models.QuestionTranslation, error) {
	if !t.Enabled() || len(questions) == 0 {
		return nil, nil
	}

	// Serialise per test: the second user to open the same untranslated test
	// waits for the first run and then reads the cached rows.
	lock := t.testLock(testID)
	lock.Lock()
	defer lock.Unlock()

	cached, err := t.repo.TranslationsForTest(ctx, testID, models.TestLangKK)
	if err != nil {
		return nil, err
	}
	missing := make([]models.Question, 0, len(questions))
	for _, q := range questions {
		if cached[q.ID] == nil {
			missing = append(missing, q)
		}
	}
	if len(missing) == 0 {
		return cached, nil
	}

	// One call for the whole test (20 questions) — the cheapest way.
	if err := t.translateBatch(ctx, missing); err != nil {
		log.Printf("translator: test %d: translate %d questions: %v", testID, len(missing), err)
		return nil, err
	}
	log.Printf("translator: test %d: translated %d question(s) to kk and cached them", testID, len(missing))

	// Re-read: rows just written by us (or by a concurrent run we raced).
	cached, err = t.repo.TranslationsForTest(ctx, testID, models.TestLangKK)
	if err != nil {
		return nil, err
	}
	if len(cached) < len(questions) {
		return cached, fmt.Errorf("translator: only %d of %d questions cached after translation", len(cached), len(questions))
	}
	return cached, nil
}

// TranslationFor returns the cached Kazakh translation of ONE question, or
// (nil, nil) when it does not exist yet. Used when rendering questions —
// translations are bulk-created by TranslateTest when the test is opened,
// so this is a pure DB lookup with zero API cost.
func (t *TranslatorService) TranslationFor(ctx context.Context, questionID int64, lang string) (*models.QuestionTranslation, error) {
	if !t.Enabled() {
		return nil, nil
	}
	return t.repo.TranslationForQuestion(ctx, questionID, lang)
}

// TranslatedCount returns how many of the given questions already have a
// cached Kazakh translation. Pure DB lookup, zero API cost — used to decide
// whether opening a test will trigger the one-time translation run.
func (t *TranslatorService) TranslatedCount(ctx context.Context, questionIDs []int64) (int, error) {
	if !t.Enabled() {
		return 0, nil
	}
	return t.repo.TranslatedQuestionCount(ctx, questionIDs, models.TestLangKK)
}

// QuestionIDsForTest returns the question ids of a test in their canonical
// test order. Used by the generator to pair the questions of a freshly
// CLONED personal test with the cached translations of the clone source.
func (t *TranslatorService) QuestionIDsForTest(ctx context.Context, testID int64) ([]int64, error) {
	if !t.Enabled() {
		return nil, nil
	}
	return t.repo.QuestionIDsForTest(ctx, testID)
}

// CopyTranslationsToTest carries the cached Kazakh translations of a source
// test over to a freshly CLONED test (same content, new question ids), so
// Kazakh-speaking users reuse the existing translation instead of paying
// for a new DeepSeek call. Pure DB work — zero API cost.
func (t *TranslatorService) CopyTranslationsToTest(ctx context.Context, srcTestID int64, dstQuestionIDs []int64) (int64, error) {
	if !t.Enabled() {
		return 0, nil
	}
	return t.repo.CopyTranslationsToTest(ctx, srcTestID, dstQuestionIDs, models.TestLangKK)
}

// translateBatch translates one batch of questions with a single model call
// and stores the result. The master correct_answer letters are preserved.
func (t *TranslatorService) translateBatch(ctx context.Context, questions []models.Question) error {
	payload := make([]translationQuestion, 0, len(questions))
	correctByID := make(map[int64]string, len(questions))
	masterByIdx := make(map[int]*models.Question, len(questions))
	for i, q := range questions {
		payload = append(payload, translationQuestion{
			ID:       i + 1, // a small local id, never the DB id (shorter prompt)
			Question: q.Text,
			Options:  []string{q.OptionA, q.OptionB, q.OptionC, q.OptionD},
			Topic:    q.Topic,
		})
		correctByID[q.ID] = q.CorrectAnswer
		qi := q
		masterByIdx[i+1] = &qi
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	raw, err := t.ds.TranslateJSON(ctx, []deepseek.Message{
		{Role: "system", Content: translationSystemPrompt},
		{Role: "user", Content: "Переведи на казахский язык этот JSON-массив вопросов и выдай строго JSON по схеме, ровно " + fmt.Sprint(len(payload)) + " переводов, в том же порядке:\n" + string(body)},
	}, translateMaxTokens)
	if err != nil {
		return fmt.Errorf("translate: %w", err)
	}

	tr, err := parseTranslationJSON(raw)
	if err != nil {
		return err
	}
	if len(tr.Translations) != len(payload) {
		return fmt.Errorf("translator: need %d translations, got %d", len(payload), len(tr.Translations))
	}

	out := make([]models.QuestionTranslation, 0, len(tr.Translations))
	for i := range tr.Translations {
		q := &tr.Translations[i]
		master := masterByIdx[q.ID]
		if master == nil {
			return fmt.Errorf("translator: unknown translation id %d", q.ID)
		}
		q.Question = strings.TrimSpace(q.Question)
		if len(q.Question) < 4 {
			return fmt.Errorf("translator: question %d: text too short", q.ID)
		}
		if len(q.Options) != 4 {
			return fmt.Errorf("translator: question %d: need 4 options, got %d", q.ID, len(q.Options))
		}
		for j := range q.Options {
			q.Options[j] = strings.TrimSpace(q.Options[j])
			if q.Options[j] == "" {
				return fmt.Errorf("translator: question %d: empty option %d", q.ID, j+1)
			}
		}
		out = append(out, models.QuestionTranslation{
			QuestionID:    master.ID,
			Lang:          models.TestLangKK,
			Text:          q.Question,
			OptionA:       q.Options[0],
			OptionB:       q.Options[1],
			OptionC:       q.Options[2],
			OptionD:       q.Options[3],
			Topic:         strings.TrimSpace(q.Topic),
			CorrectAnswer: master.CorrectAnswer,
		})
	}
	return t.repo.SaveTranslations(ctx, out, correctByID)
}

// parseTranslationJSON extracts the JSON object from a model reply
// (tolerates accidental markdown fences) and decodes it.
func parseTranslationJSON(raw string) (*translationResponse, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in translator reply")
	}
	var tr translationResponse
	if err := json.Unmarshal([]byte(raw[start:end+1]), &tr); err != nil {
		return nil, fmt.Errorf("decode translator JSON: %w", err)
	}
	return &tr, nil
}

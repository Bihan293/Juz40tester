// Package services — Kazakh translation pipeline for test content.
//
// Cost discipline (the whole point of this service): a question is
// translated EXACTLY ONCE, cached in question_translations and reused by
// every user forever — there is no per-user translation and no repeated
// API call. The Russian question row always stays the master version.
//
// Provider route (translation is mechanical — no paid reasoning needed):
//  1. Groq Qwen 3.8 27B, instruct mode (reasoning_effort=none) — free, fast
//     (~450 tok/s), strong multilingual; the primary translator;
//  2. Groq GPT-OSS 120B (reasoning low) — separate free quota bucket;
//  3. DeepSeek flash — the paid last resort (the old behaviour).
//
// The Groq free tier caps ONE request at 8000 tokens (prompt + max output),
// so the questions are packed into chunks that provably fit that ceiling;
// each chunk is saved as soon as it is translated (partial progress is
// never lost, the next open only translates what is missing). If the very
// same test is already cached, zero API calls are made.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

const (
	// translateMaxTokens caps one translation call (20 questions x
	// question+4 options, plus the low-effort thinking tokens). Kazakh text
	// is comparable in length to Russian, so 8000 tokens is generous.
	translateMaxTokens = 8000

	// Kazakh output is longer in tokens than the Russian input (agglutinative
	// morphology, weaker tokenizer coverage): the reply of a chunk is
	// budgeted at kkOutputFactor x the payload tokens + a fixed overhead.
	kkOutputFactor   = 1.7
	kkOutputOverhead = 150
	// groqTranslateMaxWait: a user is waiting on the translation — never
	// sit on the per-minute window for long; the next free bucket (or the
	// paid fallback) is tried instead.
	groqTranslateMaxWait = 8 * time.Second
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
	gq   *groq.Client     // nil when GROQ_API_KEY is not set
	repo *repositories.TranslationRepository

	// inFlight serialises translation of the same test so two concurrent
	// users never pay for the same translation twice. Entries are
	// reference-counted and removed when the last holder/waiter releases,
	// so the map holds only tests being translated right now.
	mu       sync.Mutex
	inFlight map[int64]*testLockEntry
}

// testLockEntry is a per-test mutex plus the number of goroutines that
// currently hold or wait for it (guarded by TranslatorService.mu).
type testLockEntry struct {
	mu   sync.Mutex
	refs int
}

func NewTranslatorService(ds *deepseek.Client, repo *repositories.TranslationRepository) *TranslatorService {
	return &TranslatorService{ds: ds, repo: repo, inFlight: map[int64]*testLockEntry{}}
}

// WithGroq wires the free Groq provider (Qwen primary, GPT-OSS secondary).
func (t *TranslatorService) WithGroq(gq *groq.Client) *TranslatorService {
	if gq != nil && gq.Enabled() {
		t.gq = gq
	}
	return t
}

// Enabled reports whether translation is configured (any provider).
func (t *TranslatorService) Enabled() bool {
	return t != nil && t.repo != nil && (t.ds != nil || t.gq != nil)
}

// lockTest acquires the per-test translation mutex and returns its release
// function. Release must be deferred: it unlocks and drops the map entry
// once nobody else holds or waits for it — also on error and on panic.
func (t *TranslatorService) lockTest(testID int64) (release func()) {
	t.mu.Lock()
	e, ok := t.inFlight[testID]
	if !ok {
		e = &testLockEntry{}
		t.inFlight[testID] = e
	}
	e.refs++
	t.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		t.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(t.inFlight, testID)
		}
		t.mu.Unlock()
	}
}

// inFlightLen reports the number of tracked per-test locks (for tests).
func (t *TranslatorService) inFlightLen() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inFlight)
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
	release := t.lockTest(testID)
	defer release()

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

// translateBatch translates the given questions (packed into chunks that
// fit one Groq free-tier request) and stores every chunk right away. The
// master correct_answer letters are preserved (never sent to the model).
func (t *TranslatorService) translateBatch(ctx context.Context, questions []models.Question) error {
	var errs []error
	for _, ch := range chunkForTranslation(questions) {
		if err := t.translateChunk(ctx, ch); err != nil {
			// Keep going: the other chunks can still be cached; the missing
			// questions fall back to Russian and are retried on next open.
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// translationPayload builds the compact model payload of a chunk (local ids
// 1..n — shorter than DB ids — and no answer key).
func translationPayload(questions []models.Question) []translationQuestion {
	payload := make([]translationQuestion, 0, len(questions))
	for i, q := range questions {
		payload = append(payload, translationQuestion{
			ID:       i + 1,
			Question: q.Text,
			Options:  []string{q.OptionA, q.OptionB, q.OptionC, q.OptionD},
			Topic:    q.Topic,
		})
	}
	return payload
}

func translationMessages(payload []translationQuestion) ([]deepseek.Message, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []deepseek.Message{
		{Role: "system", Content: translationSystemPrompt},
		{Role: "user", Content: "Переведи на казахский язык этот JSON-массив вопросов и выдай строго JSON по схеме, ровно " + fmt.Sprint(len(payload)) + " переводов, в том же порядке:\n" + string(body)},
	}, nil
}

// translationOutputBudget estimates the reply size (tokens) of a chunk.
func translationOutputBudget(payload []translationQuestion) int {
	body, _ := json.Marshal(payload)
	return int(float64(groq.EstimateTextTokens(string(body)))*kkOutputFactor) + kkOutputOverhead
}

// gptOSSTranslateHeadroom: GPT-OSS always spends some reasoning tokens even
// at effort=low — extra output budget on top of the visible JSON.
const (
	gptOSSTranslateHeadroom    = 1200
	gptOSSTranslateMinHeadroom = 300
)

// fitsOneGroqRequest reports whether a chunk (prompt + expected reply) fits
// the per-request ceiling (free-tier TPM) of the primary Groq translator.
func fitsOneGroqRequest(questions []models.Question) bool {
	payload := translationPayload(questions)
	msgs, err := translationMessages(payload)
	if err != nil {
		return false
	}
	return groq.Budget(groq.ModelQwen27B, toGroqMessages(msgs)) >= translationOutputBudget(payload)
}

// chunkForTranslation greedily packs questions into the fewest chunks that
// each fit one Groq request (typically 2–3 chunks for a 20-question test).
// A single oversized question still forms its own chunk — the Groq step
// then reports ErrTooLarge locally (no HTTP call) and DeepSeek handles it.
func chunkForTranslation(questions []models.Question) [][]models.Question {
	var chunks [][]models.Question
	var cur []models.Question
	for _, q := range questions {
		next := append(append([]models.Question(nil), cur...), q)
		if len(cur) > 0 && !fitsOneGroqRequest(next) {
			chunks = append(chunks, cur)
			cur = []models.Question{q}
			continue
		}
		cur = next
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// translationSteps returns the provider route of one chunk.
func (t *TranslatorService) translationSteps(msgs []deepseek.Message, outBudget int) []aiStep {
	var steps []aiStep
	if t.gq != nil {
		gm := toGroqMessages(msgs)
		steps = append(steps,
			groqStep(t.gq, groq.Request{
				Model: groq.ModelQwen27B, Messages: gm,
				// +33% headroom over the estimate (clamped to the per-request
				// ceiling by the client); the limiter re-credits unused tokens.
				MaxTokens: outBudget + outBudget/3, MinTokens: outBudget,
				Effort:      groq.EffortNone, // instruct mode — translation needs no reasoning
				Temperature: 0.3, TopP: 0.8,
				Schema: translationJSONSchema, SchemaName: "kk_translation",
				MaxWait: groqTranslateMaxWait,
			}),
			groqStep(t.gq, groq.Request{
				Model: groq.ModelGPTOSS120B, Messages: gm,
				MaxTokens: outBudget + gptOSSTranslateHeadroom,
				MinTokens: outBudget + gptOSSTranslateMinHeadroom,
				Effort:    groq.EffortLow,
				Schema:    translationJSONSchema, SchemaName: "kk_translation",
				MaxWait: groqTranslateMaxWait,
			}),
		)
	}
	if t.ds != nil {
		ds := t.ds
		steps = append(steps, aiStep{
			name: "deepseek/" + ds.ReasonerModel() + "(translate)",
			run: func(ctx context.Context) (string, error) {
				return ds.TranslateJSON(ctx, msgs, translateMaxTokens)
			},
		})
	}
	return steps
}

// translationJSONSchema is the strict Structured Outputs schema of a reply.
var translationJSONSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"translations": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "integer"},
					"question": map[string]any{"type": "string"},
					"options":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"topic":    map[string]any{"type": "string"},
				},
				"required":             []string{"id", "question", "options", "topic"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"translations"},
	"additionalProperties": false,
}

// translateChunk translates one chunk through the provider route and saves
// it. Every reply is validated locally before anything is written.
func (t *TranslatorService) translateChunk(ctx context.Context, questions []models.Question) error {
	payload := translationPayload(questions)
	msgs, err := translationMessages(payload)
	if err != nil {
		return err
	}
	correctByID := make(map[int64]string, len(questions))
	masterByIdx := make(map[int]*models.Question, len(questions))
	for i := range questions {
		correctByID[questions[i].ID] = questions[i].CorrectAnswer
		masterByIdx[i+1] = &questions[i]
	}

	var out []models.QuestionTranslation
	validate := func(raw string) error {
		rows, err := buildTranslations(raw, len(payload), masterByIdx)
		if err != nil {
			return err
		}
		out = rows
		return nil
	}
	task := fmt.Sprintf("translate %d q", len(questions))
	if _, _, err := runSteps(ctx, task, t.translationSteps(msgs, translationOutputBudget(payload)), validate); err != nil {
		return fmt.Errorf("translate: %w", err)
	}
	return t.repo.SaveTranslations(ctx, out, correctByID)
}

// buildTranslations parses and validates a model reply against the chunk.
func buildTranslations(raw string, want int, masterByIdx map[int]*models.Question) ([]models.QuestionTranslation, error) {
	tr, err := parseTranslationJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(tr.Translations) != want {
		return nil, fmt.Errorf("translator: need %d translations, got %d", want, len(tr.Translations))
	}
	seen := make(map[int]bool, want)
	out := make([]models.QuestionTranslation, 0, want)
	for i := range tr.Translations {
		q := &tr.Translations[i]
		master := masterByIdx[q.ID]
		if master == nil {
			return nil, fmt.Errorf("translator: unknown translation id %d", q.ID)
		}
		if seen[q.ID] {
			return nil, fmt.Errorf("translator: duplicate translation id %d", q.ID)
		}
		seen[q.ID] = true
		q.Question = strings.TrimSpace(q.Question)
		if len(q.Question) < 4 {
			return nil, fmt.Errorf("translator: question %d: text too short", q.ID)
		}
		if len(q.Options) != 4 {
			return nil, fmt.Errorf("translator: question %d: need 4 options, got %d", q.ID, len(q.Options))
		}
		for j := range q.Options {
			q.Options[j] = strings.TrimSpace(q.Options[j])
			if q.Options[j] == "" {
				return nil, fmt.Errorf("translator: question %d: empty option %d", q.ID, j+1)
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
	return out, nil
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

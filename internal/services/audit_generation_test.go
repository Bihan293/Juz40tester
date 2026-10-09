package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// --- free Groq retry before the paid step -----------------------------------

func TestRunStepsWaitsForFreeQuotaBeforePaying(t *testing.T) {
	var freeCalls, paidCalls int32
	steps := []aiStep{
		{name: "groq-a", run: func(context.Context) (string, error) {
			if atomic.AddInt32(&freeCalls, 1) == 1 {
				return "", &groq.RateLimitError{Model: "a", Reason: "tokens per minute", RetryAfter: 20 * time.Millisecond}
			}
			return "good", nil
		}},
		{name: "deepseek", paid: true, run: func(context.Context) (string, error) {
			atomic.AddInt32(&paidCalls, 1)
			return "good", nil
		}},
	}
	raw, by, err := runStepsOpts(context.Background(), "t", steps, func(s string) error { return nil }, stepOpts{freeRetryWait: time.Second})
	if err != nil || raw != "good" || by != "groq-a" {
		t.Fatalf("got %q %q %v", raw, by, err)
	}
	if paidCalls != 0 || freeCalls != 2 {
		t.Fatalf("free %d paid %d: the paid step must not run when the free quota is back soon", freeCalls, paidCalls)
	}
}

func TestRunStepsPaysWhenFreeQuotaIsDailyOrFar(t *testing.T) {
	for _, rl := range []*groq.RateLimitError{
		{Model: "a", Reason: "requests per day", RetryAfter: time.Millisecond},
		{Model: "a", Reason: "tokens per minute", RetryAfter: time.Hour},
	} {
		var freeCalls, paidCalls int32
		steps := []aiStep{
			{name: "groq-a", run: func(context.Context) (string, error) {
				atomic.AddInt32(&freeCalls, 1)
				return "", rl
			}},
			{name: "deepseek", paid: true, run: func(context.Context) (string, error) {
				atomic.AddInt32(&paidCalls, 1)
				return "good", nil
			}},
		}
		_, by, err := runStepsOpts(context.Background(), "t", steps, func(string) error { return nil }, stepOpts{freeRetryWait: time.Second})
		if err != nil || by != "deepseek" || freeCalls != 1 || paidCalls != 1 {
			t.Fatalf("%v: by %q err %v free %d paid %d", rl, by, err, freeCalls, paidCalls)
		}
	}
}

func TestRunStepsDoesNotRetryRejectedFreeReply(t *testing.T) {
	var freeCalls int32
	steps := []aiStep{
		{name: "groq-a", run: func(context.Context) (string, error) {
			atomic.AddInt32(&freeCalls, 1)
			return "bad", nil
		}},
		{name: "deepseek", paid: true, run: func(context.Context) (string, error) { return "good", nil }},
	}
	_, by, err := runStepsOpts(context.Background(), "t", steps, func(s string) error {
		if s != "good" {
			return errors.New("rejected")
		}
		return nil
	}, stepOpts{freeRetryWait: time.Second})
	if err != nil || by != "deepseek" || freeCalls != 1 {
		t.Fatalf("by %q err %v free calls %d", by, err, freeCalls)
	}
}

func TestStepNotNeededIsSilent(t *testing.T) {
	r := &genRun{}
	ctx := withGenRun(context.Background(), r)
	_, by, err := runSteps(ctx, "t", []aiStep{
		{name: "cond", run: func(context.Context) (string, error) { return "", errStepNotNeeded }},
		{name: "real", run: func(context.Context) (string, error) { return "ok", nil }},
	}, func(string) error { return nil })
	if err != nil || by != "real" {
		t.Fatalf("by %q err %v", by, err)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("a skipped conditional step must not count as an AI call (calls=%d)", n)
	}
}

// --- translation: cheap Qwen retry before paid DeepSeek ---------------------

func groqReply(content string) []byte {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 100, "total_tokens": 200},
	})
	return b
}

func translationReply(ids ...int) string {
	var tr translationResponse
	for _, id := range ids {
		tr.Translations = append(tr.Translations, translatedQuestion{
			ID: id, Question: fmt.Sprintf("Сұрақ %d қай органоид?", id),
			Options: []string{"Митохондрия", "Рибосома", "Гольджи аппараты", "Лизосома"}, Topic: "Жасуша",
		})
	}
	b, _ := json.Marshal(tr)
	return string(b)
}

func TestTranslationDuplicateIDRetriesQwenBeforeDeepSeek(t *testing.T) {
	var mu sync.Mutex
	var models []string
	var lastPrompt string
	groqSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string         `json:"model"`
			Messages []groq.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		models = append(models, req.Model)
		n := len(models)
		lastPrompt = req.Messages[len(req.Messages)-1].Content
		mu.Unlock()
		if n == 1 {
			_, _ = w.Write(groqReply(translationReply(1, 1, 3))) // duplicate id 1, id 2 missing
			return
		}
		_, _ = w.Write(groqReply(translationReply(1, 2, 3)))
	}))
	defer groqSrv.Close()
	var dsCalls int32
	dsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&dsCalls, 1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer dsSrv.Close()

	tr := &TranslatorService{gq: groq.New("k", groqSrv.URL), ds: deepseek.New("k", "", "", dsSrv.URL)}
	rows, err := tr.translateChunkRows(context.Background(), sampleQuestions(3))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows: %d", len(rows))
	}
	if dsCalls != 0 {
		t.Fatalf("paid DeepSeek called %d time(s) for a duplicate-id slip", dsCalls)
	}
	if len(models) != 2 || models[0] != groq.ModelQwen27B || models[1] != groq.ModelQwen27B {
		t.Fatalf("route: %v (want Qwen, Qwen retry)", models)
	}
	if !strings.Contains(lastPrompt, "duplicate translation id") {
		t.Fatalf("the retry must carry the rejection reason, prompt tail: %q", lastPrompt[max(0, len(lastPrompt)-200):])
	}
}

func TestTranslationRouteWithRetry(t *testing.T) {
	tr := &TranslatorService{gq: groq.New("k", ""), ds: deepseek.New("k", "", "", "")}
	r := stepNames(tr.translationSteps([]deepseek.Message{{Role: "user", Content: "x"}}, 1000, func() string { return "" }))
	want := "groq/qwen/qwen3.8-27b(none),groq/qwen/qwen3.8-27b(none)(retry),groq/openai/gpt-oss-120b(low),deepseek/deepseek-flash(translate)"
	if r != want {
		t.Fatalf("translation route:\n got %s\nwant %s", r, want)
	}
	steps := tr.translationSteps([]deepseek.Message{{Role: "user", Content: "x"}}, 1000, func() string { return "" })
	if !steps[len(steps)-1].paid {
		t.Fatal("the DeepSeek step must be marked paid")
	}
}

// Every chunk must fit the SECOND free translator (GPT-OSS with its
// minimal reasoning headroom), not only Qwen — otherwise a rejected Qwen
// reply skipped GPT-OSS locally and went to the paid fallback.
func TestChunksFitGPTOSSToo(t *testing.T) {
	qs := sampleQuestions(40)
	for i := range qs {
		qs[i].Text += strings.Repeat(" длинное уточнение условия", i%5)
	}
	for _, ch := range chunkForTranslation(qs) {
		payload := translationPayload(ch)
		msgs, _ := translationMessages(payload)
		out := translationOutputBudget(payload)
		if len(ch) > 1 {
			if _, err := groq.FitMaxTokens(groq.ModelGPTOSS120B, toGroqMessages(msgs), out+gptOSSTranslateHeadroom, out+gptOSSTranslateMinHeadroom); err != nil {
				t.Fatalf("chunk of %d questions does not fit GPT-OSS: %v", len(ch), err)
			}
		}
	}
}

// --- batch generation: new-topic budget is not leaked -----------------------

func TestRejectedBatchReleasesNewTopicHolds(t *testing.T) {
	cat := &models.TopicCatalog{Aliases: map[string]string{"генетика": "генетика"}, Titles: []string{"Генетика"}}
	spec := &genSpec{kind: models.TestKindChain, catalog: cat, slots: []genSlot{
		{Difficulty: 3}, {Difficulty: 3}, {Difficulty: 3}, {Difficulty: 3},
	}}
	g := &GeneratorService{}
	mk := func(text, topic string, diff int) generatedQuestion {
		return generatedQuestion{Text: text, Options: []string{"Первый вариант", "Второй вариант", "Третий вариант", "Четвёртый вариант"}, Correct: 1, Topic: topic, Difficulty: diff}
	}
	// One usable question on a NEW topic out of 4 → the reply is rejected
	// (fewer than half usable); its hold on the new topic must be released.
	bad := batchReply(mk("Вопрос про новую тему один?", "Новая тема А", 3), mk("Вопрос номер два без опций?", "Генетика", 1),
		mk("Вопрос номер три без опций?", "Генетика", 1), mk("Вопрос номер четыре?", "Генетика", 1))
	if _, err := g.matchBatch(context.Background(), spec, []int{0, 1, 2, 3}, bad, nil, false); err == nil {
		t.Fatal("expected the reply to be rejected")
	}
	if len(spec.newTopics) != 0 {
		t.Fatalf("rejected reply kept new-topic holds: %v", spec.newTopics)
	}
	// Two OTHER new topics are still allowed afterwards.
	good := batchReply(mk("Вопрос про тему Б номер один?", "Новая тема Б", 3), mk("Вопрос про тему В номер два?", "Новая тема В", 3),
		mk("Генетика: вопрос номер три?", "Генетика", 3), mk("Генетика: вопрос номер четыре про гены?", "Генетика", 3))
	got, err := g.matchBatch(context.Background(), spec, []int{0, 1, 2, 3}, good, nil, false)
	if err != nil || len(got) != 4 {
		t.Fatalf("good reply after a rejected one: %d questions, %v", len(got), err)
	}
	// Dropping a placed question releases its hold.
	for _, q := range got {
		spec.releaseTopic(q)
	}
	if len(spec.newTopics) != 0 {
		t.Fatalf("holds left after release: %v", spec.newTopics)
	}
}

func TestWeakTopicQuestionPrefersItsSlot(t *testing.T) {
	spec := &genSpec{kind: models.TestKindChain, slots: []genSlot{
		{Difficulty: 3}, {Topic: "Генетика", Difficulty: 3},
	}}
	g := &GeneratorService{}
	q := generatedQuestion{Text: "Какой закон Менделя описывает расщепление?", Options: []string{"Первый закон", "Второй закон", "Третий закон", "Закон Моргана"}, Correct: 1, Topic: "Генетика", Difficulty: 3}
	got, err := g.matchBatch(context.Background(), spec, []int{0, 1}, batchReply(q), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got[1] == nil || got[0] != nil {
		t.Fatalf("the weak-topic question must fill the weak-topic slot, got %v", got)
	}
}

// --- topic matching: cosmetic noise is not «тема не по спецификации» -------

func TestTopicMatchesCosmeticNoise(t *testing.T) {
	for _, got := range []string{"«Фотосинтез»", "Фотосинтез.", "\"фотосинтез\"", " ФОТОСИНТЕЗ "} {
		if !topicMatches(got, "Фотосинтез", nil) {
			t.Fatalf("%q must match «Фотосинтез»", got)
		}
	}
	if !topicMatches("Свёртывание крови", "Свертывание крови", nil) {
		t.Fatal("ё/е spelling must match")
	}
	if topicMatches("Фотосинтез растений", "Фотосинтез", nil) {
		t.Fatal("a different topic must not match")
	}
	gt := &generatedTest{Questions: []generatedQuestion{{Topic: "«Генетика»"}, {Topic: "Клетка."}}}
	if err := validatePersonalCoverage(gt, []string{"Генетика", "Клетка"}, nil); err != nil {
		t.Fatalf("cosmetic noise rejected: %v", err)
	}
	if gt.Questions[0].Topic != "Генетика" || gt.Questions[1].Topic != "Клетка" {
		t.Fatalf("topics not rewritten to the requested spelling: %+v", gt.Questions)
	}
}

func TestPersonalNearDuplicatesRejected(t *testing.T) {
	g := &GeneratorService{}
	spec := &genSpec{kind: models.TestKindPersonal, personalTopics: []string{"Генетика"}}
	gt := &generatedTest{Questions: []generatedQuestion{
		{Text: "Какой закон Менделя описывает расщепление признаков во втором поколении?", Topic: "Генетика", Difficulty: 3},
		{Text: "Какой закон Менделя описывает расщепление признаков во втором поколении гибридов?", Topic: "Генетика", Difficulty: 3},
	}}
	if err := g.validateReply(gt, spec); rejectClass(err) != rejectRepeat {
		t.Fatalf("near-duplicate questions in a weak-topics test must be rejected, got %v", err)
	}
}

// --- repair ------------------------------------------------------------------

func TestRewriteKeepsDifficulty(t *testing.T) {
	orig := generatedQuestion{Text: "Исходный вопрос о клетке?", Topic: "Клетка", Difficulty: 4}
	q := generatedQuestion{Text: "Какой органоид синтезирует белок?", Options: []string{"Митохондрия", "Рибосома", "Лизосома", "Вакуоль"}, Correct: 1, Topic: "Другое", Difficulty: 1}
	if err := checkRewrite(orig, &q); err != nil {
		t.Fatal(err)
	}
	if q.Difficulty != 4 || q.Topic != "Клетка" {
		t.Fatalf("rewrite drifted: difficulty %d topic %q", q.Difficulty, q.Topic)
	}
}

func TestSoftOnlyRepairIsFree(t *testing.T) {
	g := &GeneratorService{gq: groq.New("k", ""), ds: deepseek.New("k", "", "", "")}
	msgs := []deepseek.Message{{Role: "user", Content: "x"}}
	if s := stepNames(g.repairSteps(msgs, true)); strings.Contains(s, "deepseek") {
		t.Fatalf("soft-only repair must not pay: %s", s)
	}
	if s := stepNames(g.repairSteps(msgs)); !strings.Contains(s, "deepseek") {
		t.Fatalf("hard repair keeps the paid fallback: %s", s)
	}
}

// --- admin alerts --------------------------------------------------------------

type sentAlert struct {
	chat int64
	text string
}

func newTestAlerter(t *testing.T) (*AdminAlerter, *[]sentAlert, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var sent []sentAlert
	a := NewAdminAlerter([]int64{11, 22}, func(_ context.Context, chat int64, text string) error {
		mu.Lock()
		sent = append(sent, sentAlert{chat, text})
		mu.Unlock()
		return nil
	})
	return a, &sent, &mu
}

func TestAdminAlerterRateLimits(t *testing.T) {
	a, sent, _ := newTestAlerter(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	if !a.Alert(context.Background(), "k", time.Hour, "one") {
		t.Fatal("first alert must be sent")
	}
	if a.Alert(context.Background(), "k", time.Hour, "two") {
		t.Fatal("repeat inside the interval must be suppressed")
	}
	if !a.Alert(context.Background(), "other", time.Hour, "three") {
		t.Fatal("another key is independent")
	}
	now = now.Add(61 * time.Minute)
	if !a.Alert(context.Background(), "k", time.Hour, "four") {
		t.Fatal("after the interval the key may alert again")
	}
	if len(*sent) != 6 { // 3 alerts × 2 admins
		t.Fatalf("sent %d messages, want 6", len(*sent))
	}
	if NewAdminAlerter(nil, func(context.Context, int64, string) error { return nil }) != nil {
		t.Fatal("no admins → no alerter")
	}
	var nilA *AdminAlerter
	nilA.AlertAsync(context.Background(), "x", time.Hour, "y") // must not panic
}

func TestAdminAlerterClusterClaim(t *testing.T) {
	a, sent, _ := newTestAlerter(t)
	a.WithClaim(func(context.Context, string, time.Duration) (bool, error) { return false, nil })
	if a.Alert(context.Background(), "k", time.Hour, "x") || len(*sent) != 0 {
		t.Fatal("an alert claimed by another instance must not be sent")
	}
	b, sentB, _ := newTestAlerter(t)
	b.WithClaim(func(context.Context, string, time.Duration) (bool, error) { return false, errors.New("db down") })
	if !b.Alert(context.Background(), "k", time.Hour, "x") || len(*sentB) != 2 {
		t.Fatal("a claim error falls back to the local guard and still alerts")
	}
}

type fakeBudget struct{ err error }

func (f *fakeBudget) Reserve(context.Context, float64) error   { return f.err }
func (f *fakeBudget) Settle(context.Context, float64, float64) {}

func TestCapAlertOncePerDay(t *testing.T) {
	a, sent, mu := newTestAlerter(t)
	inner := &fakeBudget{err: fmt.Errorf("%w ($2.00/day)", deepseek.ErrBudgetExceeded)}
	b := WithCapAlert(inner, a, 2)
	for i := 0; i < 5; i++ {
		if err := b.Reserve(context.Background(), 0.01); !errors.Is(err, deepseek.ErrBudgetExceeded) {
			t.Fatalf("the wrapper must pass the cap error through: %v", err)
		}
	}
	a.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(*sent) != 2 { // one alert × 2 admins
		t.Fatalf("cap alert sent %d messages, want 2", len(*sent))
	}
	if !strings.Contains((*sent)[0].text, "$2.00") {
		t.Fatalf("alert text: %q", (*sent)[0].text)
	}
	inner.err = nil
	if err := b.Reserve(context.Background(), 0.01); err != nil {
		t.Fatal(err)
	}
	if WithCapAlert(inner, nil, 2) != deepseek.Budget(inner) {
		t.Fatal("without an alerter the budget is returned as is")
	}
}

func TestJobFailureAlerts(t *testing.T) {
	a, sent, mu := newTestAlerter(t)
	g := &GeneratorService{alerts: a}
	job := &models.GenerationJob{ID: 7, Kind: models.TestKindChain, SubjectID: 3, TestNumber: 12, Attempts: 1}
	g.noteJobFailure(context.Background(), job, errors.New("groq: boom"))
	a.Wait()
	if len(*sent) != 0 {
		t.Fatal("a first non-final failure must not alert")
	}
	job.Attempts = maxJobAttempts
	g.noteJobFailure(context.Background(), job, errors.New("deepseek: boom"))
	a.Wait()
	mu.Lock()
	n := len(*sent)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("final failure: %d messages, want 2", n)
	}
	// A third failure in a row → the streak alert; the same final slot is
	// not repeated.
	g.noteJobFailure(context.Background(), job, errors.New("deepseek: boom"))
	a.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(*sent) != 4 || !strings.Contains((*sent)[3].text, "подряд") {
		t.Fatalf("streak alert missing: %d messages", len(*sent))
	}
	g.noteJobSuccess()
	if g.failStreak.n != 0 {
		t.Fatal("success must reset the streak")
	}
}

func TestPaidCallsPerJobAreCapped(t *testing.T) {
	ctx := withPaidCallLimit(context.Background(), 2)
	for i := 0; i < 2; i++ {
		if err := takePaidCall(ctx); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if err := takePaidCall(ctx); !errors.Is(err, errPaidCallLimit) {
		t.Fatalf("third paid call must be refused, got %v", err)
	}
	if err := takePaidCall(context.Background()); err != nil {
		t.Fatal("no counter → no limit")
	}
	// The DeepSeek step of a generation route honours the cap without an
	// HTTP call.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "no", http.StatusBadRequest)
	}))
	defer srv.Close()
	g := &GeneratorService{ds: deepseek.New("k", "", "", srv.URL)}
	steps := g.generationSteps([]deepseek.Message{{Role: "user", Content: "x"}}, models.TestKindPersonal, 1)
	full := withPaidCallLimit(context.Background(), 0)
	if _, _, err := runSteps(full, "t", steps, func(string) error { return nil }); !errors.Is(err, errPaidCallLimit) {
		t.Fatalf("want the per-job limit error, got %v", err)
	}
	if hits != 0 {
		t.Fatalf("a capped paid step must not reach DeepSeek (%d calls)", hits)
	}
}

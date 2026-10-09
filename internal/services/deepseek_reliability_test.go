package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// «X (уточнение)», «X: подтема», «X — подтема» count as topic X; the
// requested topic itself is never stripped.
func TestTopicMatchesQualifiedTopic(t *testing.T) {
	for _, got := range []string{"Генетика (законы Менделя)", "генетика: моногибридное скрещивание", "Генетика — сцепленное наследование"} {
		if !topicMatches(got, "Генетика", nil) {
			t.Fatalf("%q must count as «Генетика»", got)
		}
	}
	if topicMatches("Генетика", "Генетика (законы Менделя)", nil) {
		t.Fatal("the requested topic must keep its qualifier")
	}
	if topicMatches("Клетка (митоз)", "Генетика", nil) {
		t.Fatal("a different topic must not match")
	}
	gt := &generatedTest{Questions: []generatedQuestion{{Topic: "Генетика (задачи)"}, {Topic: "Клетка"}}}
	if err := validatePersonalCoverage(gt, []string{"Генетика", "Клетка"}, nil); err != nil {
		t.Fatalf("qualified topic rejected: %v", err)
	}
	if gt.Questions[0].Topic != "Генетика" {
		t.Fatalf("qualified topic not rewritten: %q", gt.Questions[0].Topic)
	}
}

// Rescue rounds accept a slightly off difficulty and, for a personal test,
// any of the requested weak topics.
func TestMatchBatchRelaxed(t *testing.T) {
	g := &GeneratorService{}
	spec := &genSpec{
		kind:           models.TestKindPersonal,
		personalTopics: []string{"Генетика", "Клетка"},
		slots:          []genSlot{{Topic: "Генетика", Difficulty: 1}, {Topic: "Генетика", Difficulty: 1}},
	}
	raw := batchReply(
		generatedQuestion{Text: "Какой закон Менделя описывает расщепление", Options: []string{"первый", "второй", "третий", "четвёртый"}, Correct: 1, Topic: "Генетика", Difficulty: 3},
		generatedQuestion{Text: "Какая органелла синтезирует белок в клетке", Options: []string{"рибосома", "лизосома", "вакуоль", "центриоль"}, Correct: 0, Topic: "Клетка", Difficulty: 2},
	)
	if _, err := g.matchBatch(context.Background(), spec, []int{0, 1}, raw, nil, false); err == nil {
		t.Fatal("strict round must reject difficulty 3 for a level-1 slot")
	}
	got, err := g.matchBatch(context.Background(), spec, []int{0, 1}, raw, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Topic != "Генетика" || got[1].Topic != "Клетка" {
		t.Fatalf("relaxed placement: %+v %+v", got[0], got[1])
	}
}

func TestUrgentJobRetriesSooner(t *testing.T) {
	if d := jobRetryDelay(&models.GenerationJob{Urgent: true}); d != urgentRetryDelay || d > 2*time.Minute {
		t.Fatalf("urgent retry delay %s", d)
	}
	if d := jobRetryDelay(&models.GenerationJob{}); d != retryDelay {
		t.Fatalf("background retry delay %s", d)
	}
}

// The DeepSeek steps carry no step timeout: their call timeout starts after
// the semaphore slot (waiting in the queue never eats the model's time).
func TestDeepSeekStepsTimeoutAfterSemaphore(t *testing.T) {
	g := &GeneratorService{ds: deepseek.New("k", "", "", "")}
	msgs := func() []deepseek.Message { return []deepseek.Message{{Role: "user", Content: "x"}} }
	for _, st := range append(g.batchSteps(msgs, models.TestKindChain, 1, 0), g.generationStepsDyn(msgs, models.TestKindChain, 1)...) {
		if st.timeout != 0 {
			t.Fatalf("%s: step timeout %s would include the semaphore wait", st.name, st.timeout)
		}
	}
	if jobTimeout <= deepseekGenReserve+2*groqStepTimeout || stuckJobTimeout <= jobTimeout {
		t.Fatal("job timeout must fit two Groq steps plus the DeepSeek reserve, and stay below the reaper timeout")
	}
	if deepseekGenReserve < deepseekGenCallTimeout {
		t.Fatal("the reserve must cover the DeepSeek call timeout")
	}
}

// A batched test where the strict rounds cannot fill a few slots is
// completed by the rescue rounds instead of failing as a whole.
func TestGenerateBatchedRescueFillsMissing(t *testing.T) {
	stems := []string{
		"Какой закон Менделя описывает расщепление признаков",
		"Сколько хромосом в соматической клетке человека",
		"Как называется участок ДНК кодирующий белок",
		"Какой тип наследования у дальтонизма",
		"Что такое генотип организма в генетике",
		"Как называется скрещивание по одной паре признаков",
		"Кто открыл законы наследственности гороха",
		"Какая аллель проявляется у гетерозиготы",
	}
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1))
		var qs []generatedQuestion
		for k := 0; k < 2; k++ {
			s := stems[(2*i+k)%len(stems)]
			qs = append(qs, generatedQuestion{
				Text:    fmt.Sprintf("%s (вариант %d)", s, 2*i+k),
				Options: []string{"первый ответ", "второй ответ", "третий ответ", "четвёртый ответ"},
				Correct: (i + k) % 4, Topic: "Генетика (подтема)", Difficulty: 3,
			})
		}
		content, _ := json.Marshal(generatedTest{Questions: qs})
		resp, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}}})
		_, _ = w.Write(resp)
	}))
	defer srv.Close()
	g := &GeneratorService{ds: deepseek.New("k", "", "", srv.URL)}
	spec := &genSpec{
		kind:           models.TestKindPersonal,
		subjectName:    "Биология",
		personalTopics: []string{"Генетика"},
		// Level 1 slots: the model answers at level 3 — off by 2, rejected
		// by the strict rounds (slack 1), accepted by the rescue (slack 2).
		slots: []genSlot{{Topic: "Генетика", Difficulty: 1}, {Topic: "Генетика", Difficulty: 1}},
	}
	job := &models.GenerationJob{Kind: models.TestKindPersonal, Attempts: 1}
	gt, err := g.generateBatched(context.Background(), job, spec)
	if err != nil {
		t.Fatalf("rescue must complete the test: %v", err)
	}
	if len(gt.Questions) != 2 {
		t.Fatalf("got %d questions", len(gt.Questions))
	}
	for _, q := range gt.Questions {
		if q.Topic != "Генетика" {
			t.Fatalf("topic not normalised: %q", q.Topic)
		}
	}
}

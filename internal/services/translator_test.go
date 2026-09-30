package services

import (
	"strings"
	"testing"
)

// TestParseTranslationJSONOK checks the happy path: a valid translation
// payload for a whole test decodes into the expected structure.
func TestParseTranslationJSONOK(t *testing.T) {
	raw := `{"translations":[
		{"id":1,"question":"Қазақстанның астанасы қандай қала?","options":["Алматы","Астана","Шымкент","Қарағанды"],"topic":"География"},
		{"id":2,"question":"2 + 2 неге тең?","options":["3","4","5","6"],"topic":"Математика"}
	]}`
	tr, err := parseTranslationJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tr.Translations) != 2 {
		t.Fatalf("need 2 translations, got %d", len(tr.Translations))
	}
	if tr.Translations[0].ID != 1 || len(tr.Translations[0].Options) != 4 {
		t.Fatalf("bad first translation: %+v", tr.Translations[0])
	}
	if tr.Translations[1].Question != "2 + 2 неге тең?" {
		t.Fatalf("kazakh text mangled: %q", tr.Translations[1].Question)
	}
}

// TestParseTranslationJSONToleratesFences: models sometimes wrap the JSON in
// markdown fences or add prose around it — the parser must still extract the
// object (a thrown-away reply is a thrown-away paid API call).
func TestParseTranslationJSONToleratesFences(t *testing.T) {
	raw := "Вот перевод:\n```json\n{\"translations\":[{\"id\":1,\"question\":\"Сұрақ\",\"options\":[\"а\",\"б\",\"в\",\"г\"],\"topic\":\"Тақырып\"}]}\n```\nГотово."
	tr, err := parseTranslationJSON(raw)
	if err != nil {
		t.Fatalf("fenced JSON must parse: %v", err)
	}
	if len(tr.Translations) != 1 || tr.Translations[0].Question != "Сұрақ" {
		t.Fatalf("bad parse: %+v", tr.Translations)
	}
}

// TestParseTranslationJSONRejectsGarbage: a reply without a JSON object must
// fail fast so the caller falls back to the Russian master instead of
// storing garbage into the translation cache.
func TestParseTranslationJSONRejectsGarbage(t *testing.T) {
	for _, raw := range []string{"", "no json here", "[]", "{}"} {
		if _, err := parseTranslationJSON(raw); err == nil && raw != "{}" {
			t.Fatalf("expected error for %q", raw)
		}
	}
	// "{}" decodes into an empty list — the batch-size check upstream
	// (translateBatch) rejects the count mismatch; here it just must not crash.
	tr, err := parseTranslationJSON("{}")
	if err != nil || len(tr.Translations) != 0 {
		t.Fatalf("empty object must decode to zero translations: %v %+v", err, tr)
	}
}

// TestTranslationSystemPromptContract pins the cost/safety contract of the
// translator: the answer key is never sent to the model (it is copied from
// the master row server-side), so the prompt must not even mention answers.
func TestTranslationSystemPromptContract(t *testing.T) {
	p := strings.ToLower(translationSystemPrompt)
	if strings.Contains(p, "correct") || strings.Contains(p, "answer") || strings.Contains(p, "ответ") {
		t.Fatal("translation prompt must not ask for or mention answer keys — they are copied server-side")
	}
	if !strings.Contains(translationSystemPrompt, "JSON") {
		t.Fatal("translation prompt must pin the JSON output format")
	}
}

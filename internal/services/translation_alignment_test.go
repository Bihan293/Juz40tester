package services

import (
	"testing"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// TestBuildTranslationsRejectsReorderedOptions: the correct letter is copied
// from the Russian master, so a translation that reorders the options
// (here: numeric answers sorted) would mark a wrong option as correct for
// Kazakh students. Such a reply must be rejected.
func TestBuildTranslationsRejectsReorderedOptions(t *testing.T) {
	q := models.Question{ID: 7, Text: "Сколько хромосом у человека?", OptionA: "48", OptionB: "46", OptionC: "23", OptionD: "44", CorrectAnswer: "B"}
	master := map[int]*models.Question{1: &q}

	good := `{"translations":[{"id":1,"question":"Адамда неше хромосома бар?","options":["48","46","23","44"],"topic":"Генетика"}]}`
	if _, err := buildTranslations(good, 1, master); err != nil {
		t.Fatalf("aligned translation rejected: %v", err)
	}
	sorted := `{"translations":[{"id":1,"question":"Адамда неше хромосома бар?","options":["23","44","46","48"],"topic":"Генетика"}]}`
	if _, err := buildTranslations(sorted, 1, master); err == nil {
		t.Fatal("reordered options must be rejected (the answer key would point to the wrong option)")
	}
	// Units / words around the numbers may change — only the digits matter.
	q2 := models.Question{ID: 8, Text: "Год?", OptionA: "1941 год", OptionB: "1945 год", OptionC: "1939 год", OptionD: "1914 год", CorrectAnswer: "A"}
	m2 := map[int]*models.Question{1: &q2}
	words := `{"translations":[{"id":1,"question":"Қай жыл?","options":["1941 жыл","1945 жыл","1939 жыл","1914 жыл"],"topic":"Тарих"}]}`
	if _, err := buildTranslations(words, 1, m2); err != nil {
		t.Fatalf("translated words around numbers rejected: %v", err)
	}
}

// TestBuildTranslationsRejectsCollapsedOptions: two different master options
// translated into the same text make the question ambiguous.
func TestBuildTranslationsRejectsCollapsedOptions(t *testing.T) {
	q := models.Question{ID: 9, Text: "Что верно?", OptionA: "Митоз", OptionB: "Мейоз", OptionC: "Амитоз", OptionD: "Эндомитоз", CorrectAnswer: "A"}
	master := map[int]*models.Question{1: &q}
	dup := `{"translations":[{"id":1,"question":"Қайсысы дұрыс?","options":["Митоз","Митоз","Амитоз","Эндомитоз"],"topic":"Биология"}]}`
	if _, err := buildTranslations(dup, 1, master); err == nil {
		t.Fatal("collapsed options must be rejected")
	}
	ok := `{"translations":[{"id":1,"question":"Қайсысы дұрыс?","options":["Митоз","Мейоз","Амитоз","Эндомитоз"],"topic":"Биология"}]}`
	if _, err := buildTranslations(ok, 1, master); err != nil {
		t.Fatalf("valid translation rejected: %v", err)
	}
}

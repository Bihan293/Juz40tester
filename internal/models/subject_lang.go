package models

import "strings"

// Content languages of LANGUAGE subjects (the subject itself teaches a
// language). Such tests are written in that language and must NEVER be
// machine-translated: a Russian spelling question or an English grammar
// question translated into Kazakh loses its meaning completely (the
// answer options are words/forms of the studied language).
const (
	ContentLangRU = "ru" // Русский язык / Русская литература
	ContentLangKK = "kk" // Казахский язык / Казахская литература
	ContentLangEN = "en" // Английский язык
	ContentLangDE = "de" // Немецкий язык
	ContentLangFR = "fr" // Французский язык
)

// languageSubjectMarkers maps lowercase name fragments to the language the
// subject teaches. Fragments are checked in order, the first match wins.
// Both Russian and Kazakh/English subject names are recognised.
var languageSubjectMarkers = []struct {
	fragment string
	lang     string
}{
	// Russian
	{"русский язык", ContentLangRU},
	{"русская литература", ContentLangRU},
	{"русский", ContentLangRU},
	{"орыс тілі", ContentLangRU},
	{"орыс әдебиеті", ContentLangRU},
	// Kazakh
	{"казахский язык", ContentLangKK},
	{"казахская литература", ContentLangKK},
	{"қазақ тілі", ContentLangKK},
	{"қазақ әдебиеті", ContentLangKK},
	{"казахский", ContentLangKK},
	// English
	{"английский", ContentLangEN},
	{"ағылшын", ContentLangEN},
	{"english", ContentLangEN},
	// German
	{"немецкий", ContentLangDE},
	{"неміс", ContentLangDE},
	{"german", ContentLangDE},
	// French
	{"французский", ContentLangFR},
	{"француз", ContentLangFR},
	{"french", ContentLangFR},
	// Generic
	{"иностранный язык", ContentLangEN},
	{"шет тілі", ContentLangEN},
	{"литература", ContentLangRU}, // plain «Литература» in a Russian-master bot
	{"әдебиет", ContentLangKK},
}

// SubjectContentLang returns the language a LANGUAGE subject is taught in
// ("ru", "kk", "en", ...) or "" for a regular subject (maths, biology,
// history, ...) whose content may be translated freely.
//
// NOTE: «Казахстан» (История Казахстана) must NOT match — only the
// explicit «казахский язык/литература» / «қазақ тілі» fragments do.
func SubjectContentLang(subjectName string) string {
	n := strings.ToLower(strings.Join(strings.Fields(subjectName), " "))
	if n == "" {
		return ""
	}
	for _, m := range languageSubjectMarkers {
		if strings.Contains(n, m.fragment) {
			return m.lang
		}
	}
	return ""
}

// IsLanguageSubject reports whether the subject teaches a language — its
// tests are shown in the original language whatever the user's setting.
func IsLanguageSubject(subjectName string) bool {
	return SubjectContentLang(subjectName) != ""
}

package models

import "testing"

func TestSubjectContentLang(t *testing.T) {
	cases := map[string]string{
		"Русский язык":                ContentLangRU,
		"русский язык и литература":   ContentLangRU,
		"  Русская   литература ":     ContentLangRU,
		"Английский язык":             ContentLangEN,
		"Английский":                  ContentLangEN,
		"English":                     ContentLangEN,
		"Казахский язык":              ContentLangKK,
		"Казахский язык и литература": ContentLangKK,
		"Қазақ тілі":                  ContentLangKK,
		"Немецкий язык":               ContentLangDE,
		"Французский язык":            ContentLangFR,
		"Иностранный язык":            ContentLangEN,
		// Regular subjects — translatable.
		"История Казахстана":         "",
		"Всемирная история":          "",
		"Математика":                 "",
		"Математическая грамотность": "",
		"Грамотность чтения":         "",
		"Биология":                   "",
		"Физика":                     "",
		"География":                  "",
		"Информатика":                "",
		"":                           "",
	}
	for name, want := range cases {
		if got := SubjectContentLang(name); got != want {
			t.Errorf("SubjectContentLang(%q) = %q, want %q", name, got, want)
		}
		if IsLanguageSubject(name) != (want != "") {
			t.Errorf("IsLanguageSubject(%q) mismatch", name)
		}
	}
}

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

func TestMeetsUnlockBar(t *testing.T) {
	cases := []struct {
		green, yellow int
		want          bool
	}{
		{15, 5, true},
		{16, 4, true}, // better than the bar — used to stay LOCKED
		{18, 2, true},
		{20, 0, true}, // everything mastered — used to stay LOCKED
		{15, 4, false},
		{14, 6, false},
		{10, 10, false},
		{0, 0, false},
	}
	for _, c := range cases {
		if got := MeetsUnlockBar(c.green, c.yellow); got != c.want {
			t.Fatalf("MeetsUnlockBar(%d, %d) = %v, want %v", c.green, c.yellow, got, c.want)
		}
	}
}

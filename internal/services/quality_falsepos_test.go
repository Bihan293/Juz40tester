package services

import "testing"

// Final check of quality.go detectors: every false positive here would
// trigger a paid AI repair of a perfectly good question; every true
// positive must still be caught.
func TestQualityDetectorsEdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		stem    string
		options []string
		correct int
		hard    bool
	}{
		// --- must NOT be flagged ---
		{"answer-unit instruction", "Найдите скорость тела (ответ дайте в м/с).", []string{"5", "10", "15", "20"}, 0, false},
		{"choose-the-answer instruction", "Выберите правильный ответ: столица Казахстана", []string{"Алматы", "Астана", "Шымкент", "Тараз"}, 1, false},
		{"spelling: hyphen/space/solid", "Как правильно пишется наречие?", []string{"по-моему", "по моему", "помоему", "помоемуто"}, 0, false},
		{"code identifiers", "Какое имя переменной допустимо в Python?", []string{"my_var", "2var", "var-1", "for"}, 0, false},
		{"real gaps in every option", "Укажите слово с пропущенной буквой О", []string{"р_сти", "м_локо", "г_ра", "д_м"}, 0, false},
		{"plain question with punctuation", "Сколько будет 2 + 2, если считать в десятичной системе?", []string{"3", "4", "5", "6"}, 1, false},
		{"sentence ellipsis is not a gap", "Продолжите: «Он сказал…»", []string{"что придёт", "что ушёл", "что спит", "что ест"}, 0, false},
		{"command is not comma", "Which command lists files in Linux?", []string{"ls", "cd", "pwd", "rm"}, 0, false},
		// --- MUST be flagged (go to repair) ---
		{"answer appended", "Сколько будет 2+2? Ответ: B", []string{"3", "4", "5", "6"}, 1, true},
		{"answer in parens", "Сколько будет 2+2? (ответ: 4)", []string{"3", "4", "5", "6"}, 1, true},
		{"hint in stem", "Подсказка: вспомните таблицу умножения. 7·8 = ?", []string{"54", "56", "58", "64"}, 1, true},
		{"marked option", "Столица Франции?", []string{"Париж (правильный)", "Лион", "Ницца", "Марсель"}, 0, true},
		{"catch-all option", "Что относится к млекопитающим?", []string{"Кит", "Акула", "Окунь", "Все ответы верны"}, 0, true},
		{"only key filled in", "Вставьте пропущенную букву: р..сти", []string{"р..сти", "р..сли", "расти", "р..стёт"}, 2, true},
		{"only key has the comma", "Где нужно поставить запятую?", []string{"Я пришёл и он ушёл", "Я пришёл, а он ушёл", "Я пришёл и ушёл", "Он пришёл и ушёл"}, 1, true},
		{"punct-only difference outside punctuation/spelling", "Столица Казахстана?", []string{"Нур-Султан", "Нур Султан", "Алматы", "Тараз"}, 0, true},
	}
	for _, c := range cases {
		rep := auditQuestion(c.stem, c.options, c.correct)
		if rep.HasHard() != c.hard {
			t.Errorf("%s: hard=%v, want %v (%s)", c.name, rep.HasHard(), c.hard, rep.Reasons())
		}
	}
}

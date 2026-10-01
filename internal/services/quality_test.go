package services

import (
	"strings"
	"testing"
)

type auditCase struct {
	name     string
	text     string
	options  []string
	correct  int
	wantHard bool
}

func TestAuditQuestion(t *testing.T) {
	cases := []auditCase{
		// --- The reported bug -------------------------------------------
		{
			name:     "dash only in the key (reported bug)",
			text:     "В каком предложении нужно поставить тире?",
			options:  []string{"Наступила зима.", "Книга лежит на столе.", "Я люблю русский язык.", "Москва — столица России."},
			correct:  3,
			wantHard: true,
		},
		{
			name:     "dash with gaps everywhere is fine",
			text:     "В каком предложении на месте пропуска нужно поставить тире?",
			options:  []string{"Москва _ столица России.", "Зимой _ здесь очень холодно.", "Книга _ лежит на столе.", "Он _ мой старый друг."},
			correct:  0,
			wantHard: false,
		},
		{
			name:     "dash shown in every option (where is it correct) is fine",
			text:     "В каком предложении тире поставлено верно?",
			options:  []string{"Москва — столица России.", "Книга — лежит на столе.", "Он — пришёл домой.", "Зимой — холодно и снежно."},
			correct:  0,
			wantHard: false,
		},
		{
			name:     "dash only missing in the key",
			text:     "В каком предложении НЕ нужно тире?",
			options:  []string{"Москва — столица России.", "Брат — мой лучший друг.", "Книга лежит на столе.", "Учиться — всегда пригодится."},
			correct:  2,
			wantHard: true,
		},
		{
			name:     "dash printed in two options of a where-needed question",
			text:     "В каком предложении нужно поставить тире?",
			options:  []string{"Москва — столица России.", "Книга — лежит на столе.", "Наступила зима.", "Я люблю читать."},
			correct:  0,
			wantHard: true,
		},
		{
			name:     "comma only in the key",
			text:     "В каком предложении нужна запятая?",
			options:  []string{"Я пришёл домой и лёг спать.", "Мы гуляли в парке.", "Когда стемнело, мы ушли.", "Он читал книгу у окна."},
			correct:  2,
			wantHard: true,
		},
		{
			name:     "comma placed in every option is fine",
			text:     "Укажите предложение, в котором запятая поставлена правильно.",
			options:  []string{"Когда стемнело, мы ушли домой.", "Мы гуляли, в парке до вечера.", "Он читал, книгу у окна.", "Я пришёл, домой поздно."},
			correct:  0,
			wantHard: false,
		},
		{
			name:     "colon only in the key",
			text:     "В каком предложении нужно двоеточие?",
			options:  []string{"На столе лежали: книги, тетради, ручки.", "Мы пошли в лес за грибами.", "Солнце светило ярко.", "Дети играли во дворе."},
			correct:  0,
			wantHard: true,
		},
		{
			name:     "hyphen only in the key",
			text:     "Какое слово пишется через дефис?",
			options:  []string{"кое-как", "внутри", "сверху", "издалека"},
			correct:  0,
			wantHard: true,
		},
		{
			name:     "hyphen with gaps everywhere is fine",
			text:     "Какое слово пишется через дефис? (на месте пропуска)",
			options:  []string{"кое_как", "в_нутри", "с_верху", "из_далека"},
			correct:  0,
			wantHard: false,
		},
		// --- Gap tells (orthography) ------------------------------------
		{
			name:     "key is the only filled-in word",
			text:     "В каком слове пишется буква О?",
			options:  []string{"р..сти", "к..саться", "гореть", "з..ря"},
			correct:  2,
			wantHard: true,
		},
		{
			name:     "gaps in every option are fine",
			text:     "В каком слове на месте пропуска пишется буква О?",
			options:  []string{"р..сти", "к..саться", "г..реть", "з..ря"},
			correct:  2,
			wantHard: false,
		},
		// --- Duplicates & catch-all -------------------------------------
		{
			name:     "duplicate options (case/period)",
			text:     "Столица Казахстана — это какой город?",
			options:  []string{"Астана", "Алматы", "астана.", "Шымкент"},
			correct:  0,
			wantHard: true,
		},
		{
			name:     "all of the above",
			text:     "Which of these are mammals?",
			options:  []string{"Dolphin", "Whale", "Bat", "All of the above"},
			correct:  3,
			wantHard: true,
		},
		{
			name:     "нет правильного ответа",
			text:     "Чему равен корень из 16?",
			options:  []string{"2", "4", "8", "Нет правильного ответа"},
			correct:  1,
			wantHard: true,
		},
		{
			name:     "explicit marker on the key",
			text:     "Какой органоид отвечает за синтез АТФ?",
			options:  []string{"Рибосома", "Митохондрия (верно)", "Лизосома", "Аппарат Гольджи"},
			correct:  1,
			wantHard: true,
		},
		{
			name:     "words that differ only by punctuation outside punctuation topic",
			text:     "Как называется процесс деления клетки?",
			options:  []string{"митоз", "ми-тоз", "мейоз", "амитоз"},
			correct:  0,
			wantHard: true,
		},
		// --- Clean questions of other subjects ---------------------------
		{
			name:     "maths numbers incl. negative are fine",
			text:     "Решите уравнение x + 5 = 3.",
			options:  []string{"-2", "2", "8", "-8"},
			correct:  0,
			wantHard: false,
		},
		{
			name:     "biology clean",
			text:     "Какой органоид клетки отвечает за синтез АТФ?",
			options:  []string{"Рибосома", "Митохондрия", "Лизосома", "Аппарат Гольджи"},
			correct:  1,
			wantHard: false,
		},
		{
			name:     "history clean",
			text:     "В каком году Казахстан провозгласил независимость?",
			options:  []string{"1990", "1991", "1993", "1995"},
			correct:  1,
			wantHard: false,
		},
		{
			name:     "english clean",
			text:     "Choose the correct form: She ___ to school every day.",
			options:  []string{"go", "goes", "going", "gone"},
			correct:  1,
			wantHard: false,
		},
		{
			name:     "english apostrophe only in the key",
			text:     "Which sentence needs an apostrophe?",
			options:  []string{"The cats are sleeping now.", "It's raining again today.", "They play football daily.", "We read books together."},
			correct:  1,
			wantHard: true,
		},
		{
			name:     "physics clean with units",
			text:     "Чему равна единица силы в СИ?",
			options:  []string{"Ньютон", "Джоуль", "Ватт", "Паскаль"},
			correct:  0,
			wantHard: false,
		},
		{
			name:     "chemistry formulas are fine",
			text:     "Какова формула серной кислоты?",
			options:  []string{"H2SO4", "H2SO3", "HCl", "HNO3"},
			correct:  0,
			wantHard: false,
		},
		{
			name:     "key the only sentence with a final period",
			text:     "Какое утверждение о фотосинтезе верно?",
			options:  []string{"Происходит в хлоропластах растений.", "Происходит в митохондриях животных", "Идёт только ночью в корнях", "Не требует света вообще никогда"},
			correct:  0,
			wantHard: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := auditQuestion(c.text, c.options, c.correct)
			if rep.HasHard() != c.wantHard {
				t.Fatalf("hard=%v, want %v; issues: %s", rep.HasHard(), c.wantHard, rep.Reasons())
			}
		})
	}
}

func TestAuditSoftLengthTell(t *testing.T) {
	rep := auditQuestion("Что такое фотосинтез?", []string{
		"Дыхание",
		"Процесс образования органических веществ из углекислого газа и воды на свету",
		"Деление",
		"Брожение",
	}, 1)
	if rep.HasHard() || rep.Empty() {
		t.Fatalf("expected a soft-only length warning, got %+v", rep.Issues)
	}
}

func TestNormalizeOptionFormat(t *testing.T) {
	opts := []string{"Происходит в хлоропластах растений.", "Происходит в митохондриях животных", "Идёт только ночью в корнях", "Не требует света вообще"}
	normalizeOptionFormat("Какое утверждение о фотосинтезе верно?", opts)
	for _, o := range opts {
		if strings.HasSuffix(o, ".") {
			t.Fatalf("trailing period not normalised: %q", o)
		}
	}
	if rep := auditQuestion("Какое утверждение о фотосинтезе верно?", opts, 0); rep.HasHard() {
		t.Fatalf("normalised options still flagged: %s", rep.Reasons())
	}
	// Punctuation questions are never normalised.
	p := []string{"Наступила зима.", "Книга лежит на столе", "Я люблю язык", "Москва — столица России"}
	normalizeOptionFormat("В каком предложении нужно поставить тире?", p)
	if p[0] != "Наступила зима." {
		t.Fatal("punctuation question options must stay untouched")
	}
}

func TestCheckRewriteKeepsTopicAndRejectsBad(t *testing.T) {
	orig := generatedQuestion{Text: "x", Topic: "Тире", Difficulty: 2}
	good := generatedQuestion{
		Text:    "В каком предложении на месте пропуска нужно поставить тире?",
		Options: []string{"Москва _ столица России.", "Зимой _ здесь очень холодно.", "Книга _ лежит на столе.", "Он _ мой старый друг."},
		Correct: 0, Topic: "Другая тема", Difficulty: 9,
	}
	if err := checkRewrite(orig, &good); err != nil {
		t.Fatalf("good rewrite rejected: %v", err)
	}
	if good.Topic != "Тире" || good.Difficulty != 2 {
		t.Fatalf("topic/difficulty must be pinned to the original: %+v", good)
	}
	bad := generatedQuestion{
		Text:    "В каком предложении нужно поставить тире?",
		Options: []string{"Наступила зима.", "Книга лежит на столе.", "Я люблю русский язык.", "Москва — столица России."},
		Correct: 3,
	}
	if err := checkRewrite(orig, &bad); err == nil {
		t.Fatal("bad rewrite accepted")
	}
}

func TestParseRejectsMassivelyFlaggedReplyCount(t *testing.T) {
	gt := &generatedTest{Questions: validQuestions()}
	for i := 0; i < maxHardFlaggedPerReply+1; i++ {
		gt.Questions[i].Options[1] = gt.Questions[i].Options[0]
	}
	if n := countHard(auditGenerated(gt)); n != maxHardFlaggedPerReply+1 {
		t.Fatalf("expected %d flagged, got %d", maxHardFlaggedPerReply+1, n)
	}
	if n := countHard(auditGenerated(&generatedTest{Questions: validQuestions()})); n != 0 {
		t.Fatalf("clean fixture must have no hard issues, got %d", n)
	}
}

func TestLetterIndex(t *testing.T) {
	for i, l := range []string{"A", "b", " C ", "D"} {
		if letterIndex(l) != i {
			t.Fatalf("letterIndex(%q) != %d", l, i)
		}
	}
	if letterIndex("E") != -1 {
		t.Fatal("bad letter must be -1")
	}
}

func TestKeywordBoundaries(t *testing.T) {
	// No false positives from substrings in non-punctuation questions.
	for _, stem := range []string{
		"which command starts the colonial period?",
		"какой антирекорд установлен в 1990 году?",
	} {
		if isPunctuationQuestion(stem) {
			t.Fatalf("%q wrongly treated as a punctuation question", stem)
		}
	}
	for _, stem := range []string{"где нужна запятая?", "which sentence needs a comma?", "в каком предложении тире?"} {
		if !isPunctuationQuestion(stem) {
			t.Fatalf("%q must be a punctuation question", stem)
		}
	}
}

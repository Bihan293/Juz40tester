// Package services — post-generation quality audit of test questions.
//
// The structural validator (validateTest) only checks the JSON contract:
// 20 questions, 4 options, an index in range. It cannot see the most
// annoying kind of a bad question — one whose answer is given away by the
// FORMAT of the options rather than by knowledge, e.g.
//
//	В каком предложении нужно поставить тире?
//	A) Наступила зима.   B) Книга лежит на столе.
//	C) Я люблю русский язык.   D) Москва — столица России.
//
// (the only option that already contains a dash is the key). auditQuestion
// is a language-agnostic heuristic linter run on every generated question
// of every subject BEFORE it is stored, and by a background sweep over the
// questions already in the database. Flagged questions are rewritten by the
// model (see GeneratorService.repairFlagged / RunQualitySweep).
//
// Issues are split by severity:
//   - hard: the question must never reach a student (duplicate options,
//     the key revealed by punctuation / a filled-in gap, explicit hint
//     markers, "все ответы верны"-style options, empty/degenerate options);
//   - soft: a classic test-writing smell (the key is much longer than every
//     distractor, the key is copied verbatim from the stem). Soft issues are
//     repaired when possible but never block a test on their own — the
//     heuristics are not perfect and a false positive must not leave a
//     student without a test.
package services

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// questionIssue is one finding of the audit.
type questionIssue struct {
	Hard   bool
	Reason string // human-readable (Russian) — also sent to the model on repair
}

// qualityReport is the audit result of one question.
type qualityReport struct {
	Issues []questionIssue
}

func (r qualityReport) HasHard() bool {
	for _, is := range r.Issues {
		if is.Hard {
			return true
		}
	}
	return false
}

func (r qualityReport) Empty() bool { return len(r.Issues) == 0 }

// Reasons joins the issue texts (for logs and repair prompts).
func (r qualityReport) Reasons() string {
	parts := make([]string, 0, len(r.Issues))
	for _, is := range r.Issues {
		parts = append(parts, is.Reason)
	}
	return strings.Join(parts, "; ")
}

func (r *qualityReport) hard(format string, a ...any) {
	r.Issues = append(r.Issues, questionIssue{Hard: true, Reason: fmt.Sprintf(format, a...)})
}

func (r *qualityReport) soft(format string, a ...any) {
	r.Issues = append(r.Issues, questionIssue{Hard: false, Reason: fmt.Sprintf(format, a...)})
}

// ---------------------------------------------------------------------------
// Punctuation marks a question may be "about"
// ---------------------------------------------------------------------------

type punctMark struct {
	name     string         // for messages
	keywords []string       // lower-case stem fragments in RU / KK / EN
	present  *regexp.Regexp // how the mark looks inside an option
}

var (
	reDash       = regexp.MustCompile(`[—–]|\s-\s|\s-$|^-\s`)
	reHyphen     = regexp.MustCompile(`\p{L}-\p{L}`)
	reComma      = regexp.MustCompile(`,`)
	reColon      = regexp.MustCompile(`:`)
	reSemicolon  = regexp.MustCompile(`;`)
	reQuotes     = regexp.MustCompile(`[«»"“”„]`)
	reApostrophe = regexp.MustCompile(`\p{L}['’]|['’]\p{L}`)
	reExclaim    = regexp.MustCompile(`!`)
	reQuestion   = regexp.MustCompile(`\?`)
	reSoftSign   = regexp.MustCompile(`[ьЬ]`)
	reHardSign   = regexp.MustCompile(`[ъЪ]`)
)

// Order matters: «точка с запятой» must be recognised (and cut out of the
// stem) before the plain «запятая» check, otherwise every semicolon question
// would also be treated as a comma question.
var punctMarks = []punctMark{
	{"точка с запятой", []string{"точка с запятой", "точку с запятой", "точки с запятой", "нүктелі үтір", "semicolon", "semicolons"}, reSemicolon},
	{"тире", []string{"тире", "сызықша", "dash", "dashes"}, reDash},
	{"дефис", []string{"дефис", "hyphen", "hyphens", "hyphenated", "дефиспен"}, reHyphen},
	{"запятая", []string{"запят", "үтір", "comma", "commas"}, reComma},
	{"двоеточие", []string{"двоеточ", "қос нүкте", "colon", "colons"}, reColon},
	{"кавычки", []string{"кавычк", "тырнақша", "quotation mark", "quotation marks", "quotes"}, reQuotes},
	{"апостроф", []string{"апостроф", "apostrophe", "apostrophes"}, reApostrophe},
	{"восклицательный знак", []string{"восклицательн", "леп белгісі", "exclamation"}, reExclaim},
	{"вопросительный знак", []string{"вопросительный знак", "сұрақ белгісі", "question mark"}, reQuestion},
	{"разделительный Ь", []string{"разделительный ь", "мягкий знак", "мягким знаком"}, reSoftSign},
	{"разделительный Ъ", []string{"разделительный ъ", "твёрдый знак", "твердый знак", "твёрдым знаком", "твердым знаком"}, reHardSign},
}

// genericPunctKeywords mark a question about punctuation in general
// («знаки препинания расставлены…», «пунктуационная ошибка»).
var genericPunctKeywords = []string{"знак препинания", "знаки препинания", "знаков препинания", "пунктуац", "тыныс белгі", "punctuation", "punctuated"}

// insertKeywords mark a question that asks WHERE a mark is needed: the
// student is supposed to insert it, so the options must not already show it
// (or must all show it in the same form).
var insertKeywords = []string{
	"нужно постав", "надо постав", "следует постав", "необходимо постав", "нужно ставить",
	"ставится", "ставят", "нужна запятая", "нужно тире", "нужен", "нужна", "нужны", "требует",
	"пропущен", "на месте пропуск", "на месте пробел",
	"қою керек", "қойылады", "қажет",
	"needed", "is needed", "needs", "should be", "missing", "required", "be inserted",
}

// gap markers inside a word: «деревя..ый», «р_сти», «пр…красный».
// A trailing «…» of a sentence («Он сказал…») is NOT a gap.
var reGap = regexp.MustCompile(`_+|\p{L}(\.\.+|…)\p{L}|(^|\s)(\.\.+|…)\p{L}|\(\s*\)|\[\s*\]`)

// options that make the key ambiguous or guessable by test-taking tricks.
var catchAllOptions = []string{
	"все ответы верны", "все варианты верны", "все перечисленное", "все перечисленные", "всё перечисленное",
	"все вышеперечисленн", "всё вышеперечисленн", "нет правильного ответа", "нет верного ответа", "все ответы неверны",
	"оба варианта", "ни один из вариантов",
	"барлық жауап дұрыс", "дұрыс жауап жоқ",
	"all of the above", "none of the above", "both a and b", "all the above", "all answers are correct",
}

// explicit giveaway markers sometimes leaked by the model.
var hintMarkers = []string{
	"(правильн", "(верн", "(correct", "(right", "(дұрыс", "✓", "✔", "✅", "☑", "*правильн", "правильный ответ:", "correct answer:",
}

// ---------------------------------------------------------------------------
// Normalisation helpers
// ---------------------------------------------------------------------------

// normOption collapses case and whitespace and drops trailing terminal
// punctuation — «Москва.» and «москва» are the same option.
func normOption(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = strings.ReplaceAll(s, "ё", "е")
	return strings.TrimRight(s, ".!?;… ")
}

// normLetters keeps only letters and digits — used to detect options that
// differ ONLY by punctuation.
func normLetters(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if r == 'ё' {
				r = 'е'
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// hasKeyword reports whether kw occurs in s starting at a word boundary
// («тире» must not match «антирекорд»). Latin keywords must also END at a
// word boundary («comma» ≠ «command», «colon» ≠ «colonial»); Cyrillic ones
// are stems («запят» → «запятая», «запятую») and may continue.
func hasKeyword(s, kw string) bool {
	latin := true
	for _, r := range kw {
		if r > unicode.MaxASCII {
			latin = false
			break
		}
	}
	for from := 0; from < len(s); {
		i := strings.Index(s[from:], kw)
		if i < 0 {
			return false
		}
		i += from
		okStart := true
		if i > 0 {
			r, _ := utf8.DecodeLastRuneInString(s[:i])
			okStart = !unicode.IsLetter(r)
		}
		okEnd := true
		if latin && i+len(kw) < len(s) {
			r, _ := utf8.DecodeRuneInString(s[i+len(kw):])
			okEnd = !unicode.IsLetter(r)
		}
		if okStart && okEnd {
			return true
		}
		from = i + len(kw)
	}
	return false
}

func hasAnyKeyword(s string, kws []string) bool {
	for _, kw := range kws {
		if hasKeyword(s, kw) {
			return true
		}
	}
	return false
}

func letterCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}

func wordCount(s string) int { return len(strings.Fields(s)) }

// oddOneOut returns the index of the single option whose flag differs from
// the other three (exactly 1 or exactly 3 options have the flag), or -1.
func oddOneOut(flags []bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	if len(flags) != 4 || (n != 1 && n != 3) {
		return -1
	}
	want := n == 1 // the odd one has the flag when only one has it
	for i, f := range flags {
		if f == want {
			return i
		}
	}
	return -1
}

func countTrue(flags []bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

var optionLetters = []string{"A", "B", "C", "D"}

// ---------------------------------------------------------------------------
// The audit
// ---------------------------------------------------------------------------

// auditQuestion lints one question. It never mutates its input.
func auditQuestion(text string, options []string, correct int) qualityReport {
	var rep qualityReport
	if len(options) != 4 || correct < 0 || correct > 3 {
		rep.hard("нарушен формат: нужно 4 варианта и correct_index 0–3")
		return rep
	}
	stem := strings.ToLower(strings.Join(strings.Fields(text), " "))

	// 1. Empty / degenerate / duplicate options.
	seen := map[string]int{}
	seenLetters := map[string]int{}
	for i, o := range options {
		o = strings.TrimSpace(o)
		if o == "" {
			rep.hard("вариант %s пустой", optionLetters[i])
			continue
		}
		n := normOption(o)
		if j, ok := seen[n]; ok {
			rep.hard("варианты %s и %s совпадают", optionLetters[j], optionLetters[i])
		} else {
			seen[n] = i
		}
		// Options made of words only (no numbers/formulas: «-2» vs «2» is a
		// legitimate maths distractor).
		if l := normLetters(o); l != "" && letterCount(o) >= 3 && !strings.ContainsAny(o, "0123456789") {
			if j, ok := seenLetters[l]; ok && seen[n] == i {
				// Same words, different punctuation: legitimate ONLY for
				// punctuation questions (the marks ARE the answer there).
				if !isPunctuationQuestion(stem) && !isSpellingQuestion(stem) {
					rep.hard("варианты %s и %s различаются только знаками препинания", optionLetters[j], optionLetters[i])
				}
			} else if !ok {
				seenLetters[l] = i
			}
		}
	}

	// 2. Explicit hint markers and catch-all options.
	for i, o := range options {
		lo := strings.ToLower(o)
		if containsAny(lo, hintMarkers) {
			rep.hard("вариант %s содержит явную пометку-подсказку", optionLetters[i])
		}
		if containsAny(strings.ReplaceAll(lo, "ё", "е"), catchAllOptions) || containsAny(lo, catchAllOptions) {
			rep.hard("вариант %s типа «все/ни один из перечисленных» — недопустим", optionLetters[i])
		}
	}
	if stemLeaksAnswer(stem) {
		rep.hard("в тексте вопроса есть подсказка/ответ")
	}

	// 3. Punctuation tells (the core bug).
	auditPunctuation(&rep, stem, options, correct)

	// 4. Gap tells: «деревя..ый» in three options, the key written out in full.
	// Code questions are skipped: «my_var» is an identifier, not a gap.
	gaps := make([]bool, 4)
	if !isCodeQuestion(stem) {
		for i, o := range options {
			gaps[i] = reGap.MatchString(o)
		}
	}
	if k := oddOneOut(gaps); k >= 0 {
		if k == correct {
			rep.hard("правильный вариант выдаёт себя оформлением пропуска (только он отличается заполненностью/пропусками)")
		} else {
			rep.hard("варианты оформлены неоднородно: пропуски («..», «_») есть не во всех вариантах")
		}
	}

	// 5. Sentence options: the key is the only one with/without a final mark.
	sentences := true
	for _, o := range options {
		if wordCount(o) < 3 {
			sentences = false
			break
		}
	}
	if sentences {
		term := make([]bool, 4)
		for i, o := range options {
			t := strings.TrimSpace(o)
			term[i] = strings.HasSuffix(t, ".") || strings.HasSuffix(t, "!") || strings.HasSuffix(t, "?") || strings.HasSuffix(t, "…")
		}
		if k := oddOneOut(term); k == correct {
			rep.hard("правильный вариант единственный отличается конечным знаком предложения")
		}
	}

	// 6. Length tell (soft): the key is far longer/shorter than every distractor.
	lens := make([]int, 4)
	for i, o := range options {
		lens[i] = utf8.RuneCountInString(strings.TrimSpace(o))
	}
	maxOther, minOther := 0, 1<<30
	for i, l := range lens {
		if i == correct {
			continue
		}
		if l > maxOther {
			maxOther = l
		}
		if l < minOther {
			minOther = l
		}
	}
	if c := lens[correct]; maxOther > 0 && c >= 20 && float64(c) >= 1.8*float64(maxOther) && c-maxOther >= 15 {
		rep.soft("правильный вариант заметно длиннее всех дистракторов")
	} else if minOther >= 20 && c > 0 && float64(c) <= 0.4*float64(minOther) {
		rep.soft("правильный вариант заметно короче всех дистракторов")
	}

	// 7. The key copied verbatim from the stem while no distractor is (soft).
	if key := normOption(options[correct]); letterCount(key) >= 4 && utf8.RuneCountInString(key) >= 5 {
		inStem := func(o string) bool {
			n := normOption(o)
			return letterCount(n) >= 4 && strings.Contains(strings.ReplaceAll(stem, "ё", "е"), n)
		}
		if inStem(options[correct]) {
			others := 0
			for i, o := range options {
				if i != correct && inStem(o) {
					others++
				}
			}
			if others == 0 {
				rep.soft("правильный ответ дословно повторяет текст вопроса")
			}
		}
	}
	return rep
}

// reAnswerLeak: «… ? Ответ: B», «(ответ: 4)», «Ответ = 12» — a short answer
// appended after the question itself ended (the part before «ответ» holds
// a «?» or «.»). Instructions like «(ответ дайте в м/с)» or «Выберите
// правильный ответ: столица Казахстана» are NOT leaks.
var reAnswerLeak = regexp.MustCompile(`[?.!)]\s*\(?\s*(правильный\s+|верный\s+|correct\s+)?(ответ|answer|жауап)\s*[:=]\s*\S+(\s+\S+){0,2}\s*\)?\.?\s*$`)

// stemLeaksAnswer reports an explicit hint/answer inside the question text.
func stemLeaksAnswer(stem string) bool {
	if hasKeyword(stem, "подсказка") || hasKeyword(stem, "hint:") || hasKeyword(stem, "кеңес:") {
		return true
	}
	return reAnswerLeak.MatchString(stem)
}

// spellingKeywords mark spelling questions (слитно/раздельно/через дефис):
// their options legitimately differ only by a hyphen or a space.
var spellingKeywords = []string{"слитно", "раздельно", "через дефис", "пишется", "пишутся", "написан", "правописан", "орфограф", "бірге жазыл", "бөлек жазыл", "жазылады", "spelled", "spelling", "spelt"}

func isSpellingQuestion(stem string) bool { return hasAnyKeyword(stem, spellingKeywords) }

// codeKeywords mark programming questions where «_» is part of identifiers.
var codeKeywords = []string{"python", "pascal", "java", "c++", "программ", "переменн", "идентификатор", "код ", "кода", "функци", "бағдарлама", "айнымалы", "variable", "identifier", "code"}

func isCodeQuestion(stem string) bool { return hasAnyKeyword(stem, codeKeywords) }

// isPunctuationQuestion reports whether the stem is about punctuation or a
// specific orthographic mark.
func isPunctuationQuestion(stem string) bool {
	if hasAnyKeyword(stem, genericPunctKeywords) {
		return true
	}
	for _, m := range punctMarks {
		if hasAnyKeyword(stem, m.keywords) {
			return true
		}
	}
	return false
}

// auditPunctuation finds options that differ from the others only by the
// presence of the very mark the question asks about.
func auditPunctuation(rep *qualityReport, stem string, options []string, correct int) {
	rest := stem
	var targets []punctMark
	for _, m := range punctMarks {
		hit := false
		for _, kw := range m.keywords {
			if hasKeyword(rest, kw) {
				hit = true
				rest = strings.ReplaceAll(rest, kw, " ")
			}
		}
		if hit {
			targets = append(targets, m)
		}
	}
	generic := hasAnyKeyword(stem, genericPunctKeywords)
	if len(targets) == 0 && !generic {
		return
	}
	insert := containsAny(stem, insertKeywords)

	for _, m := range targets {
		has := make([]bool, 4)
		for i, o := range options {
			has[i] = m.present.MatchString(o)
		}
		n := countTrue(has)
		if k := oddOneOut(has); k >= 0 {
			if k == correct {
				rep.hard("правильный ответ определяется визуально: только он отличается наличием знака «%s»", m.name)
			} else {
				rep.hard("знак «%s» показан не во всех вариантах одинаково (вариант %s выделяется)", m.name, optionLetters[k])
			}
			continue
		}
		if insert && n == 2 {
			// «Где нужно поставить X?» — X already printed in two options
			// narrows the choice to a coin toss without any knowledge.
			rep.hard("вопрос «где нужен знак «%s»», но знак уже напечатан в части вариантов — варианты нужно дать единообразно (без знака)", m.name)
		}
	}

	if generic || len(targets) > 0 {
		// Any punctuation mark that ONLY the key has (or only the key lacks)
		// while the other three agree is a visual giveaway too.
		for _, m := range punctMarks {
			already := false
			for _, t := range targets {
				if t.name == m.name {
					already = true
				}
			}
			if already || m.present == reSoftSign || m.present == reHardSign {
				continue
			}
			has := make([]bool, 4)
			for i, o := range options {
				has[i] = m.present.MatchString(o)
			}
			if k := oddOneOut(has); k == correct {
				rep.hard("правильный вариант единственный отличается знаком «%s» — ответ виден без знания правила", m.name)
			}
		}
	}
}

// auditGenerated audits every question of a generated test and returns the
// reports keyed by question index (only questions with issues).
func auditGenerated(gt *generatedTest) map[int]qualityReport {
	out := map[int]qualityReport{}
	for i := range gt.Questions {
		q := &gt.Questions[i]
		if rep := auditQuestion(q.Text, q.Options, q.Correct); !rep.Empty() {
			out[i] = rep
		}
	}
	return out
}

func countHard(reps map[int]qualityReport) int {
	n := 0
	for _, r := range reps {
		if r.HasHard() {
			n++
		}
	}
	return n
}

// normalizeOptionFormat removes harmless formatting differences that would
// otherwise single out one option: if only some options end with a period,
// the trailing periods are dropped from all of them. Punctuation questions
// are left untouched — there the marks ARE the subject of the question.
func normalizeOptionFormat(text string, options []string) {
	if len(options) != 4 || isPunctuationQuestion(strings.ToLower(text)) {
		return
	}
	withDot := 0
	for _, o := range options {
		t := strings.TrimSpace(o)
		if strings.HasSuffix(t, ".") && !strings.HasSuffix(t, "..") {
			withDot++
		}
	}
	if withDot == 0 || withDot == 4 {
		return
	}
	for i, o := range options {
		t := strings.TrimSpace(o)
		if strings.HasSuffix(t, ".") && !strings.HasSuffix(t, "..") {
			options[i] = strings.TrimSpace(strings.TrimSuffix(t, "."))
		}
	}
}

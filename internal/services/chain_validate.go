package services

// Hard validation of generated chain/personal tests beyond the JSON
// contract (audit items 1–3):
//
//   - no question may repeat a question of the PREVIOUS chain test (exact
//     or near-identical stem), and no two questions of one test may be
//     near-duplicates;
//   - the weak topics the model was told to train must actually be covered;
//   - the difficulty must match the chain position (tighter tolerance than
//     before, an outlier cap, and no regression below the previous test).
//
// Every rejection is a typed *genRejectError, so runSteps can count it per
// class (metrics: difficulty violations, repeat rejects, weak coverage …)
// and the next provider step gets the reason as feedback.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// Reject classes (metric label values).
const (
	rejectFormat     = "format"
	rejectDifficulty = "difficulty"
	rejectRepeat     = "repeat"
	rejectWeak       = "weak_coverage"
	rejectTopics     = "topics"
	rejectQuality    = "quality"
)

// genRejectError is a validation failure with a class for metrics/feedback.
type genRejectError struct {
	class string
	msg   string
}

func (e *genRejectError) Error() string { return e.msg }

func rejectf(class, format string, a ...any) error {
	return &genRejectError{class: class, msg: fmt.Sprintf(format, a...)}
}

// rejectClass returns the class of a validation error ("" = unclassified).
func rejectClass(err error) string {
	var re *genRejectError
	if errors.As(err, &re) {
		return re.class
	}
	return ""
}

// ---------------------------------------------------------------------------
// Repeats
// ---------------------------------------------------------------------------

// normStem canonicalises a question stem for equality: lower case, single
// spaces, no trailing punctuation. Digits are KEPT («2+3» and «2+4» are
// different questions).
func normStem(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	return strings.TrimRight(s, " ?.!:;…")
}

// stemWords returns the significant words (≥ 3 letters/digits) of a stem.
func stemWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(w)) >= 3 {
			out[w] = true
		}
	}
	return out
}

// nearDuplicateMinWords: the word-overlap check only applies to stems with
// at least this many significant words — short stems («Что такое ДНК?»)
// legitimately share most of their words.
const nearDuplicateMinWords = 5

// nearDuplicateJaccard: two stems sharing at least this share of their
// significant words (Jaccard) are the same question reworded cosmetically.
const nearDuplicateJaccard = 0.8

// similarStems reports whether two stems are the same question: equal after
// normalisation, or (for long enough stems) nearly the same word set.
func similarStems(a, b string) bool {
	if normStem(a) == normStem(b) {
		return true
	}
	wa, wb := stemWords(a), stemWords(b)
	if len(wa) < nearDuplicateMinWords || len(wb) < nearDuplicateMinWords {
		return false
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	union := len(wa) + len(wb) - inter
	return union > 0 && float64(inter)/float64(union) >= nearDuplicateJaccard
}

// repeatsAny returns the index of the first stem in others that the stem
// repeats (-1 = none).
func repeatsAny(stem string, others []string) int {
	for i, o := range others {
		if similarStems(stem, o) {
			return i
		}
	}
	return -1
}

// questionStems returns the stems of stored questions.
func questionStems(qs []models.Question) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.Text
	}
	return out
}

// validateNoRepeats rejects a reply that repeats a question of an earlier
// chain test (prevStems: the previous test and the older ones of the repeat
// window), or contains two near-identical questions.
func validateNoRepeats(gt *generatedTest, prevStems []string) error {
	for i, q := range gt.Questions {
		if j := repeatsAny(q.Text, prevStems); j >= 0 {
			stem := prevStems[j]
			if r := []rune(stem); len(r) > 80 {
				stem = string(r[:80]) + "…"
			}
			return rejectf(rejectRepeat, "question %d repeats a question of an earlier chain test («%s»)", i+1, stem)
		}
		for k := 0; k < i; k++ {
			if similarStems(q.Text, gt.Questions[k].Text) {
				return rejectf(rejectRepeat, "questions %d and %d are near-duplicates", k+1, i+1)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Weak topics of the chain
// ---------------------------------------------------------------------------

// chainWeakLimit: how many weak topics a chain test is told to train.
const chainWeakLimit = 5

// weakMarkBelow: a topic of the previous test whose AVERAGE mark (0..2 over
// every student and every question of the topic) is below this is weak.
const weakMarkBelow = 1.0

// topicMark is the average mark of one topic of the previous test.
type topicMark struct {
	Topic string
	Mark  float64
	N     int // questions of the topic
}

// prevTopicMarks aggregates the per-question marks of the previous test by
// topic (questions nobody answered — mark < 0 — are ignored). Sorted worst
// first.
func prevTopicMarks(prev []models.Question, marks []float64) []topicMark {
	type acc struct {
		title string
		sum   float64
		n     int
	}
	by := map[string]*acc{}
	var order []string
	for i, q := range prev {
		if i >= len(marks) || marks[i] < 0 || strings.TrimSpace(q.Topic) == "" {
			continue
		}
		k := models.NormalizeTopic(q.Topic)
		a := by[k]
		if a == nil {
			a = &acc{title: q.Topic}
			by[k] = a
			order = append(order, k)
		}
		a.sum += marks[i]
		a.n++
	}
	out := make([]topicMark, 0, len(order))
	for _, k := range order {
		a := by[k]
		out = append(out, topicMark{Topic: a.title, Mark: math.Round(a.sum/float64(a.n)*10) / 10, N: a.n})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mark < out[j].Mark })
	return out
}

// chainWeakTopics merges the weak topics of the previous test (average mark
// below weakMarkBelow) with the subject-wide weak topics (aggregated
// user_topic_stats of the active students), worst first, de-duplicated,
// capped at limit.
func chainWeakTopics(prevMarks []topicMark, subjectWeak []string, limit int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		k := models.NormalizeTopic(t)
		if k == "" || seen[k] || len(out) >= limit {
			return
		}
		seen[k] = true
		out = append(out, t)
	}
	for _, m := range prevMarks {
		if m.Mark < weakMarkBelow {
			add(m.Topic)
		}
	}
	for _, t := range subjectWeak {
		add(t)
	}
	return out
}

// weakCoverageNeed: how many DISTINCT weak topics a chain test must cover
// (all of them when there are fewer).
const weakCoverageNeed = 3

// topicMatches reports whether the question topic is the wanted topic
// (normalised spelling or the same catalog topic_key).
func topicMatches(got, want string, catalog *models.TopicCatalog) bool {
	if topicMatchesExact(got, want, catalog) {
		return true
	}
	// A model often adds a qualifier to the requested topic: «X (уточнение)»,
	// «X: подтема», «X — подтема». It is still a question on topic X.
	if b := topicBase(got); b != "" && b != strings.TrimSpace(got) {
		return topicMatchesExact(b, want, catalog)
	}
	return false
}

// topicBase strips a trailing qualifier from a model-written topic:
// «Генетика (законы Менделя)» → «Генетика», «Генетика: законы» →
// «Генетика», «Генетика — законы» → «Генетика». Only the GOT side is ever
// stripped — a requested topic keeps its full meaning.
func topicBase(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.Index(t, "("); i > 0 {
		t = t[:i]
	}
	for _, sep := range []string{":", " — ", " – ", " - "} {
		if i := strings.Index(t, sep); i > 0 {
			t = t[:i]
		}
	}
	return strings.TrimSpace(t)
}

func topicMatchesExact(got, want string, catalog *models.TopicCatalog) bool {
	if models.NormalizeTopic(got) == models.NormalizeTopic(want) {
		return true
	}
	// Cosmetic differences a model makes when «copying» a topic: quotes,
	// a trailing dot, ё/е. They used to reject a correct question as
	// «тема не по спецификации».
	lg, lw := looseTopic(got), looseTopic(want)
	if lg != "" && lg == lw {
		return true
	}
	if catalog == nil {
		return false
	}
	kg, ok1 := resolveLoose(catalog, got)
	kw, ok2 := resolveLoose(catalog, want)
	return ok1 && ok2 && kg == kw
}

// looseTopic is NormalizeTopic without the cosmetic noise of a model's
// copy: surrounding quotes/brackets/punctuation and ё → е. Used only to
// MATCH topics (the stored spelling is the requested one).
func looseTopic(t string) string {
	t = strings.ReplaceAll(strings.ReplaceAll(t, "ё", "е"), "Ё", "Е")
	t = strings.Trim(strings.TrimSpace(t), " «»\"'“”„`.,;:!?")
	return models.NormalizeTopic(t)
}

// resolveLoose resolves a topic through the catalog, retrying with the
// loose spelling.
func resolveLoose(catalog *models.TopicCatalog, t string) (string, bool) {
	if k, ok := catalog.Resolve(t); ok {
		return k, true
	}
	return catalog.Resolve(strings.Trim(strings.TrimSpace(t), " «»\"'“”„`.,;:!?"))
}

// validateWeakCoverage rejects a chain reply that ignores the weak topics:
// at least min(weakCoverageNeed, len(weak)) distinct weak topics must be
// covered by some question.
func validateWeakCoverage(gt *generatedTest, weak []string, catalog *models.TopicCatalog) error {
	if len(weak) == 0 {
		return nil
	}
	need := min(weakCoverageNeed, len(weak))
	covered := 0
	for _, w := range weak {
		for _, q := range gt.Questions {
			if topicMatches(q.Topic, w, catalog) {
				covered++
				break
			}
		}
	}
	if covered < need {
		return rejectf(rejectWeak, "only %d of the weak topics covered, need %d (%s)", covered, need, strings.Join(weak, "; "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Difficulty
// ---------------------------------------------------------------------------

// chainDifficultyOutlierGap / chainDifficultyMaxOutliers: questions whose
// level is ≥ 2 away from the target mean are outliers; more than 20% of
// them means the model ignored the requested distribution (e.g. 10 level-1
// and 10 level-5 questions averaging to a "correct" 3.0).
const (
	chainDifficultyOutlierGap  = 2.0
	chainDifficultyMaxOutliers = 0.2
)

// chainRegressionSlack: a chain test may not be easier than
// min(previous mean, own target) - slack — the progression never goes back.
const chainRegressionSlack = 0.3

// validateChainProgression rejects a test noticeably easier than the
// previous chain test (prevMean = 0: unknown, no check).
func validateChainProgression(gt *generatedTest, testNumber int, prevMean float64) error {
	if prevMean <= 0 || len(gt.Questions) == 0 {
		return nil
	}
	floor := math.Min(prevMean, chainDifficultyTarget(testNumber)) - chainRegressionSlack
	mean := genMeanDifficulty(gt)
	if mean < floor {
		return rejectf(rejectDifficulty, "difficulty: mean %.2f regresses below the previous test (%.1f, floor %.2f) at test %d", mean, prevMean, floor, testNumber)
	}
	return nil
}

func genMeanDifficulty(gt *generatedTest) float64 {
	if len(gt.Questions) == 0 {
		return 0
	}
	sum := 0
	for _, q := range gt.Questions {
		sum += q.Difficulty
	}
	return float64(sum) / float64(len(gt.Questions))
}

// ---------------------------------------------------------------------------
// Answer-key balance
// ---------------------------------------------------------------------------

// rebalanceAnswerKeys moves correct answers from over-used positions to
// under-used ones by swapping two options of a question (the content is
// unchanged; attempts shuffle the options anyway). Batch generation can not
// balance keys across independent model calls, so the test is balanced
// locally instead of being rejected for it.
func rebalanceAnswerKeys(gt *generatedTest) {
	n := len(gt.Questions)
	if n == 0 {
		return
	}
	var count [4]int
	for _, q := range gt.Questions {
		if q.Correct >= 0 && q.Correct < 4 {
			count[q.Correct]++
		}
	}
	limit := (n + 3) / 4
	for i := range gt.Questions {
		q := &gt.Questions[i]
		if q.Correct < 0 || q.Correct > 3 || len(q.Options) != 4 || count[q.Correct] <= limit {
			continue
		}
		best := -1
		for p := 0; p < 4; p++ {
			if count[p] < limit && (best < 0 || count[p] < count[best]) {
				best = p
			}
		}
		if best < 0 {
			continue
		}
		q.Options[q.Correct], q.Options[best] = q.Options[best], q.Options[q.Correct]
		count[q.Correct]--
		count[best]++
		q.Correct = best
	}
}

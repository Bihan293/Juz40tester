package models

import (
	"sort"
	"strings"
)

// Weak-topic detection is based on the user's ACCUMULATED statistics per
// TOPIC of a subject (table user_topic_stats), not on the status of
// individual questions. A question's 🔴/🟡/🟢 status says how well the user
// remembers THAT question; a topic's level says whether the user
// systematically fails the topic — across all tests, chain and personal,
// including questions that no longer exist.
//
// The level is computed over the last TopicWindow answers of the topic (a
// sliding window), so a topic the user has since fixed leaves the weak list,
// and old mistakes do not haunt the user forever.
const (
	// TopicWindow is how many most recent answers of a topic are evaluated.
	TopicWindow = 10
	// TopicMinAnswers — fewer answers than this are not enough evidence.
	TopicMinAnswers = 3
	// TopicMinWrong — a topic is never weak because of a single slip.
	TopicMinWrong = 2
	// TopicRedBelow / TopicOKFrom are accuracy thresholds (percent) inside
	// the window: < 50% → 🔴, 50–79% → 🟡, ≥ 80% → fine.
	TopicRedBelow = 50
	TopicOKFrom   = 80
)

// Topic levels.
const (
	TopicUnknown = -1 // not enough answers yet
	TopicRed     = 0  // 🔴 systematic mistakes
	TopicYellow  = 1  // 🟡 unstable
	TopicOK      = 2  // 🟢 fine
)

// TopicStat is the user's accumulated statistics of one topic of a subject.
type TopicStat struct {
	SubjectID int64
	Key       string // normalised topic key (NormalizeTopic)
	Topic     string // display spelling (latest seen)
	Correct   int    // all-time counters
	Wrong     int
	Recent    string // last ≤ TopicWindow answers, oldest first: '1' correct, '0' wrong
}

// NormalizeTopic canonicalises a topic for grouping: lower case, collapsed
// whitespace. «Клетка», «клетка » and «КЛЕТКА» are the same topic.
func NormalizeTopic(t string) string {
	return strings.ToLower(strings.Join(strings.Fields(t), " "))
}

// WindowCounts returns (answers, correct) inside the recent window.
func (s TopicStat) WindowCounts() (answers, correct int) {
	r := s.Recent
	if len(r) > TopicWindow {
		r = r[len(r)-TopicWindow:]
	}
	return len(r), strings.Count(r, "1")
}

// Level classifies the topic from its recent window.
func (s TopicStat) Level() int {
	n, c := s.WindowCounts()
	if n < TopicMinAnswers {
		return TopicUnknown
	}
	wrong := n - c
	pct := c * 100 / n
	switch {
	case wrong < TopicMinWrong || pct >= TopicOKFrom:
		return TopicOK
	case pct < TopicRedBelow:
		return TopicRed
	default:
		return TopicYellow
	}
}

// IsWeak reports whether the topic is 🔴 or 🟡.
func (s TopicStat) IsWeak() bool {
	l := s.Level()
	return l == TopicRed || l == TopicYellow
}

// TopicLevelEmoji returns the mark of a topic level.
func TopicLevelEmoji(level int) string {
	switch level {
	case TopicRed:
		return "🔴"
	case TopicYellow:
		return "🟡"
	case TopicOK:
		return "🟢"
	}
	return "⚪"
}

// WeakTopicStats filters the weak topics and sorts them worst first:
// 🔴 before 🟡, then by lower recent accuracy, then by more all-time
// mistakes, then alphabetically (stable, deterministic — the fingerprint
// cache relies on it). limit <= 0 means no limit.
func WeakTopicStats(stats []TopicStat, limit int) []TopicStat {
	out := make([]TopicStat, 0, len(stats))
	for _, s := range stats {
		if s.IsWeak() {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].Level(), out[j].Level()
		if li != lj {
			return li < lj
		}
		ni, ci := out[i].WindowCounts()
		nj, cj := out[j].WindowCounts()
		// compare ci/ni < cj/nj without floats
		if ci*nj != cj*ni {
			return ci*nj < cj*ni
		}
		if out[i].Wrong != out[j].Wrong {
			return out[i].Wrong > out[j].Wrong
		}
		return out[i].Key < out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

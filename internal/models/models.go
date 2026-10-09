// Package models defines domain entities and knowledge-status rules.
package models

import "time"

// Knowledge status of a question for a user.
const (
	StatusNone      = 0 // 🔴 — not learned / not answered correctly
	StatusPartial   = 1 // 🟡 — answered correctly once
	StatusMastered  = 2 // 🟢 — answered correctly at least twice
	DefaultStatus   = StatusNone
	FirstCorrect    = StatusPartial
	MasteredCorrect = StatusMastered
)

// StatusEmoji returns the emoji for a knowledge status.
func StatusEmoji(status int) string {
	switch status {
	case StatusMastered:
		return "🟢"
	case StatusPartial:
		return "🟡"
	default:
		return "🔴"
	}
}

// NextStatus applies the knowledge-system transition rules.
//
//	correct:   🔴→🟡, 🟡→🟢, 🟢→🟢
//	incorrect: 🔴→🔴, 🟡→🔴, 🟢→🟡
func NextStatus(current int, correct bool) int {
	if correct {
		if current < StatusMastered {
			return current + 1
		}
		return StatusMastered
	}
	if current > StatusNone {
		return current - 1
	}
	return StatusNone
}

// User is a registered Telegram user.
type User struct {
	ID           int64
	TelegramID   int64
	Username     string
	FirstName    string
	LastName     string
	LanguageCode string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// StreakDays is the daily-activity streak (the 🔥 "огонёк"): consecutive
	// calendar days (StreakLocation, Kazakhstan UTC+5) with any bot activity.
	// Day 1 -> 1; a missed day resets it to 1 (see NextStreak). The stored
	// value only changes on activity — display it via EffectiveStreak.
	// LastActiveDate is the calendar date of the last activity.
	StreakDays     int
	LastActiveDate time.Time
	// TestLang is the language of the TEST CONTENT only ("ru" or "kk") — the
	// bot interface (buttons, menus, system messages) always stays Russian.
	TestLang string
	// ActiveAttemptID is the attempt the user is inside right now («test
	// mode»: the main menu is hidden, no other test opens). 0 = none. Read
	// by UserRepository.Upsert so the router decides with ZERO extra
	// queries when no test is open; it may point to an attempt that has
	// been closed meanwhile (the router re-checks via ActiveTestOf).
	ActiveAttemptID int64
}

// Test content languages (settings screen). The interface language is NOT
// affected by this choice.
const (
	TestLangRU = "ru" // 🇷🇺 Русский — the master version of every test
	TestLangKK = "kk" // 🇰🇿 Қазақша — translated on demand, cached in the DB
)

// Subject is a school subject.
type Subject struct {
	ID       int64
	Name     string
	Position int
}

// Question is a multiple-choice question (4 options).
type Question struct {
	ID            int64
	SubjectID     int64
	Text          string
	OptionA       string
	OptionB       string
	OptionC       string
	OptionD       string
	CorrectAnswer string // "A", "B", "C" or "D"
	Topic         string
	Difficulty    int
}

// Options returns the four answer options as a slice.
func (q *Question) Options() []string {
	return []string{q.OptionA, q.OptionB, q.OptionC, q.OptionD}
}

// Test kinds.
const (
	TestKindChain    = "chain"    // main linear chain: Тест 1, Тест 2, ...
	TestKindPersonal = "personal" // per-user weak-topics test (owner_user_id), deletable on finish
	// TestKindCustom: «✨ Свой тест» — a per-user test generated from the
	// student's own description (owner_user_id), deletable on finish. Never
	// part of the bank, templates, topic statistics, chain or daily quota.
	TestKindCustom = "custom"
	// JobKindTopicBatch (B4) is a generation job (not a test kind): ~10 bank
	// questions on one catalog topic, stored without a test row.
	JobKindTopicBatch = "topic_batch"
)

// Test is a fixed set of questions for a subject.
type Test struct {
	ID          int64
	SubjectID   int64
	TestNumber  int
	Title       string
	IsActive    bool
	Kind        string
	Topics      []string
	OwnerUserID int64 // personal tests only: the user this test belongs to (0 otherwise)
	// TopicsFingerprint is sha256 of the sorted weak-topic set the personal
	// test was generated for. Users with IDENTICAL weak topics share the same
	// questions (a clone of the same test, zero AI cost).
	TopicsFingerprint string
	// OriginTestID is the root personal test a clone was copied from (0 for
	// generated tests). Used to never hand a user content they finished.
	OriginTestID int64
	// FromBank (B3): personal test assembled from the question bank — it
	// links existing questions and owns none of them.
	FromBank bool
	// CustomOrderID: the custom_test_orders row a custom test was generated
	// for (set only when the test is stored; not read back by scanTest).
	CustomOrderID int64
}

// UserSubjectState is per-user UI/progress state that survives restarts.
type UserSubjectState struct {
	UserID         int64
	SubjectID      int64
	TestsPage      int // last opened page of the tests grid
	LastTestNumber int // highest chain test that reached the unlock bar (permanent unlock watermark)
}

// GenerationJob is a queued AI test-generation task.
type GenerationJob struct {
	ID                int64
	Kind              string // "chain" or "personal"
	SubjectID         int64
	TestNumber        int    // chain jobs only
	TopicsFingerprint string // legacy weak jobs only
	Status            string // pending | running | done | failed
	Attempts          int
	LastError         string
	TestID            int64
	Urgent            bool   // run immediately, ignore the off-peak deferral
	OwnerUserID       int64  // personal jobs only: the user the test is generated for
	TopicKey          string // topic_batch jobs only: the catalog topic (B4)
	CustomOrderID     int64  // custom jobs only: the custom_test_orders row
}

// IsOwnedKind reports a per-user test kind (weak-topics / custom): such a
// test opens only for its owner and is deleted when finished.
func IsOwnedKind(kind string) bool { return kind == TestKindPersonal || kind == TestKindCustom }

// Unlock rule for the next chain test: the previous test must have at least
// this many 🟢 (mastered) and 🟡 (in progress) questions.
const (
	UnlockGreen  = 15
	UnlockYellow = 5
	// TestsPerPage is the number of test buttons on one page of the subject
	// screen (a grid of 3 per row). MaxVisibleTests caps how far ahead the
	// user can look.
	TestsPerPage     = 12 // 3 x 4 grid
	TestsGridColumns = 3
	// MaxVisibleTests is the length of a subject's chain. The difficulty
	// curve (see services.chainDifficultyTarget) keeps rising up to ~150, so
	// the chain is long enough for "100+ — серьёзно сложные тесты".
	MaxVisibleTests = 200
)

// ClampVisibleTests caps a chain level for DISPLAY: after the last test of
// the chain (MaxVisibleTests) is passed, the unlock watermark yields
// MaxVisibleTests+1, which is not a real test. Access logic keeps the raw
// value; the UI shows at most MaxVisibleTests.
func ClampVisibleTests(n int) int {
	if n > MaxVisibleTests {
		return MaxVisibleTests
	}
	return n
}

// MeetsUnlockBar reports whether a test with the given 🟢/🟡 counts reached
// the unlock bar «15🟢 + 5🟡». The bar means "at least 15 mastered AND the
// remaining questions at least in progress": 16🟢+4🟡 or 20🟢 are BETTER
// than 15🟢+5🟡 and must pass too. (The old check `yellow >= 5` locked out
// exactly the students who knew the test best: with 20 questions, 16🟢
// leaves at most 4🟡, 20🟢 leaves 0🟡.)
func MeetsUnlockBar(green, yellow int) bool {
	return green >= UnlockGreen && green+yellow >= UnlockGreen+UnlockYellow
}

// Attempt statuses.
const (
	AttemptInProgress = "in_progress"
	AttemptCompleted  = "completed"
	AttemptAbandoned  = "abandoned"
)

// TestAttempt is one run of a test by a user.
type TestAttempt struct {
	ID              int64
	UserID          int64
	TestID          int64
	Status          string
	CurrentPosition int
	CorrectCount    int
	WrongCount      int
	StartedAt       time.Time
	CompletedAt     *time.Time
}

// AttemptQuestion binds a question to an attempt at a shuffled position.
type AttemptQuestion struct {
	ID             int64
	AttemptID      int64
	QuestionID     int64
	Position       int
	Answered       bool
	SelectedAnswer string
	IsCorrect      *bool
	OptionOrder    []string // shuffled display order of labels, e.g. ["C","A","D","B"]
}

// QuestionTranslation is the cached translation of a question into a
// non-master language (currently only Kazakh). The Russian question row is
// always the master; translations are produced once per question and reused
// by every user afterwards.
type QuestionTranslation struct {
	QuestionID int64
	Lang       string
	Text       string
	OptionA    string
	OptionB    string
	OptionC    string
	OptionD    string
	Topic      string
	// CorrectAnswer is copied 1:1 from the master question row server-side:
	// the model translates ONLY the texts, never the answer key, so a sloppy
	// translation can never corrupt the correct answers.
	CorrectAnswer string // "A", "B", "C" or "D" — identical to the master row
}

// UserQuestionProgress stores per-user knowledge status for a question.
type UserQuestionProgress struct {
	UserID       int64
	QuestionID   int64
	Status       int
	CorrectCount int
	WrongCount   int
	UpdatedAt    time.Time
}

// SubjectProgress is aggregated knowledge statistics per subject.
type SubjectProgress struct {
	SubjectID      int64
	SubjectName    string
	TotalQuestions int
	Green          int
	Yellow         int
	Red            int
	CorrectCount   int
	WrongCount     int
}

// LeaderboardEntry is one row of the leaderboard: a user with their daily
// streak and, for subject boards, how many chain tests they unlocked and
// how many questions of the subject are mastered (🟢).
type LeaderboardEntry struct {
	UserID        int64
	FirstName     string
	Username      string
	StreakDays    int
	UnlockedTests int
	Green         int
}

// Package services contains the business logic of JUZ40 Tester.
package services

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	mrand "math/rand"
	"sync"

	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// OptionLabels are the displayed answer labels.
var OptionLabels = []string{"A", "B", "C", "D"}

// QuizService orchestrates subjects, tests, attempts and knowledge progress.
type QuizService struct {
	subjects   *repositories.SubjectRepository
	attempts   *repositories.AttemptRepository
	state      *repositories.StateRepository
	gen        *repositories.GenerationRepository
	genSvc     *GeneratorService            // nil when AI generation is disabled
	users      *repositories.UserRepository // leaderboard (streaks) — may be nil
	translator *TranslatorService           // nil when translation is unavailable
	// rng shuffles questions and options per attempt. math/rand.Rand is NOT
	// safe for concurrent use, and webhook updates are processed in parallel
	// goroutines — every Shuffle must hold the mutex, otherwise two users
	// starting a test at the same moment corrupt the source and panic.
	rng   *mrand.Rand
	rngMu sync.Mutex
}

func NewQuizService(subjects *repositories.SubjectRepository, attempts *repositories.AttemptRepository, state *repositories.StateRepository, gen *repositories.GenerationRepository, genSvc *GeneratorService, users ...*repositories.UserRepository) *QuizService {
	s := &QuizService{subjects: subjects, attempts: attempts, state: state, gen: gen, genSvc: genSvc, rng: newSecureRand()}
	if len(users) > 0 {
		s.users = users[0]
	}
	return s
}

// WithTranslator wires the Kazakh translation service (optional — when nil,
// every test is served in the Russian master version).
func (s *QuizService) WithTranslator(t *TranslatorService) *QuizService {
	s.translator = t
	return s
}

// newSecureRand creates a math/rand source seeded from crypto/rand.
func newSecureRand() *mrand.Rand {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; fall back to a fixed seed rather than failing.
		return mrand.New(mrand.NewSource(1))
	}
	return mrand.New(mrand.NewSource(int64(binary.LittleEndian.Uint64(b[:]))))
}

// ListSubjects returns all subjects for the "Предметы" screen.
func (s *QuizService) ListSubjects(ctx context.Context) ([]models.Subject, error) {
	return s.subjects.List(ctx)
}

// GetSubject returns a subject by id (leaderboard screen header).
func (s *QuizService) GetSubject(ctx context.Context, subjectID int64) (*models.Subject, error) {
	return s.subjects.GetByID(ctx, subjectID)
}

// GetTest returns a test by id.
func (s *QuizService) GetTest(ctx context.Context, testID int64) (*models.Test, error) {
	return s.subjects.GetTest(ctx, testID)
}

// TestQuestionCount returns how many questions are attached to a test.
func (s *QuizService) TestQuestionCount(ctx context.Context, testID int64) (int, error) {
	return s.subjects.TestQuestionCount(ctx, testID)
}

// ResumeOrNil returns the user's unfinished attempt for a test, or nil.
func (s *QuizService) ResumeOrNil(ctx context.Context, userID, testID int64) (*models.TestAttempt, error) {
	return s.attempts.GetActiveAttempt(ctx, userID, testID)
}

// SubjectInfo describes a subject screen: its tests and the unfinished
// attempt per test (so the user can resume it).
type SubjectInfo struct {
	Subject       *models.Subject
	Tests         []models.Test
	QuestionCount map[int64]int                 // testID -> number of questions
	Resume        map[int64]*models.TestAttempt // testID -> unfinished attempt
}

// GetSubjectInfo loads a subject with its tests and the user's unfinished
// attempts for those tests.
func (s *QuizService) GetSubjectInfo(ctx context.Context, userID, subjectID int64) (*SubjectInfo, error) {
	subject, err := s.subjects.GetByID(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	tests, err := s.subjects.ListTests(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	if len(tests) == 0 {
		return nil, repositories.ErrNotFound
	}
	info := &SubjectInfo{
		Subject:       subject,
		Tests:         tests,
		QuestionCount: make(map[int64]int, len(tests)),
		Resume:        make(map[int64]*models.TestAttempt, len(tests)),
	}
	for i := range tests {
		t := &tests[i]
		count, err := s.subjects.TestQuestionCount(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		info.QuestionCount[t.ID] = count
		resume, err := s.attempts.GetActiveAttempt(ctx, userID, t.ID)
		if err != nil {
			return nil, err
		}
		if resume != nil {
			info.Resume[t.ID] = resume
		}
	}
	return info, nil
}

// ---------------------------------------------------------------------------
// Subject screen: paginated chain grid, unlock chain, pre-generation
// ---------------------------------------------------------------------------

// TestSlot is one cell of the tests grid.
type TestSlot struct {
	Test      *models.Test // nil while the test is still being generated
	Number    int
	Unlocked  bool
	Completed bool   // user's knowledge of this test already satisfies the unlock bar
	Resume    bool   // an unfinished attempt exists
	Pending   bool   // unlocked but not generated yet
	Label     string // button caption without decorations, e.g. "Тест 3"
}

// SubjectScreen is everything the subject page needs.
type SubjectScreen struct {
	Subject     *models.Subject
	Slots       []TestSlot // the current page, up to TestsPerPage cells
	Page        int        // current page (0-based, after clamping)
	TotalPages  int
	UnlockedMax int // highest unlocked chain test number
	MaxVisible  int // min(MaxVisibleTests, UnlockedMax + lookahead)
}

// WeakEntry is the per-user weak-topics test state of one subject.
type WeakEntry struct {
	Test    *models.Test // the user's own personal test, ready to start
	Pending bool         // generation queued (urgent — runs right away)
	Topics  []string
	Hidden  bool // not enough data yet — button is not shown
}

// unlockedMax computes how far the user's chain is unlocked: test 1 is
// always unlocked; test n+1 unlocks when the user's knowledge of test n has
// at least UnlockGreen 🟢 and UnlockYellow 🟡 questions. The chain breaks
// at the first test below the bar (or not generated yet).
//
// The lookup is by test NUMBER, not by slice position: a hole in the chain
// (e.g. tests 2 and 3 exist but test 1 was removed) must never lock test 1
// itself — otherwise the user would be asked to pass a test that does not
// exist in order to open the first one.
//
// Once a test has reached the bar, the unlock is PERMANENT: the per-user
// watermark user_subject_state.last_test_number (the highest chain test
// that reached the bar) keeps every test up to last_test_number+1 open,
// even if a later retry of an earlier test drops its 🟢/🟡 below the bar.
func (s *QuizService) unlockedMax(ctx context.Context, userID, subjectID int64, chain []models.Test) (int, error) {
	ids := make([]int64, 0, len(chain))
	byNumber := make(map[int]*models.Test, len(chain))
	for i := range chain {
		ids = append(ids, chain[i].ID)
		byNumber[chain[i].TestNumber] = &chain[i]
	}
	progress, err := s.gen.TestProgressForUser(ctx, userID, ids)
	if err != nil {
		return 0, err
	}
	watermark := 0
	if s.state != nil {
		st, err := s.state.Get(ctx, userID, subjectID)
		if err != nil {
			return 0, err
		}
		watermark = st.LastTestNumber
	}
	unlocked := 1 // Тест 1 всегда открыт
	for {
		t := byNumber[unlocked]
		if t == nil {
			break // следующий тест ещё не сгенерирован — цепочка упирается сюда
		}
		p := progress[t.ID]
		passed := unlocked <= watermark || (p != nil && models.MeetsUnlockBar(p.Green, p.Yellow))
		if !passed {
			break // следующий тест закрыт
		}
		unlocked++
	}
	if watermark+1 > unlocked {
		unlocked = watermark + 1
	}
	return unlocked, nil
}

// GetSubjectScreen builds the tests-grid page: 3-per-row cells, lock marks,
// pre-generation of the next chain test and the weak-topics entry. The page
// is clamped to the visible range and remembered per user (so reopening the
// subject restores exactly where the user stopped).
func (s *QuizService) GetSubjectScreen(ctx context.Context, userID, subjectID int64, page int, remember bool) (*SubjectScreen, error) {
	subject, err := s.subjects.GetByID(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	chain, err := s.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		return nil, err
	}

	unlockedMax, err := s.unlockedMax(ctx, userID, subjectID, chain)
	if err != nil {
		return nil, err
	}
	maxVisible := unlockedMax + 1 // show one step ahead (pending / generating)
	if maxVisible > models.MaxVisibleTests {
		maxVisible = models.MaxVisibleTests
	}
	if maxVisible < len(chain) {
		maxVisible = len(chain) // never hide already-generated tests
	}
	if maxVisible > models.MaxVisibleTests {
		maxVisible = models.MaxVisibleTests
	}

	totalPages := (maxVisible + models.TestsPerPage - 1) / models.TestsPerPage
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	if remember && s.state != nil {
		if err := s.state.SavePage(ctx, userID, subjectID, page); err != nil {
			return nil, err
		}
	}

	// Next-test generation is NOT triggered here. The user asked for it to
	// start ONLY at the moment the unlock bar (15🟢 + 5🟡) is actually
	// reached — that is OnTestCompleted. Opening a subject screen must never
	// queue a paid generation: a user who just browses the grid would
	// otherwise burn an API call for a test they cannot even open yet.
	//
	// Self-healing only: when a test the user CAN ALREADY OPEN is missing
	// (a generation failed earlier and left a hole in the chain), it is
	// re-queued URGENTLY — the user must never wait for a test they are
	// allowed to open. This is a recovery path, not pre-generation.
	if s.genSvc != nil && s.genSvc.Enabled() {
		byNumber0 := make(map[int]bool, len(chain))
		for _, t := range chain {
			byNumber0[t.TestNumber] = true
		}
		if !byNumber0[unlockedMax] && unlockedMax >= 1 && unlockedMax <= models.MaxVisibleTests {
			if _, err := s.genSvc.EnsureChainTest(ctx, subjectID, unlockedMax, true, userID); err != nil {
				_ = err // generation failures must never break the UI
			}
		}
	}

	// Knowledge progress of the visible chain tests (lock/completed marks).
	ids := make([]int64, 0, len(chain))
	for _, t := range chain {
		ids = append(ids, t.ID)
	}
	progress, err := s.gen.TestProgressForUser(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	byNumber := make(map[int]*models.Test, len(chain))
	for i := range chain {
		byNumber[chain[i].TestNumber] = &chain[i]
	}

	start := page * models.TestsPerPage
	end := start + models.TestsPerPage
	if end > maxVisible {
		end = maxVisible
	}
	slots := make([]TestSlot, 0, end-start)
	for n := start + 1; n <= end; n++ {
		slot := TestSlot{Number: n, Label: fmt.Sprintf("%d", n)}
		t := byNumber[n]
		slot.Test = t
		slot.Unlocked = n <= unlockedMax
		if t != nil {
			p := progress[t.ID]
			slot.Completed = p != nil && models.MeetsUnlockBar(p.Green, p.Yellow)
			resume, err := s.attempts.GetActiveAttempt(ctx, userID, t.ID)
			if err != nil {
				return nil, err
			}
			slot.Resume = resume != nil
		} else if slot.Unlocked {
			slot.Pending = true
		}
		slots = append(slots, slot)
	}

	screen := &SubjectScreen{
		Subject:     subject,
		Slots:       slots,
		Page:        page,
		TotalPages:  totalPages,
		UnlockedMax: unlockedMax,
		MaxVisible:  maxVisible,
	}
	// NOTE: weak-topics tests are NOT looked up or generated here anymore —
	// opening a subject screen must never trigger a paid generation. They
	// live in the main menu ("🎯 Слабые темы") and are created only when the
	// user explicitly asks for one.
	return screen, nil
}

// SavedTestsPage returns the remembered tests-grid page for the user.
func (s *QuizService) SavedTestsPage(ctx context.Context, userID, subjectID int64) (int, error) {
	st, err := s.state.Get(ctx, userID, subjectID)
	if err != nil {
		return 0, err
	}
	return st.TestsPage, nil
}

// CanOpenTest checks the unlock rule for a chain test before starting it.
// Weak tests are always open. Returns (allowed, reason).
//
// The check goes through unlockedMax, so it is robust to holes in the chain
// numbering: the user is always asked to pass the FIRST unfinished test of
// the chain, never a "previous number" that might not exist (a missing
// previous test used to make the test impossible to open at all).
func (s *QuizService) CanOpenTest(ctx context.Context, userID int64, test *models.Test) (bool, string, error) {
	if test.Kind != models.TestKindChain {
		return true, "", nil
	}
	chain, err := s.subjects.ListChainTests(ctx, test.SubjectID)
	if err != nil {
		return false, "", err
	}
	unlocked, err := s.unlockedMax(ctx, userID, test.SubjectID, chain)
	if err != nil {
		return false, "", err
	}
	if test.TestNumber <= unlocked {
		return true, "", nil
	}
	// The blocking test is the first unfinished one of the chain (test number
	// == unlocked), which is not necessarily test.TestNumber-1.
	var blocking *models.Test
	for i := range chain {
		if chain[i].TestNumber == unlocked {
			blocking = &chain[i]
			break
		}
	}
	if blocking == nil {
		// The next test of the chain is still being generated.
		return false, fmt.Sprintf("«Тест %d» ещё генерируется — загляни чуть позже ⏳", unlocked), nil
	}
	progress, err := s.gen.TestProgressForUser(ctx, userID, []int64{blocking.ID})
	if err != nil {
		return false, "", err
	}
	green, yellow := 0, 0
	if p := progress[blocking.ID]; p != nil {
		green, yellow = p.Green, p.Yellow
	}
	reason := fmt.Sprintf("Чтобы открыть «Тест %d», доведи «Тест %d» до %d🟢 + %d🟡 (сейчас %d🟢 + %d🟡)",
		test.TestNumber, blocking.TestNumber, models.UnlockGreen, models.UnlockYellow, green, yellow)
	return false, reason, nil
}

// OnTestCompleted is called after an attempt is finished: ONLY when the
// attempt reached the unlock bar it bumps the chain unlock watermark and when the attempt reached the unlock bar
// (15🟢 + 5🟡), queues the generation of the next chain test URGENTLY.
//
// Why only at the bar: the user wants the AI to see the FULL knowledge
// picture of a finished (15🟢+5🟡) test — that is the only moment when the
// weak/mastered topics are stable enough to build the next test on. An
// unfinished attempt tells the model almost nothing, so generating earlier
// would waste an API call on a worse test.
func (s *QuizService) OnTestCompleted(ctx context.Context, userID int64, test *models.Test, green, yellow int) {
	if test.Kind != models.TestKindChain || s.state == nil {
		return
	}
	// Not at the bar yet — nothing to unlock or generate. The user keeps
	// training this same test; generation starts exactly when the bar is
	// crossed.
	if !models.MeetsUnlockBar(green, yellow) {
		return
	}
	// The bar is reached — remember it permanently (the unlock watermark):
	// a later retry with mistakes must never lock the next tests again.
	if err := s.state.SaveProgress(ctx, userID, test.SubjectID, test.TestNumber, 0); err != nil {
		return
	}
	if s.genSvc == nil || !s.genSvc.Enabled() {
		return
	}
	chain, err := s.subjects.ListChainTests(ctx, test.SubjectID)
	if err != nil {
		return
	}
	byNumber := make(map[int]bool, len(chain))
	for _, t := range chain {
		byNumber[t.TestNumber] = true
	}
	// The next test of the chain (the one this unlock opened) does not
	// exist yet — generate it NOW, with the freshest knowledge marks.
	if next := test.TestNumber + 1; next <= models.MaxVisibleTests && !byNumber[next] {
		_, _ = s.genSvc.EnsureChainTest(ctx, test.SubjectID, next, true, userID)
	}
}

// ReviveChainTest exposes the stuck/failed-generation recovery to handlers:
// a tap on a ⏳ (still generating) test button re-queues the job as URGENT.
// Safe to call for any (subject, testNumber) — it is a no-op when the test
// already exists, generation is disabled, or a healthy job is in flight.
//
// The test number comes from callback data, which can be stale or forged:
// only tests the user has actually unlocked (testNumber <= unlockedMax) may
// trigger a (paid) generation — otherwise any Тест 50/100 could be
// generated on demand.
func (s *QuizService) ReviveChainTest(ctx context.Context, subjectID int64, testNumber int, userID int64) {
	if s.genSvc == nil || testNumber < 1 {
		return
	}
	chain, err := s.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		log.Printf("revive chain test: list subject %d: %v", subjectID, err)
		return
	}
	unlocked, err := s.unlockedMax(ctx, userID, subjectID, chain)
	if err != nil {
		log.Printf("revive chain test: unlocked subject %d user %d: %v", subjectID, userID, err)
		return
	}
	if testNumber > unlocked {
		log.Printf("revive chain test: refused subject %d test %d for user %d (unlocked up to %d)",
			subjectID, testNumber, userID, unlocked)
		return
	}
	s.genSvc.reviveChainTest(ctx, subjectID, testNumber, userID)
}

// SubjectsWithWeakTopics returns only the subjects in which the user has at
// least one weak (🔴/🟡) topic — the «🎯 Слабые темы» picker must not offer
// subjects the user never practised (there is nothing to build a personal
// test from, and tapping such a subject used to hang in «generating…»).
func (s *QuizService) SubjectsWithWeakTopics(ctx context.Context, userID int64) ([]models.Subject, error) {
	all, err := s.subjects.List(ctx)
	if err != nil {
		return nil, err
	}
	weak, err := s.gen.SubjectsWithWeakTopics(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]models.Subject, 0, len(weak))
	for _, subj := range all {
		if weak[subj.ID] {
			out = append(out, subj)
		}
	}
	return out, nil
}

// WeakTopicStats returns the user's weak topics of the subject with their
// statistics (worst first) for the «🎯 Слабые темы» screen.
func (s *QuizService) WeakTopicStats(ctx context.Context, userID, subjectID int64, limit int) ([]models.TopicStat, error) {
	return s.gen.WeakTopicStats(ctx, userID, subjectID, limit)
}

// ChainTestStatus reports whether chain test #number of the subject already
// exists (test != nil) or is still being generated (pending). Used by the
// «wait for the test» notifier: when both are empty the generation failed.
func (s *QuizService) ChainTestStatus(ctx context.Context, subjectID int64, number int) (test *models.Test, pending bool, err error) {
	chain, err := s.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		return nil, false, err
	}
	for i := range chain {
		if chain[i].TestNumber == number {
			return &chain[i], false, nil
		}
	}
	pending, err = s.gen.HasPendingOrRunningChainJob(ctx, subjectID, number)
	return nil, pending, err
}

// PersonalTestStatus reports whether the user's personal weak-topics test of
// the subject exists (test != nil) or is still being generated (pending).
func (s *QuizService) PersonalTestStatus(ctx context.Context, userID, subjectID int64) (test *models.Test, pending bool, err error) {
	test, err = s.gen.FindPersonalTest(ctx, subjectID, userID)
	if err != nil || test != nil {
		return test, false, err
	}
	pending, err = s.gen.HasPendingOrRunningPersonalJob(ctx, subjectID, userID)
	return nil, pending, err
}

// EnsurePersonalTest exposes the per-user weak-topics test
// lookup/generation to handlers ("🎯 Слабые темы" in the main menu).
//
// Weak-topic maintenance: an existing personal test whose topics are ALL no
// longer weak (every one of them got mastered since the test was generated)
// is stale — it is deleted and replaced by a fresh test built from the
// CURRENT weak topics. A test that still trains at least one actually-weak
// topic is kept as is (deleting it would waste the money already spent).
func (s *QuizService) EnsurePersonalTest(ctx context.Context, userID, subjectID int64) (test *models.Test, pending bool, topics []string, err error) {
	if s.genSvc == nil {
		return nil, false, nil, nil
	}
	test, pending, topics, err = s.genSvc.EnsurePersonalTest(ctx, userID, subjectID)
	if err != nil || test == nil || len(topics) == 0 {
		return test, pending, topics, err
	}
	// Match topics with the canonical normalisation used everywhere else
	// (case + inner whitespace), so «Тема  X» and «тема x» are the same.
	weak := make(map[string]bool, len(topics))
	for _, t := range topics {
		weak[models.NormalizeTopic(t)] = true
	}
	for _, t := range test.Topics {
		if weak[models.NormalizeTopic(t)] {
			return test, false, topics, nil // still trains a real gap — keep it
		}
	}
	// Never delete a test the user is in the middle of: the started attempt
	// (and its progress) would vanish silently. The stale test is replaced
	// only once no attempt is in progress.
	active, aerr := s.attempts.GetActiveAttempt(ctx, userID, test.ID)
	if aerr != nil {
		log.Printf("personal test %d: active attempt lookup: %v", test.ID, aerr)
		return test, false, topics, nil // unknown — keep the test
	}
	if active != nil {
		return test, false, topics, nil
	}
	// Stale: nothing left to train in this test. Delete it and build a fresh
	// one from the current weak topics (clone of a matching fingerprint when
	// one exists — otherwise one paid generation).
	if err := s.gen.DeletePersonalTest(ctx, userID, test.ID, subjectID); err != nil {
		return test, false, topics, nil // deletion failed — keep the old test
	}
	return s.genSvc.EnsurePersonalTest(ctx, userID, subjectID)
}

// FinishPersonalTest deletes the user's personal weak-topics test (and its
// questions) so the next weak-topics run generates a fresh one. Only the
// owner can finish their own personal test.
func (s *QuizService) FinishPersonalTest(ctx context.Context, userID, testID int64) error {
	test, err := s.subjects.GetTest(ctx, testID)
	if err != nil {
		return err
	}
	if test.Kind != models.TestKindPersonal || test.OwnerUserID != userID {
		return repositories.ErrNotFound
	}
	return s.gen.DeletePersonalTest(ctx, userID, testID, test.SubjectID)
}

// StartTest creates a brand-new attempt of the given test with shuffled
// questions and shuffled answer options. Every launch is a separate
// test_attempt; the shuffled orders are persisted in attempt_questions so a
// resumed attempt keeps its original order.
func (s *QuizService) StartTest(ctx context.Context, userID, testID int64) (*models.TestAttempt, error) {
	return s.startTest(ctx, userID, testID, false)
}

// RestartTest starts a brand-new attempt («🔄 Пройти ещё раз»): any attempt
// of this test that is still in progress is closed as abandoned first, so
// a double tap never leaves two open attempts.
func (s *QuizService) RestartTest(ctx context.Context, userID, testID int64) (*models.TestAttempt, error) {
	return s.startTest(ctx, userID, testID, true)
}

func (s *QuizService) startTest(ctx context.Context, userID, testID int64, replace bool) (*models.TestAttempt, error) {
	test, err := s.subjects.GetTest(ctx, testID)
	if err != nil {
		return nil, err
	}
	if !test.IsActive {
		return nil, repositories.ErrNotFound
	}
	questions, err := s.subjects.TestQuestions(ctx, test.ID)
	if err != nil {
		return nil, err
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("test %d has no questions", test.ID)
	}

	// Shuffle the question order for this attempt — under the mutex (the
	// RNG source is shared across all concurrent webhook goroutines).
	ids := make([]int64, len(questions))
	for i, q := range questions {
		ids[i] = q.ID
	}
	s.rngMu.Lock()
	s.rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })

	// Shuffle the option order (A/B/C/D) for every question of this attempt.
	orders := make([][]string, len(ids))
	for i := range ids {
		ord := append([]string(nil), OptionLabels...)
		s.rng.Shuffle(len(ord), func(a, b int) { ord[a], ord[b] = ord[b], ord[a] })
		orders[i] = ord
	}
	s.rngMu.Unlock()
	return s.attempts.CreateAttempt(ctx, userID, test.ID, ids, orders, replace)
}

// QuestionView is a question rendered for the user inside an attempt.
// Text and DisplayTexts are already in the user's TEST LANGUAGE (Russian
// master or the cached Kazakh translation); the correct-answer letter is
// identical in both versions, so the knowledge bookkeeping is untouched.
type QuestionView struct {
	Attempt      *models.TestAttempt
	AttemptQ     *models.AttemptQuestion
	Question     *models.Question // master (Russian) row — metadata only
	Text         string           // question text in the user's test language
	Total        int
	DisplayTexts []string // option texts in display order after shuffling
}

// CurrentQuestion loads the next unanswered question of the attempt.
func (s *QuizService) CurrentQuestion(ctx context.Context, attemptID int64, user *models.User) (*QuestionView, error) {
	attempt, err := s.attempts.GetAttemptForUser(ctx, attemptID, user.ID)
	if err != nil {
		return nil, err
	}
	aq, q, err := s.attempts.CurrentQuestion(ctx, attempt.ID)
	if err != nil {
		return nil, err
	}
	if aq == nil {
		return nil, nil // nothing left to answer
	}
	return s.buildView(ctx, user, attempt, aq, q)
}

// QuestionAtPosition loads the question at a fixed position of the attempt.
func (s *QuizService) QuestionAtPosition(ctx context.Context, attemptID int64, user *models.User, position int) (*QuestionView, error) {
	attempt, err := s.attempts.GetAttemptForUser(ctx, attemptID, user.ID)
	if err != nil {
		return nil, err
	}
	aq, q, err := s.attempts.QuestionAtPosition(ctx, attempt.ID, position)
	if err != nil {
		return nil, err
	}
	return s.buildView(ctx, user, attempt, aq, q)
}

func (s *QuizService) buildView(ctx context.Context, user *models.User, attempt *models.TestAttempt, aq *models.AttemptQuestion, q *models.Question) (*QuestionView, error) {
	total, err := s.subjects.TestQuestionCount(ctx, attempt.TestID)
	if err != nil {
		return nil, err
	}
	// The Russian master row is the default; Kazakh users get the cached
	// translation of THIS question (written once per question at test open).
	text := q.Text
	byLabel := map[string]string{
		"A": q.OptionA, "B": q.OptionB, "C": q.OptionC, "D": q.OptionD,
	}
	if user != nil && user.TestLang == models.TestLangKK && s.translator != nil &&
		s.subjectTranslatable(ctx, q.SubjectID) && s.testFullyTranslatedSafe(ctx, attempt.TestID) {
		// Only a COMPLETE translation is used — a partially translated test
		// is shown entirely in Russian instead of a per-question mix.
		if tr, terr := s.translator.TranslationFor(ctx, q.ID, models.TestLangKK); terr != nil {
			log.Printf("translation lookup q%d: %v", q.ID, terr)
		} else if tr != nil {
			text = tr.Text
			byLabel = map[string]string{
				"A": tr.OptionA, "B": tr.OptionB, "C": tr.OptionC, "D": tr.OptionD,
			}
		}
	}
	// Map original labels to texts, then apply the stored shuffle.
	texts := make([]string, 0, len(aq.OptionOrder))
	for _, orig := range aq.OptionOrder {
		texts = append(texts, byLabel[orig])
	}
	return &QuestionView{
		Attempt:      attempt,
		AttemptQ:     aq,
		Question:     q,
		Text:         text,
		Total:        total,
		DisplayTexts: texts,
	}, nil
}

// testFullyTranslatedSafe wraps TestFullyTranslated, treating lookup errors
// as "not translated" (Russian master is always a safe fallback).
func (s *QuizService) testFullyTranslatedSafe(ctx context.Context, testID int64) bool {
	ok, err := s.TestFullyTranslated(ctx, testID)
	if err != nil {
		log.Printf("translation status test %d: %v", testID, err)
		return false
	}
	return ok
}

// MapDisplayToOriginal converts the displayed letter (position in the
// shuffled list) back to the original option label stored in the DB.
func MapDisplayToOriginal(aq *models.AttemptQuestion, displayIndex int) (string, error) {
	if displayIndex < 0 || displayIndex >= len(aq.OptionOrder) {
		return "", fmt.Errorf("invalid option index %d", displayIndex)
	}
	return aq.OptionOrder[displayIndex], nil
}

// SubmitAnswer applies an answer through the repository transaction.
func (s *QuizService) SubmitAnswer(ctx context.Context, userID, attemptID int64, position int, original string) (*repositories.AnswerResult, error) {
	return s.attempts.SubmitAnswer(ctx, userID, attemptID, position, original)
}

// Exit keeps the attempt in progress ("Выйти") so it can be resumed later;
// it only verifies that the attempt belongs to the user.
func (s *QuizService) Exit(ctx context.Context, attemptID, userID int64) error {
	return s.attempts.AssertAttemptForUser(ctx, attemptID, userID)
}

// AttemptSummary aggregates data for the result screen.
type AttemptSummary struct {
	Attempt      *models.TestAttempt
	Test         *models.Test
	Subject      *models.Subject
	Total        int
	StatusCounts map[int]int // knowledge status -> count of test questions
}

// BuildSummary collects everything needed to render the final result. The
// 🔴🟡🟢 counts reflect the user's CURRENT knowledge status of the test's
// questions (after the run), not just the number of correct answers.
func (s *QuizService) BuildSummary(ctx context.Context, attemptID, userID int64) (*AttemptSummary, error) {
	attempt, err := s.attempts.GetAttemptForUser(ctx, attemptID, userID)
	if err != nil {
		return nil, err
	}
	test, err := s.subjects.GetTest(ctx, attempt.TestID)
	if err != nil {
		return nil, err
	}
	subject, err := s.subjects.GetByID(ctx, test.SubjectID)
	if err != nil {
		return nil, err
	}
	questions, err := s.subjects.TestQuestions(ctx, test.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(questions))
	for i, q := range questions {
		ids[i] = q.ID
	}
	statuses, err := s.subjects.QuestionStatuses(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	counts := map[int]int{models.StatusNone: 0, models.StatusPartial: 0, models.StatusMastered: 0}
	for _, id := range ids {
		counts[statuses[id]]++
	}
	return &AttemptSummary{
		Attempt:      attempt,
		Test:         test,
		Subject:      subject,
		Total:        len(questions),
		StatusCounts: counts,
	}, nil
}

// LeaderboardSize is how many rows each leaderboard shows.
const LeaderboardSize = 10

// StreakLeaderboard returns the top users by daily-activity streak (🔥).
// Returns (nil, nil) when the users repository is not wired.
func (s *QuizService) StreakLeaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error) {
	if s.users == nil {
		return nil, nil
	}
	return s.users.StreakLeaderboard(ctx, limit)
}

// UnlockedTestsLeaderboard returns the top users of a subject by unlocked
// chain tests ("уровни").
func (s *QuizService) UnlockedTestsLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	return s.subjects.UnlockedTestsLeaderboard(ctx, subjectID, limit)
}

// GreenLeaderboard returns the top users of a subject by mastered (🟢)
// questions.
func (s *QuizService) GreenLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	return s.subjects.GreenLeaderboard(ctx, subjectID, limit)
}

// SubjectProgress returns knowledge statistics of a single subject for the
// user (used by the "Мой прогресс -> предмет" screen).
//
// ONLY the tests the user can actually open RIGHT NOW are counted: the
// unlock chain decides which chain tests are accessible (test 1 always is;
// test n+1 requires the previous one at 15🟢 + 5🟡), and locked tests are
// fully excluded from progress, the 🟢/🟡/🔴 counters and the weak-topic
// calculations. Previously the statistics covered EVERY generated test, so
// a user with a single open test saw the red questions of a still-locked
// second test too.
func (s *QuizService) SubjectProgress(ctx context.Context, userID, subjectID int64) (*models.SubjectProgress, error) {
	chain, err := s.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	unlocked, err := s.unlockedMax(ctx, userID, subjectID, chain)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(chain))
	for _, t := range chain {
		if t.TestNumber <= unlocked {
			ids = append(ids, t.ID)
		}
	}
	return s.subjects.SubjectProgress(ctx, userID, subjectID, ids)
}

// ---------------------------------------------------------------------------
// Test content language (🇷🇺/🇰🇿 — the interface itself always stays Russian)
// ---------------------------------------------------------------------------

// ResolvedQuestion is a question rendered in the user's test language:
// either the Russian master row or its cached Kazakh translation. The
// correct-answer letter is always consistent with the shown options.
type ResolvedQuestion struct {
	QuestionID    int64
	Text          string
	Options       [4]string // A, B, C, D in the user's language
	CorrectAnswer string    // letter matching Options
	Topic         string
}

// ResolveTestForUser returns the questions of a test in the user's test
// language. For Russian users this is a plain pass-through. For Kazakh
// users the cached translation is used; missing translations are produced
// ONCE for the whole test (a single DeepSeek Flash call) and stored, so the
// next user — of any test containing the same questions — reuses them.
//
// (translated=true, err=nil) means every question has a Kazakh version;
// translated=false means the Russian master must be used as a fallback
// (translation unavailable or failed — the test must still open).
func (s *QuizService) ResolveTestForUser(ctx context.Context, user *models.User, testID int64) ([]ResolvedQuestion, bool, error) {
	questions, err := s.subjects.TestQuestions(ctx, testID)
	if err != nil {
		return nil, false, err
	}
	resolved := make([]ResolvedQuestion, 0, len(questions))
	if !s.wantsTranslation(ctx, user, testID) {
		for _, q := range questions {
			resolved = append(resolved, russianQuestion(&q))
		}
		return resolved, false, nil
	}

	tr, err := s.translator.TranslateTest(ctx, testID, questions)
	if err != nil {
		// Translation failed (API down, invalid reply) — the user must still
		// be able to take the test, in the Russian master version.
		log.Printf("translator fallback for test %d: %v", testID, err)
		for _, q := range questions {
			resolved = append(resolved, russianQuestion(&q))
		}
		return resolved, false, nil
	}
	// A partially translated test must never be served half Russian, half
	// Kazakh: when any question lacks a translation the WHOLE test falls
	// back to the Russian master and translated=false is reported.
	for _, q := range questions {
		if tr[q.ID] == nil {
			log.Printf("translator: test %d only partially translated (q%d missing) — Russian fallback", testID, q.ID)
			resolved = resolved[:0]
			for _, rq := range questions {
				resolved = append(resolved, russianQuestion(&rq))
			}
			return resolved, false, nil
		}
	}
	for _, q := range questions {
		t := tr[q.ID]
		resolved = append(resolved, ResolvedQuestion{
			QuestionID:    q.ID,
			Text:          t.Text,
			Options:       [4]string{t.OptionA, t.OptionB, t.OptionC, t.OptionD},
			CorrectAnswer: q.CorrectAnswer, // letter matches the translated options 1:1
			Topic:         t.Topic,
		})
	}
	return resolved, true, nil
}

// russianQuestion wraps the master (Russian) question row.
func russianQuestion(q *models.Question) ResolvedQuestion {
	return ResolvedQuestion{
		QuestionID:    q.ID,
		Text:          q.Text,
		Options:       [4]string{q.OptionA, q.OptionB, q.OptionC, q.OptionD},
		CorrectAnswer: q.CorrectAnswer,
		Topic:         q.Topic,
	}
}

// TestFullyTranslated reports whether the Kazakh version of the test is
// already COMPLETE in the cache. Pure DB lookup, zero API cost — the
// handler uses it to show the "⏳ Перевожу тест…" toast only when the
// first-ever translation of this test is actually about to run (every
// later Kazakh user opens the cached version instantly, no toast).
func (s *QuizService) TestFullyTranslated(ctx context.Context, testID int64) (bool, error) {
	questions, err := s.subjects.TestQuestions(ctx, testID)
	if err != nil {
		return false, err
	}
	if len(questions) == 0 {
		return false, fmt.Errorf("test %d has no questions", testID)
	}
	if s.translator == nil || !s.translator.Enabled() {
		return false, nil
	}
	ids := make([]int64, len(questions))
	for i, q := range questions {
		ids[i] = q.ID
	}
	n, err := s.translator.TranslatedCount(ctx, ids)
	if err != nil {
		return false, err
	}
	return n >= len(questions), nil
}

// EnsureTestTranslated makes sure a Kazakh version of the test exists when
// the user's test language is kk: the cached translation is used when it is
// already in the DB (ZERO DeepSeek calls — this is why the same test opened
// by the second, third, ... Kazakh user is completely free), otherwise the
// Russian master test is translated ONCE (a single DeepSeek Flash call) and
// stored for everyone. ready=true means the test was (or now is) fully
// available in the user's test language; ready=false means the Russian
// master is served (Russian users, translator disabled, or a translation
// failure — a missing translation must never block a test from opening).
func (s *QuizService) EnsureTestTranslated(ctx context.Context, user *models.User, testID int64) (ready bool, err error) {
	if !s.wantsTranslation(ctx, user, testID) {
		return false, nil
	}
	_, ok, err := s.ResolveTestForUser(ctx, user, testID)
	if err != nil {
		log.Printf("ensure test %d translated: %v", testID, err)
		return false, err
	}
	return ok, nil
}

// ---------------------------------------------------------------------------
// Language subjects are never translated
// ---------------------------------------------------------------------------

// subjectTranslatable reports whether the content of the subject may be
// machine-translated into the user's test language. LANGUAGE subjects
// («Русский язык», «Английский язык», «Казахский язык», литература …)
// teach the language itself: their questions and answer options ARE words
// and forms of that language, so a translation destroys the task (a Russian
// spelling question translated into Kazakh has no correct answer anymore).
// Such tests are always shown in their original language.
//
// On a lookup error the subject is treated as NOT translatable: showing the
// original is always correct, a wrong translation never is.
func (s *QuizService) subjectTranslatable(ctx context.Context, subjectID int64) bool {
	subject, err := s.subjects.GetByID(ctx, subjectID)
	if err != nil {
		log.Printf("subject %d lookup for translation check: %v", subjectID, err)
		return false
	}
	return !models.IsLanguageSubject(subject.Name)
}

// testTranslatable is subjectTranslatable for the subject of a test.
func (s *QuizService) testTranslatable(ctx context.Context, testID int64) bool {
	test, err := s.subjects.GetTest(ctx, testID)
	if err != nil {
		log.Printf("test %d lookup for translation check: %v", testID, err)
		return false
	}
	return s.subjectTranslatable(ctx, test.SubjectID)
}

// wantsTranslation reports whether the test must be served in Kazakh to this
// user: the user picked 🇰🇿, a translator is configured and the test does
// NOT belong to a language subject.
func (s *QuizService) wantsTranslation(ctx context.Context, user *models.User, testID int64) bool {
	if user == nil || user.TestLang != models.TestLangKK || s.translator == nil || !s.translator.Enabled() {
		return false
	}
	return s.testTranslatable(ctx, testID)
}

// TestNeedsTranslation is the handler-facing check: true only when opening
// this test should make sure its Kazakh version exists. Language subjects
// (Русский/Английский/Казахский язык, литература) always return false —
// they are shown in the original language and never sent to the translator.
func (s *QuizService) TestNeedsTranslation(ctx context.Context, user *models.User, testID int64) bool {
	return s.wantsTranslation(ctx, user, testID)
}

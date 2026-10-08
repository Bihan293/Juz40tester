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
	// boards caches the heavy per-subject leaderboards (P0-4).
	boards *leaderboardCache
	// metas caches TestViewMeta per attempt (R-2): the question count and
	// the subject name never change during an attempt, so they are loaded
	// once instead of on every question.
	metas *viewMetaCache
	// subjectNames caches the subject list for subjectTranslatable (A5).
	subjectNames *subjectNameCache
}

func NewQuizService(subjects *repositories.SubjectRepository, attempts *repositories.AttemptRepository, state *repositories.StateRepository, gen *repositories.GenerationRepository, genSvc *GeneratorService, users *repositories.UserRepository) *QuizService {
	s := &QuizService{subjects: subjects, attempts: attempts, state: state, gen: gen, genSvc: genSvc, rng: newSecureRand(),
		boards: newLeaderboardCache(LeaderboardCacheTTL), metas: newViewMetaCache(viewMetaTTL, viewMetaMax), users: users}
	s.subjectNames = newSubjectNameCache(subjectNamesTTL, subjects.List)
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

// WithSharedCache installs the cross-instance second-level cache of the
// leaderboards (CACHE_BACKEND=redis). nil = per-process cache only.
func (s *QuizService) WithSharedCache(c SharedCache) *QuizService {
	if s.boards != nil && c != nil {
		s.boards.shared = c
	}
	return s
}

// GetSubject returns a subject by id (leaderboard screen header).
func (s *QuizService) GetSubject(ctx context.Context, subjectID int64) (*models.Subject, error) {
	return s.subjects.GetByID(ctx, subjectID)
}

// GetTest returns a test by id.
func (s *QuizService) GetTest(ctx context.Context, testID int64) (*models.Test, error) {
	return s.subjects.GetTest(ctx, testID)
}

// ResumeOrNil returns the user's unfinished attempt for a test, or nil.
func (s *QuizService) ResumeOrNil(ctx context.Context, userID, testID int64) (*models.TestAttempt, error) {
	return s.attempts.GetActiveAttempt(ctx, userID, testID)
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
	UnlockedMax int // highest unlocked chain test number (access logic; may be MaxVisibleTests+1)
	// UnlockedShown is UnlockedMax capped at MaxVisibleTests — the number
	// shown to the user («Открыто тестов: N»). After the last test of the
	// chain is passed the watermark makes UnlockedMax = 201, which is not a
	// real test.
	UnlockedShown int
	MaxVisible    int // min(MaxVisibleTests, UnlockedMax + lookahead)
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
	n, _, err := s.unlockedWithProgress(ctx, userID, subjectID, chain)
	return n, err
}

// unlockedWithProgress is unlockedMax that also returns the knowledge
// progress it loaded, so callers that need both (the subject screen) do not
// query TestProgressForUser twice.
func (s *QuizService) unlockedWithProgress(ctx context.Context, userID, subjectID int64, chain []models.Test) (int, map[int64]*repositories.TestProgress, error) {
	ids := make([]int64, 0, len(chain))
	for i := range chain {
		ids = append(ids, chain[i].ID)
	}
	progress, err := s.gen.TestProgressForUser(ctx, userID, ids)
	if err != nil {
		return 0, nil, err
	}
	watermark := 0
	if s.state != nil {
		st, err := s.state.Get(ctx, userID, subjectID)
		if err != nil {
			return 0, nil, err
		}
		watermark = st.LastTestNumber
	}
	return computeUnlocked(chain, progress, watermark), progress, nil
}

// computeUnlocked is the pure unlock-chain walk shared by unlockedMax and
// the batched statistics screen (no I/O — see unlockedMax for the rules).
func computeUnlocked(chain []models.Test, progress map[int64]*repositories.TestProgress, watermark int) int {
	byNumber := make(map[int]*models.Test, len(chain))
	for i := range chain {
		byNumber[chain[i].TestNumber] = &chain[i]
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
	return unlocked
}

// maxChainNumber returns the highest TestNumber in the chain (0 if empty).
func maxChainNumber(chain []models.Test) int {
	mx := 0
	for i := range chain {
		if chain[i].TestNumber > mx {
			mx = chain[i].TestNumber
		}
	}
	return mx
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

	// One progress query serves both the unlock walk and the grid marks.
	unlockedMax, progress, err := s.unlockedWithProgress(ctx, userID, subjectID, chain)
	if err != nil {
		return nil, err
	}
	maxVisible := unlockedMax + 1 // show one step ahead (pending / generating)
	if maxVisible > models.MaxVisibleTests {
		maxVisible = models.MaxVisibleTests
	}
	// Never hide already-generated tests. The chain may have holes in its
	// numbering (e.g. 1,2,3,7,8 after a failed generation), so the bound is
	// the HIGHEST TestNumber, not len(chain).
	if mx := maxChainNumber(chain); maxVisible < mx {
		maxVisible = mx
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
				// Generation failures must never break the UI — log only.
				log.Printf("subject screen: ensure chain test subject %d #%d for user %d: %v",
					subjectID, unlockedMax, userID, err)
			}
		}
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
	// Unfinished attempts of the page's tests — ONE query for the whole page
	// instead of GetActiveAttempt per cell.
	pageIDs := make([]int64, 0, models.TestsPerPage)
	for n := start + 1; n <= end; n++ {
		if t := byNumber[n]; t != nil {
			pageIDs = append(pageIDs, t.ID)
		}
	}
	active, err := s.attempts.ActiveAttemptTests(ctx, userID, pageIDs)
	if err != nil {
		return nil, err
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
			slot.Resume = active[t.ID]
		} else if slot.Unlocked {
			slot.Pending = true
		}
		slots = append(slots, slot)
	}

	screen := &SubjectScreen{
		Subject:       subject,
		Slots:         slots,
		Page:          page,
		TotalPages:    totalPages,
		UnlockedMax:   unlockedMax,
		UnlockedShown: models.ClampVisibleTests(unlockedMax),
		MaxVisible:    maxVisible,
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
	// A5: the progress loaded for the unlock walk is reused for the
	// blocking test below (it is one of the chain tests).
	unlocked, progress, err := s.unlockedWithProgress(ctx, userID, test.SubjectID, chain)
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
func (s *QuizService) OnTestCompleted(ctx context.Context, userID int64, test *models.Test, green, yellow int) CompletionOutcome {
	var out CompletionOutcome
	if test.Kind != models.TestKindChain || s.state == nil {
		return out
	}
	// Not at the bar yet — nothing to unlock or generate. The user keeps
	// training this same test; generation starts exactly when the bar is
	// crossed.
	if !models.MeetsUnlockBar(green, yellow) {
		return out
	}
	// The bar is reached — remember it permanently (the unlock watermark):
	// a later retry with mistakes must never lock the next tests again.
	raised, err := s.state.RaiseWatermark(ctx, userID, test.SubjectID, test.TestNumber)
	if err != nil {
		log.Printf("test completed: save progress user %d subject %d test #%d: %v",
			userID, test.SubjectID, test.TestNumber, err)
		return out
	}
	out.NewUnlock = raised
	if s.genSvc == nil || !s.genSvc.Enabled() {
		return out
	}
	chain, err := s.subjects.ListChainTests(ctx, test.SubjectID)
	if err != nil {
		log.Printf("test completed: list chain tests subject %d (user %d): %v", test.SubjectID, userID, err)
		return out
	}
	byNumber := make(map[int]bool, len(chain))
	for _, t := range chain {
		byNumber[t.TestNumber] = true
	}
	// The next test of the chain (the one this unlock opened) does not
	// exist yet — generate it NOW, with the freshest knowledge marks.
	if next := test.TestNumber + 1; next <= models.MaxVisibleTests && !byNumber[next] {
		queued, err := s.genSvc.EnsureChainTest(ctx, test.SubjectID, next, true, userID)
		if err != nil {
			log.Printf("test completed: generate subject %d #%d for user %d: %v", test.SubjectID, next, userID, err)
		}
		out.NextGenerating = queued
	}
	// Opt-in (GEN_PREGEN_AHEAD=1): the test AFTER the next one is queued as
	// a NON-urgent job — it runs in the off-peak window (cheaper DeepSeek
	// fallback, no user waiting) and is upgraded to urgent automatically if
	// somebody unlocks it earlier (EnqueueChainJob's conflict update).
	if s.genSvc.PregenAhead() {
		if ahead := test.TestNumber + 2; ahead <= models.MaxVisibleTests && !byNumber[ahead] {
			if _, err := s.genSvc.EnsureChainTest(ctx, test.SubjectID, ahead, false, 0); err != nil {
				log.Printf("test completed: pre-generate subject %d #%d: %v", test.SubjectID, ahead, err)
			}
		}
	}
	return out
}

// CompletionOutcome is what OnTestCompleted did, for the result screen.
type CompletionOutcome struct {
	// NewUnlock: this pass raised the unlock watermark — the next chain
	// test was NOT open before (false for a retry of an earlier test).
	NewUnlock bool
	// NextGenerating: the next chain test does not exist yet and its
	// generation is queued or running.
	NextGenerating bool
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
	_, _ = s.ChainTestOrRevive(ctx, subjectID, testNumber, userID)
}

// ChainTestOrRevive is the ⏳ tap in ONE pass (A5): it returns chain test
// #testNumber when it already exists, otherwise revives/queues its
// generation (same unlock guard as before) and returns nil. It replaces
// ReviveChainTest + ChainTestStatus, which listed the chain twice, re-checked
// the test inside the generator and re-read the job status nobody used.
func (s *QuizService) ChainTestOrRevive(ctx context.Context, subjectID int64, testNumber int, userID int64) (*models.Test, error) {
	if testNumber < 1 {
		return nil, nil
	}
	chain, err := s.subjects.ListChainTests(ctx, subjectID)
	if err != nil {
		log.Printf("revive chain test: list subject %d: %v", subjectID, err)
		return nil, err
	}
	for i := range chain {
		if chain[i].TestNumber == testNumber {
			return &chain[i], nil
		}
	}
	// The chain list may be cached — a point query sees a test generated
	// a moment ago.
	if id, err := s.gen.ChainTestID(ctx, subjectID, testNumber); err != nil {
		return nil, err
	} else if id > 0 {
		return &models.Test{ID: id, SubjectID: subjectID, TestNumber: testNumber, Kind: models.TestKindChain, IsActive: true}, nil
	}
	if s.genSvc == nil {
		return nil, nil
	}
	unlocked, err := s.unlockedMax(ctx, userID, subjectID, chain)
	if err != nil {
		log.Printf("revive chain test: unlocked subject %d user %d: %v", subjectID, userID, err)
		return nil, err
	}
	if testNumber > unlocked {
		log.Printf("revive chain test: refused subject %d test %d for user %d (unlocked up to %d)",
			subjectID, testNumber, userID, unlocked)
		return nil, nil
	}
	s.genSvc.reviveChainTest(ctx, subjectID, testNumber, userID)
	return nil, nil
}

// WeakMenu returns, in ONE topic-stats query (+ the subject list and one
// personal-tests query), the subjects for the «🎯 Слабые темы» picker and
// their weak topics (worst first, at most limit each) (R-10a: no N+1).
//
// A subject is listed when it has weak topics OR the user already has a
// personal test there (withTest): answering the personal test improves its
// topics, so they often leave the weak list before the test is finished —
// the started (maybe paid) test must stay reachable from the menu.
func (s *QuizService) WeakMenu(ctx context.Context, userID int64, limit int) (subjects []models.Subject, weak map[int64][]models.TopicStat, withTest map[int64]bool, err error) {
	all, err := s.subjects.List(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	stats, err := s.gen.AllTopicStats(ctx, userID)
	if err != nil {
		return nil, nil, nil, err
	}
	ids := make([]int64, 0, len(all))
	for _, subj := range all {
		ids = append(ids, subj.ID)
	}
	withTest, err = s.gen.PersonalTestSubjects(ctx, userID, ids)
	if err != nil {
		return nil, nil, nil, err
	}
	weak = models.WeakTopicStatsBySubject(stats, limit)
	subjects = make([]models.Subject, 0, len(weak)+len(withTest))
	for _, subj := range all {
		if len(weak[subj.ID]) > 0 || withTest[subj.ID] {
			subjects = append(subjects, subj)
		}
	}
	return subjects, weak, withTest, nil
}

// ChainTestStatus reports whether chain test #number of the subject already
// exists (test != nil) or is still being generated (pending). Used by the
// «wait for the test» notifier: when both are empty the generation failed.
func (s *QuizService) ChainTestStatus(ctx context.Context, subjectID int64, number int) (test *models.Test, pending bool, err error) {
	// R-7: a point query instead of listing the whole chain — this runs in
	// the generation watchers' poll loop.
	id, err := s.gen.ChainTestID(ctx, subjectID, number)
	if err != nil {
		return nil, false, err
	}
	if id > 0 {
		return &models.Test{ID: id, SubjectID: subjectID, TestNumber: number, Kind: models.TestKindChain, IsActive: true}, false, nil
	}
	pending, err = s.gen.HasPendingOrRunningChainJob(ctx, subjectID, number)
	return nil, pending, err
}

// ActivePersonalJobID returns the pending/running personal generation job
// of (subject, user), 0 when none (R-7: the watcher key is job:<id>).
func (s *QuizService) ActivePersonalJobID(ctx context.Context, userID, subjectID int64) (int64, error) {
	return s.gen.ActivePersonalJobID(ctx, subjectID, userID)
}

// JobState reports the test produced by a generation job (0 until done)
// and whether the job is still pending/running (R-7).
func (s *QuizService) JobState(ctx context.Context, jobID int64) (testID int64, pending bool, err error) {
	return s.gen.JobState(ctx, jobID)
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
		return s.existingPersonalTest(ctx, userID, subjectID)
	}
	test, pending, topics, err = s.genSvc.EnsurePersonalTest(ctx, userID, subjectID)
	if err == nil && test == nil && !pending && len(topics) == 0 {
		// No weak topics left (often BECAUSE of this very personal test —
		// its answers improve the topic statistics): a test built earlier
		// must still open, otherwise a half-done (or paid) test becomes
		// unreachable.
		return s.existingPersonalTest(ctx, userID, subjectID)
	}
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

// existingPersonalTest returns the user's personal test of the subject as
// EnsurePersonalTest does (topics = the topics the test was built for), or
// nothing when there is none. Nothing is generated or deleted.
func (s *QuizService) existingPersonalTest(ctx context.Context, userID, subjectID int64) (*models.Test, bool, []string, error) {
	test, err := s.gen.FindPersonalTest(ctx, subjectID, userID)
	if err != nil || test == nil {
		return nil, false, nil, err
	}
	return test, false, test.Topics, nil
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

// RestartTest starts a brand-new attempt («🔄 Пройти ещё раз»): any attempt
// of this test that is still in progress is closed as abandoned first, so
// a double tap never leaves two open attempts.
func (s *QuizService) RestartTest(ctx context.Context, userID, testID int64) (*models.TestAttempt, error) {
	return s.startTest(ctx, userID, testID, true)
}

// StartTestFor is StartTest for a test row the caller has already loaded
// (openTest) — saves re-reading the same tests row (R-2). The caller has
// just checked ResumeOrNil and found no active attempt, so the attempt is
// created without looking the active one up again (A5); a concurrent
// creation still resolves to the single winner via the unique index.
func (s *QuizService) StartTestFor(ctx context.Context, userID int64, test *models.Test) (*models.TestAttempt, error) {
	return s.startTestRowMode(ctx, userID, test, false, true)
}

func (s *QuizService) startTest(ctx context.Context, userID, testID int64, replace bool) (*models.TestAttempt, error) {
	test, err := s.subjects.GetTest(ctx, testID)
	if err != nil {
		return nil, err
	}
	return s.startTestRow(ctx, userID, test, replace)
}

func (s *QuizService) startTestRow(ctx context.Context, userID int64, test *models.Test, replace bool) (*models.TestAttempt, error) {
	return s.startTestRowMode(ctx, userID, test, replace, false)
}

func (s *QuizService) startTestRowMode(ctx context.Context, userID int64, test *models.Test, replace, fresh bool) (*models.TestAttempt, error) {
	if test == nil || !test.IsActive {
		return nil, repositories.ErrNotFound
	}
	// A5: only the ids are needed here (loaded once, no question text).
	ids, err := s.subjects.TestQuestionIDs(ctx, test.ID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("test %d has no questions", test.ID)
	}

	// Shuffle the question order for this attempt — under the mutex (the
	// RNG source is shared across all concurrent webhook goroutines).
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
	if fresh && !replace {
		return s.attempts.CreateFreshAttempt(ctx, userID, test.ID, ids, orders)
	}
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
	return s.loadView(ctx, attemptID, user, 0)
}

// QuestionAtPosition loads the question at a fixed position of the attempt.
func (s *QuizService) QuestionAtPosition(ctx context.Context, attemptID int64, user *models.User, position int) (*QuestionView, error) {
	if position <= 0 {
		return nil, repositories.ErrNotFound
	}
	return s.loadView(ctx, attemptID, user, position)
}

// loadView renders one question of an attempt with as few round trips as
// possible (R-2). Before: GetAttemptForUser + attempt_questions + questions
// + TestViewMeta + translation lookup = 5 queries per question. Now:
//   - ONE query (LoadQuestionView) loads the attempt with the ownership
//     check, the attempt question JOINed with its question row and, for
//     Kazakh users, the cached translation of that question;
//   - TestViewMeta is fetched in the same query only the first time for
//     the attempt and then served from metas (it is immutable during the
//     attempt — see viewMetaCache for the one exception, Translated).
//
// position 0 = first unanswered question; returns (nil, nil) when nothing
// is left. A fixed position that does not exist is ErrNotFound.
func (s *QuizService) loadView(ctx context.Context, attemptID int64, user *models.User, position int) (*QuestionView, error) {
	if user == nil {
		return nil, repositories.ErrNotFound
	}
	wantKK := user.TestLang == models.TestLangKK && s.translator != nil && s.translator.Enabled()
	meta, cached := s.metas.get(attemptID)
	// An incomplete Kazakh translation may complete while the attempt is
	// running — for Kazakh users the meta is re-read until it is complete.
	if cached && wantKK && !metaComplete(meta) && !models.IsLanguageSubject(meta.SubjectName) {
		cached = false
	}
	lang := ""
	if wantKK {
		lang = models.TestLangKK
	}
	row, err := s.attempts.LoadQuestionView(ctx, attemptID, user.ID, position, !cached, lang)
	if err != nil {
		return nil, err
	}
	if !cached {
		meta = row.Meta
		s.metas.put(attemptID, row.Attempt.TestID, meta)
	}
	if row.AQ == nil {
		if position > 0 {
			return nil, repositories.ErrNotFound
		}
		return nil, nil // nothing left to answer
	}
	return s.renderView(user, row.Attempt, row.AQ, row.Question, meta, row.Translation), nil
}

// renderView builds the view from already-loaded rows (no DB access).
func (s *QuizService) renderView(user *models.User, attempt *models.TestAttempt, aq *models.AttemptQuestion, q *models.Question, meta *repositories.TestViewMeta, tr *models.QuestionTranslation) *QuestionView {
	total := meta.Total
	// The Russian master row is the default; Kazakh users get the cached
	// translation of THIS question (written once per question at test open).
	text := q.Text
	byLabel := map[string]string{
		"A": q.OptionA, "B": q.OptionB, "C": q.OptionC, "D": q.OptionD,
	}
	if user != nil && user.TestLang == models.TestLangKK && s.translator != nil && s.translator.Enabled() &&
		!models.IsLanguageSubject(meta.SubjectName) && total > 0 && meta.Translated >= total && tr != nil {
		// Only a COMPLETE translation is used — a partially translated test
		// is shown entirely in Russian instead of a per-question mix.
		text = tr.Text
		byLabel = map[string]string{
			"A": tr.OptionA, "B": tr.OptionB, "C": tr.OptionC, "D": tr.OptionD,
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
	}
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
// R-2: one query (LoadSummary) instead of five; the result is identical.
func (s *QuizService) BuildSummary(ctx context.Context, attemptID, userID int64) (*AttemptSummary, error) {
	row, err := s.attempts.LoadSummary(ctx, attemptID, userID)
	if err != nil {
		return nil, err
	}
	if row.Attempt.Status != models.AttemptInProgress {
		s.metas.drop(attemptID) // the attempt is over — its meta is not needed anymore
	}
	return &AttemptSummary{
		Attempt:      row.Attempt,
		Test:         row.Test,
		Subject:      row.Subject,
		Total:        row.Total,
		StatusCounts: row.StatusCounts,
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
// chain tests ("уровни"). The result is cached per subject for
// LeaderboardCacheTTL — the underlying recursive aggregation scans every
// user's progress in the subject.
func (s *QuizService) UnlockedTestsLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	if s.boards == nil {
		return s.subjects.UnlockedTestsLeaderboard(ctx, subjectID, limit)
	}
	return s.boards.get(ctx, leaderboardKey{boardUnlocked, subjectID, limit}, func(ctx context.Context) ([]models.LeaderboardEntry, error) {
		return s.subjects.UnlockedTestsLeaderboard(ctx, subjectID, limit)
	})
}

// GreenLeaderboard returns the top users of a subject by mastered (🟢)
// questions. Cached per subject for LeaderboardCacheTTL.
func (s *QuizService) GreenLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	if s.boards == nil {
		return s.subjects.GreenLeaderboard(ctx, subjectID, limit)
	}
	return s.boards.get(ctx, leaderboardKey{boardGreen, subjectID, limit}, func(ctx context.Context) ([]models.LeaderboardEntry, error) {
		return s.subjects.GreenLeaderboard(ctx, subjectID, limit)
	})
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

// AllSubjectsProgress is SubjectProgress for every given subject in a fixed
// number of queries (all chain tests, one knowledge-progress query, all
// unlock watermarks, one aggregate) instead of ~4 queries per subject. The
// result is identical to calling SubjectProgress for each subject.
func (s *QuizService) AllSubjectsProgress(ctx context.Context, userID int64, subjectIDs []int64) (map[int64]*models.SubjectProgress, error) {
	if len(subjectIDs) == 0 {
		return map[int64]*models.SubjectProgress{}, nil
	}
	chains, err := s.subjects.ListChainTestsBySubject(ctx, subjectIDs)
	if err != nil {
		return nil, err
	}
	var allIDs []int64
	for _, chain := range chains {
		for _, t := range chain {
			allIDs = append(allIDs, t.ID)
		}
	}
	progress, err := s.gen.TestProgressForUser(ctx, userID, allIDs)
	if err != nil {
		return nil, err
	}
	watermarks := map[int64]int{}
	if s.state != nil {
		if watermarks, err = s.state.Watermarks(ctx, userID); err != nil {
			return nil, err
		}
	}
	open := make(map[int64][]int64, len(subjectIDs))
	for _, sid := range subjectIDs {
		chain := chains[sid]
		unlocked := computeUnlocked(chain, progress, watermarks[sid])
		ids := make([]int64, 0, len(chain))
		for _, t := range chain {
			if t.TestNumber <= unlocked {
				ids = append(ids, t.ID)
			}
		}
		open[sid] = ids
	}
	return s.subjects.SubjectProgressBatch(ctx, userID, subjectIDs, open)
}

// ---------------------------------------------------------------------------
// Test content language (🇷🇺/🇰🇿 — the interface itself always stays Russian)
// ---------------------------------------------------------------------------

// TestFullyTranslated reports whether the Kazakh version of the test is
// already COMPLETE in the cache. Pure DB lookup, zero API cost — only an
// incomplete translation queues a background job (PrepareTranslation).
func (s *QuizService) TestFullyTranslated(ctx context.Context, testID int64) (bool, error) {
	// A5: only the question ids are loaded, not the full texts.
	ids, err := s.subjects.TestQuestionIDs(ctx, testID)
	if err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, fmt.Errorf("test %d has no questions", testID)
	}
	if s.translator == nil || !s.translator.Enabled() {
		return false, nil
	}
	n, err := s.translator.TranslatedCount(ctx, ids)
	if err != nil {
		return false, err
	}
	return n >= len(ids), nil
}

// PrepareTranslation makes sure the Kazakh version of the test exists or is
// being produced — WITHOUT translating anything in the caller's goroutine
// (R-4: an update handler must never wait for a model call).
//
//   - ready = true: the cached translation is complete — open the test now
//     (ZERO API calls; every later Kazakh user of a test is free);
//   - wait != nil: a background translation job has been queued (or was
//     already queued/running — UNIQUE per test and language, so a test is
//     never translated twice) and the channel delivers its outcome exactly
//     once; the caller shows «⏳ Перевод готовится…» and returns at once;
//   - ready = false, wait = nil: no translation is wanted (Russian user,
//     language subject, translator disabled) or the request failed (err;
//     ErrTranslationUnavailable when the translation failed recently) —
//     the test opens in the Russian master version.
func (s *QuizService) PrepareTranslation(ctx context.Context, user *models.User, test *models.Test) (ready bool, wait <-chan TranslationOutcome, err error) {
	if !s.TestNeedsTranslationFor(ctx, user, test) {
		return false, nil, nil
	}
	done, err := s.TestFullyTranslated(ctx, test.ID)
	if err != nil {
		return false, nil, err
	}
	if done {
		return true, nil, nil
	}
	// Subscribe BEFORE enqueuing: a local worker may finish the job between
	// the two calls, and its publish must not be missed.
	wait = s.translator.AwaitTranslation(test.ID)
	if err := s.translator.RequestTranslation(ctx, test.ID); err != nil {
		s.translator.CancelAwait(test.ID, wait)
		return false, nil, err
	}
	return false, wait, nil
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
//
// A5: the subject names come from an in-memory cache of the subject list
// (TTL subjectNamesTTL) instead of one DB read per call.
func (s *QuizService) subjectTranslatable(ctx context.Context, subjectID int64) bool {
	name, err := s.subjectNames.name(ctx, subjectID)
	if err != nil {
		log.Printf("subject %d lookup for translation check: %v", subjectID, err)
		return false
	}
	return !models.IsLanguageSubject(name)
}

// TestNeedsTranslationFor is TestNeedsTranslation for an already loaded
// test row: it skips re-reading the tests row (R-2, test open path).
func (s *QuizService) TestNeedsTranslationFor(ctx context.Context, user *models.User, test *models.Test) bool {
	if test == nil || user == nil || user.TestLang != models.TestLangKK || s.translator == nil || !s.translator.Enabled() {
		return false
	}
	return s.subjectTranslatable(ctx, test.SubjectID)
}

// PersonalTestState is the read-only look at the user's weak-topics test of
// the subject used by the PAID weak-topics flow (nothing is generated or
// deleted here): the current weak topics, the existing personal test and
// whether it is stale (none of its topics is weak anymore).
func (s *QuizService) PersonalTestState(ctx context.Context, userID, subjectID int64) (test *models.Test, stale bool, topics []string, err error) {
	weak, err := s.gen.WeakTopicStats(ctx, userID, subjectID, weakTopicsCount)
	if err != nil {
		return nil, false, nil, err
	}
	topics = make([]string, len(weak))
	norm := make(map[string]bool, len(weak))
	for i, w := range weak {
		topics[i] = w.Topic
		norm[models.NormalizeTopic(w.Topic)] = true
	}
	test, err = s.gen.FindPersonalTest(ctx, subjectID, userID)
	if err != nil || test == nil {
		return nil, false, topics, err
	}
	stale = true
	for _, t := range test.Topics {
		if norm[models.NormalizeTopic(t)] {
			stale = false
			break
		}
	}
	return test, stale, topics, nil
}

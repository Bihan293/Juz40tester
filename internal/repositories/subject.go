package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// SubjectRepository handles subjects, tests and questions lookups.
type SubjectRepository struct {
	pool *pgxpool.Pool
}

func NewSubjectRepository(pool *pgxpool.Pool) *SubjectRepository {
	return &SubjectRepository{pool: pool}
}

// List returns all subjects ordered by position.
func (r *SubjectRepository) List(ctx context.Context) ([]models.Subject, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, name, position FROM subjects ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Subject
	for rows.Next() {
		var s models.Subject
		if err := rows.Scan(&s.ID, &s.Name, &s.Position); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetByID fetches a subject by id.
func (r *SubjectRepository) GetByID(ctx context.Context, id int64) (*models.Subject, error) {
	var s models.Subject
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, position FROM subjects WHERE id = $1`, id).
		Scan(&s.ID, &s.Name, &s.Position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ListChainTests returns only the chain tests of a subject (the main linear
// sequence), ordered by test number. 'weak' tests are excluded.
// Served from the in-memory chain cache when enabled (R-10a).
func (r *SubjectRepository) ListChainTests(ctx context.Context, subjectID int64) ([]models.Test, error) {
	if tests, ok := chainCache.get(subjectID); ok {
		return tests, nil
	}
	tests, err := r.listTests(ctx, subjectID, models.TestKindChain)
	if err != nil {
		return nil, err
	}
	chainCache.put(subjectID, tests)
	return tests, nil
}

func (r *SubjectRepository) listTests(ctx context.Context, subjectID int64, kind string) ([]models.Test, error) {
	query := `
		SELECT id, subject_id, test_number, title, is_active, kind, topics
		FROM tests
		WHERE subject_id = $1 AND is_active`
	args := []any{subjectID}
	if kind != "" {
		query += ` AND kind = $2`
		args = append(args, kind)
	}
	query += ` ORDER BY test_number`

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Test
	for rows.Next() {
		var t models.Test
		var topicsJSON []byte
		if err := rows.Scan(&t.ID, &t.SubjectID, &t.TestNumber, &t.Title, &t.IsActive, &t.Kind, &topicsJSON); err != nil {
			return nil, err
		}
		if len(topicsJSON) > 0 {
			_ = json.Unmarshal(topicsJSON, &t.Topics)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTest returns a test by id.
func (r *SubjectRepository) GetTest(ctx context.Context, id int64) (*models.Test, error) {
	var t models.Test
	var topicsJSON []byte
	var owner sql.NullInt64
	err := r.pool.QueryRow(ctx, `
		SELECT id, subject_id, test_number, title, is_active, kind, topics, owner_user_id
		FROM tests WHERE id = $1`, id).
		Scan(&t.ID, &t.SubjectID, &t.TestNumber, &t.Title, &t.IsActive, &t.Kind, &topicsJSON, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(topicsJSON) > 0 {
		_ = json.Unmarshal(topicsJSON, &t.Topics)
	}
	t.OwnerUserID = owner.Int64
	return &t, nil
}

// TestQuestions returns questions of a test ordered by their fixed position.
func (r *SubjectRepository) TestQuestions(ctx context.Context, testID int64) ([]models.Question, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT q.id, q.subject_id, q.question_text, q.option_a, q.option_b,
		       q.option_c, q.option_d, q.correct_answer, q.topic, q.difficulty
		FROM test_questions tq
		JOIN questions q ON q.id = tq.question_id
		WHERE tq.test_id = $1
		ORDER BY tq.position`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Question
	for rows.Next() {
		var q models.Question
		if err := rows.Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB,
			&q.OptionC, &q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// AverageQuestionStatuses returns, per question, the average knowledge
// status (0..2) over ALL users who answered it. Questions nobody answered
// are absent from the map. Used to show the generator how the whole
// audience did on a SHARED chain test.
func (r *SubjectRepository) AverageQuestionStatuses(ctx context.Context, questionIDs []int64) (map[int64]float64, error) {
	out := make(map[int64]float64, len(questionIDs))
	if len(questionIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT question_id, AVG(status)::float8
		FROM user_question_progress
		WHERE question_id = ANY($1)
		GROUP BY question_id`, questionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var qid int64
		var avg float64
		if err := rows.Scan(&qid, &avg); err != nil {
			return nil, err
		}
		out[qid] = avg
	}
	return out, rows.Err()
}

// SubjectProgress aggregates knowledge statistics of a single subject for a
// user over an EXPLICIT list of tests (testIDs) — the tests the user can
// actually open right now. Locked chain tests are decided by the caller (the
// quiz service computes the unlock chain) and are NOT passed here, so a
// still-closed test never inflates the totals: a user with one open
// 20-question test sees exactly 20 questions, not 40 because a second,
// locked test already exists in the database.
//
// Questions shared by several of the given tests are not double-counted
// (COUNT DISTINCT). Personal weak-topics tests are never part of the list
// (they are per-user training, not part of the course).
// Returns ErrNotFound if the subject does not exist.
func (r *SubjectRepository) SubjectProgress(ctx context.Context, userID, subjectID int64, testIDs []int64) (*models.SubjectProgress, error) {
	var sp models.SubjectProgress
	err := r.pool.QueryRow(ctx, `
		SELECT s.id, s.name,
		       COALESCE(stat.total_questions, 0), COALESCE(stat.green, 0),
		       COALESCE(stat.yellow, 0), COALESCE(stat.red, 0),
		       COALESCE(stat.correct_count, 0), COALESCE(stat.wrong_count, 0)
		FROM subjects s
		LEFT JOIN LATERAL (
		    SELECT COUNT(DISTINCT tq.question_id)                                 AS total_questions,
		           COUNT(DISTINCT tq.question_id) FILTER (WHERE p.status = 2)     AS green,
		           COUNT(DISTINCT tq.question_id) FILTER (WHERE p.status = 1)     AS yellow,
		           COUNT(DISTINCT tq.question_id) FILTER (WHERE COALESCE(p.status, 0) = 0) AS red,
		           COALESCE(SUM(p.correct_count), 0)                              AS correct_count,
		           COALESCE(SUM(p.wrong_count), 0)                                AS wrong_count
		    FROM test_questions tq
		    LEFT JOIN user_question_progress p
		           ON p.question_id = tq.question_id AND p.user_id = $1
		    WHERE tq.test_id = ANY($3)
		) stat ON TRUE
		WHERE s.id = $2`, userID, subjectID, testIDs).
		Scan(&sp.SubjectID, &sp.SubjectName, &sp.TotalQuestions,
			&sp.Green, &sp.Yellow, &sp.Red, &sp.CorrectCount, &sp.WrongCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// SubjectProgressBatch is SubjectProgress for many subjects in ONE query:
// openTests[subjectID] is the explicit list of tests counted for that
// subject (same semantics as SubjectProgress — COUNT DISTINCT per subject,
// locked tests excluded by the caller). Subjects that do not exist are
// absent from the result.
func (r *SubjectRepository) SubjectProgressBatch(ctx context.Context, userID int64, subjectIDs []int64, openTests map[int64][]int64) (map[int64]*models.SubjectProgress, error) {
	out := make(map[int64]*models.SubjectProgress, len(subjectIDs))
	if len(subjectIDs) == 0 {
		return out, nil
	}
	// Flatten (subject, test) pairs; the test list is scoped per subject so
	// a test id can never leak into another subject's totals.
	var pairSubj, pairTest []int64
	for _, sid := range subjectIDs {
		for _, tid := range openTests[sid] {
			pairSubj = append(pairSubj, sid)
			pairTest = append(pairTest, tid)
		}
	}
	rows, err := r.pool.Query(ctx, `
		WITH open_tests AS (
		    SELECT * FROM unnest($3::bigint[], $4::bigint[]) AS o(subject_id, test_id)
		),
		stat AS (
		    SELECT o.subject_id,
		           COUNT(DISTINCT tq.question_id)                                 AS total_questions,
		           COUNT(DISTINCT tq.question_id) FILTER (WHERE p.status = 2)     AS green,
		           COUNT(DISTINCT tq.question_id) FILTER (WHERE p.status = 1)     AS yellow,
		           COUNT(DISTINCT tq.question_id) FILTER (WHERE COALESCE(p.status, 0) = 0) AS red,
		           COALESCE(SUM(p.correct_count), 0)                              AS correct_count,
		           COALESCE(SUM(p.wrong_count), 0)                                AS wrong_count
		    FROM open_tests o
		    JOIN test_questions tq ON tq.test_id = o.test_id
		    LEFT JOIN user_question_progress p
		           ON p.question_id = tq.question_id AND p.user_id = $1
		    GROUP BY o.subject_id
		)
		SELECT s.id, s.name,
		       COALESCE(st.total_questions, 0), COALESCE(st.green, 0),
		       COALESCE(st.yellow, 0), COALESCE(st.red, 0),
		       COALESCE(st.correct_count, 0), COALESCE(st.wrong_count, 0)
		FROM subjects s
		LEFT JOIN stat st ON st.subject_id = s.id
		WHERE s.id = ANY($2)`, userID, subjectIDs, pairSubj, pairTest)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sp models.SubjectProgress
		if err := rows.Scan(&sp.SubjectID, &sp.SubjectName, &sp.TotalQuestions,
			&sp.Green, &sp.Yellow, &sp.Red, &sp.CorrectCount, &sp.WrongCount); err != nil {
			return nil, err
		}
		out[sp.SubjectID] = &sp
	}
	return out, rows.Err()
}

// ListChainTestsBySubject is ListChainTests for many subjects in one query
// (same filter and per-subject order by test number).
func (r *SubjectRepository) ListChainTestsBySubject(ctx context.Context, subjectIDs []int64) (map[int64][]models.Test, error) {
	out := make(map[int64][]models.Test, len(subjectIDs))
	if len(subjectIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, subject_id, test_number, title, is_active, kind, topics
		FROM tests
		WHERE subject_id = ANY($1) AND is_active AND kind = $2
		ORDER BY subject_id, test_number`, subjectIDs, models.TestKindChain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t models.Test
		var topicsJSON []byte
		if err := rows.Scan(&t.ID, &t.SubjectID, &t.TestNumber, &t.Title, &t.IsActive, &t.Kind, &topicsJSON); err != nil {
			return nil, err
		}
		if len(topicsJSON) > 0 {
			_ = json.Unmarshal(topicsJSON, &t.Topics)
		}
		out[t.SubjectID] = append(out[t.SubjectID], t)
	}
	return out, rows.Err()
}

// TestViewMeta is what rendering one question needs about its test.
type TestViewMeta struct {
	Total       int    // questions attached to the test
	SubjectName string // subject of the test (language subjects are never translated)
	Translated  int    // test questions that have a cached translation in lang
}

// UnlockedTestsLeaderboard ranks users by their chain LEVEL in the subject
// — exactly the value QuizService.unlockedMax computes for the bot itself:
// Тест 1 is always open; walking the chain by test number, test n+1 opens
// while test n exists and is either within the permanent watermark
// (user_subject_state.last_test_number) or currently meets the unlock bar
// (UnlockGreen 🟢 + UnlockYellow 🟡). The walk stops at the first locked
// or missing test; finally the level is at least last_test_number + 1.
// An open-but-not-started test counts too (the previous implementation only
// looked at tests the user had already answered something in, and did not
// stop at the first locked test of the chain).
func (r *SubjectRepository) UnlockedTestsLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	rows, err := r.pool.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, test_number FROM tests
			WHERE subject_id = $1 AND is_active AND kind = 'chain'
		),
		prog AS (
			SELECT tq.test_id, p.user_id,
			       COUNT(*) FILTER (WHERE p.status = 2) AS green,
			       COUNT(*) FILTER (WHERE p.status = 1) AS yellow
			FROM test_questions tq
			JOIN chain c ON c.id = tq.test_id
			JOIN user_question_progress p ON p.question_id = tq.question_id
			GROUP BY tq.test_id, p.user_id
		),
		players AS (
			SELECT DISTINCT user_id FROM prog
			UNION
			SELECT user_id FROM user_subject_state
			WHERE subject_id = $1 AND last_test_number > 0
		),
		wm AS (
			SELECT pl.user_id, COALESCE(uss.last_test_number, 0) AS watermark
			FROM players pl
			LEFT JOIN user_subject_state uss
			       ON uss.user_id = pl.user_id AND uss.subject_id = $1
		),
		walk AS (
			SELECT w.user_id, w.watermark, 1 AS n FROM wm w
			UNION ALL
			SELECT wk.user_id, wk.watermark, wk.n + 1
			FROM walk wk
			JOIN chain c ON c.test_number = wk.n
			LEFT JOIN prog pp ON pp.test_id = c.id AND pp.user_id = wk.user_id
			WHERE wk.n < 100000
			  AND (wk.n <= wk.watermark
			       OR (COALESCE(pp.green, 0) >= $3
			           AND COALESCE(pp.green, 0) + COALESCE(pp.yellow, 0) >= $3 + $4))
		),
		levels AS (
			SELECT user_id, GREATEST(MAX(n), MAX(watermark) + 1) AS level
			FROM walk
			GROUP BY user_id
		)
		SELECT u.id, u.first_name, u.username, `+liveStreakSQL("$5")+`,
		       l.level AS unlocked_tests
		FROM levels l
		JOIN users u ON u.id = l.user_id
		ORDER BY unlocked_tests DESC, u.id
		LIMIT $2`, subjectID, limit, models.UnlockGreen, models.UnlockYellow, models.StreakToday(time.Now()).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.LeaderboardEntry
	for rows.Next() {
		var e models.LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.FirstName, &e.Username, &e.StreakDays, &e.UnlockedTests); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GreenLeaderboard ranks users by mastered (🟢) questions of the subject.
// Questions shared by several tests of the subject are counted once
// (COUNT DISTINCT), so generated duplicates cannot inflate the score.
func (r *SubjectRepository) GreenLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT u.id, u.first_name, u.username, `+liveStreakSQL("$3")+`,
		       COUNT(DISTINCT tq.question_id) AS green
		FROM user_question_progress p
		JOIN questions q ON q.id = p.question_id AND q.subject_id = $1
		JOIN test_questions tq ON tq.question_id = q.id
		JOIN tests t ON t.id = tq.test_id AND t.is_active AND t.kind = 'chain'
		JOIN users u ON u.id = p.user_id
		WHERE p.status = 2
		GROUP BY u.id, u.first_name, u.username, u.streak_days, u.last_active_date
		ORDER BY green DESC, u.id
		LIMIT $2`, subjectID, limit, models.StreakToday(time.Now()).Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.LeaderboardEntry
	for rows.Next() {
		var e models.LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.FirstName, &e.Username, &e.StreakDays, &e.Green); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

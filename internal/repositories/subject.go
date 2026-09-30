package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40-test2/internal/models"
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

// ListTests returns all active tests of a subject ordered by test number.
func (r *SubjectRepository) ListTests(ctx context.Context, subjectID int64) ([]models.Test, error) {
	return r.listTests(ctx, subjectID, "")
}

// ListChainTests returns only the chain tests of a subject (the main linear
// sequence), ordered by test number. 'weak' tests are excluded.
func (r *SubjectRepository) ListChainTests(ctx context.Context, subjectID int64) ([]models.Test, error) {
	return r.listTests(ctx, subjectID, models.TestKindChain)
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

// TestQuestionCount returns how many questions are attached to a test.
func (r *SubjectRepository) TestQuestionCount(ctx context.Context, testID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM test_questions WHERE test_id = $1`, testID).Scan(&n)
	return n, err
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

// GetQuestion returns a single question by id.
func (r *SubjectRepository) GetQuestion(ctx context.Context, id int64) (*models.Question, error) {
	var q models.Question
	err := r.pool.QueryRow(ctx, `
		SELECT id, subject_id, question_text, option_a, option_b, option_c,
		       option_d, correct_answer, topic, difficulty
		FROM questions WHERE id = $1`, id).
		Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB, &q.OptionC,
			&q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// QuestionStatuses returns knowledge statuses of the user for given questions.
func (r *SubjectRepository) QuestionStatuses(ctx context.Context, userID int64, questionIDs []int64) (map[int64]int, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT question_id, status FROM user_question_progress
		WHERE user_id = $1 AND question_id = ANY($2)`, userID, questionIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]int, len(questionIDs))
	for rows.Next() {
		var qid int64
		var status int
		if err := rows.Scan(&qid, &status); err != nil {
			return nil, err
		}
		out[qid] = status
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

// UnlockedTestsLeaderboard ranks users by how many CHAIN tests of the
// subject they have unlocked. A test counts as unlocked for a user when it
// is test 1 or when the previous test of the chain reached the unlock bar
// (UnlockGreen 🟢 + UnlockYellow 🟡). Only tests that already exist are
// considered, so the board never counts phantom slots.
func (r *SubjectRepository) UnlockedTestsLeaderboard(ctx context.Context, subjectID int64, limit int) ([]models.LeaderboardEntry, error) {
	rows, err := r.pool.Query(ctx, `
		WITH chain AS (
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
		unlocked AS (
			SELECT p.user_id, c.test_number,
			       (c.test_number = 1) OR EXISTS (
			           SELECT 1 FROM chain pc
			           JOIN prog pp ON pp.test_id = pc.id AND pp.user_id = p.user_id
			           WHERE pc.test_number = c.test_number - 1
			             AND pp.green >= $3 AND pp.yellow >= $4
			       ) AS is_unlocked
			FROM prog p
			JOIN chain c ON c.id = p.test_id
		)
		SELECT u.id, u.first_name, u.username, u.streak_days,
		       COUNT(*) FILTER (WHERE un.is_unlocked) AS unlocked_tests
		FROM unlocked un
		JOIN users u ON u.id = un.user_id
		GROUP BY u.id, u.first_name, u.username, u.streak_days
		ORDER BY unlocked_tests DESC, u.id
		LIMIT $2`, subjectID, limit, models.UnlockGreen, models.UnlockYellow)
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
		SELECT u.id, u.first_name, u.username, u.streak_days,
		       COUNT(DISTINCT tq.question_id) AS green
		FROM user_question_progress p
		JOIN questions q ON q.id = p.question_id AND q.subject_id = $1
		JOIN test_questions tq ON tq.question_id = q.id
		JOIN tests t ON t.id = tq.test_id AND t.is_active
		JOIN users u ON u.id = p.user_id
		WHERE p.status = 2
		GROUP BY u.id, u.first_name, u.username, u.streak_days
		ORDER BY green DESC, u.id
		LIMIT $2`, subjectID, limit)
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

// EnsureSubject makes sure a subject with the given name exists and returns
// its id. Subjects are managed in the database directly, but this helper
// stays for operational scripts/tests. Idempotent.
func (r *SubjectRepository) EnsureSubject(ctx context.Context, name string) (int64, error) {
	var subjectID int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO subjects (name, position)
		VALUES ($1, (SELECT COALESCE(MAX(position), 0) + 1 FROM subjects))
		ON CONFLICT (name) DO NOTHING
		RETURNING id`, name).Scan(&subjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Subject already exists — reuse it.
		if err := r.pool.QueryRow(ctx,
			`SELECT id FROM subjects WHERE name = $1`, name).Scan(&subjectID); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	return subjectID, nil
}

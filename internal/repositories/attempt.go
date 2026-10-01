package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// AnswerResult is returned by SubmitAnswer.
type AnswerResult struct {
	Question        *models.Question
	Attempt         *models.TestAttempt
	SelectedAnswer  string // original label chosen by user
	Correct         bool
	Finished        bool // attempt was completed by this answer
	AlreadyAnswered bool
	NewStatus       int // knowledge status of the question after this answer (0=🔴,1=🟡,2=🟢)
}

// AttemptRepository manages test attempts, answers and knowledge progress.
type AttemptRepository struct {
	pool *pgxpool.Pool
}

func NewAttemptRepository(pool *pgxpool.Pool) *AttemptRepository {
	return &AttemptRepository{pool: pool}
}

// CreateAttempt atomically creates a new attempt with shuffled questions and
// shuffled answer options for every question.
func (r *AttemptRepository) CreateAttempt(ctx context.Context, userID, testID int64, questionIDs []int64, optionOrders [][]string) (*models.TestAttempt, error) {
	if len(questionIDs) != len(optionOrders) {
		return nil, errors.New("questions and option orders length mismatch")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a models.TestAttempt
	if err := tx.QueryRow(ctx, `
		INSERT INTO test_attempts (user_id, test_id)
		VALUES ($1, $2)
		RETURNING id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at`,
		userID, testID).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt); err != nil {
		return nil, err
	}

	for i, qid := range questionIDs {
		orderJSON, err := json.Marshal(optionOrders[i])
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO attempt_questions (attempt_id, question_id, position, option_order)
			VALUES ($1, $2, $3, $4)`, a.ID, qid, i+1, orderJSON); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetActiveAttempt returns the user's in-progress attempt for a test, or nil.
func (r *AttemptRepository) GetActiveAttempt(ctx context.Context, userID, testID int64) (*models.TestAttempt, error) {
	var a models.TestAttempt
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at
		FROM test_attempts
		WHERE user_id = $1 AND test_id = $2 AND status = 'in_progress'
		ORDER BY id DESC
		LIMIT 1`, userID, testID).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAttemptForUser returns an attempt only if it belongs to the given user.
func (r *AttemptRepository) GetAttemptForUser(ctx context.Context, attemptID, userID int64) (*models.TestAttempt, error) {
	var a models.TestAttempt
	err := r.pool.QueryRow(ctx, `
		SELECT id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at
		FROM test_attempts
		WHERE id = $1 AND user_id = $2`, attemptID, userID).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CurrentQuestion returns the first unanswered question of the attempt.
func (r *AttemptRepository) CurrentQuestion(ctx context.Context, attemptID int64) (*models.AttemptQuestion, *models.Question, error) {
	var aq models.AttemptQuestion
	var sel sql.NullString
	var orderJSON []byte
	err := r.pool.QueryRow(ctx, `
		SELECT id, attempt_id, question_id, position, answered, selected_answer, is_correct, option_order
		FROM attempt_questions
		WHERE attempt_id = $1 AND NOT answered
		ORDER BY position
		LIMIT 1`, attemptID).
		Scan(&aq.ID, &aq.AttemptID, &aq.QuestionID, &aq.Position, &aq.Answered,
			&sel, &aq.IsCorrect, &orderJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	aq.SelectedAnswer = sel.String
	if err := json.Unmarshal(orderJSON, &aq.OptionOrder); err != nil {
		return nil, nil, err
	}

	q := &models.Question{}
	err = r.pool.QueryRow(ctx, `
		SELECT id, subject_id, question_text, option_a, option_b, option_c,
		       option_d, correct_answer, topic, difficulty
		FROM questions WHERE id = $1`, aq.QuestionID).
		Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB, &q.OptionC,
			&q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty)
	if err != nil {
		return nil, nil, err
	}
	return &aq, q, nil
}

// QuestionAtPosition returns the attempt question at a fixed position.
func (r *AttemptRepository) QuestionAtPosition(ctx context.Context, attemptID int64, position int) (*models.AttemptQuestion, *models.Question, error) {
	var aq models.AttemptQuestion
	var sel sql.NullString
	var orderJSON []byte
	err := r.pool.QueryRow(ctx, `
		SELECT id, attempt_id, question_id, position, answered, selected_answer, is_correct, option_order
		FROM attempt_questions
		WHERE attempt_id = $1 AND position = $2`, attemptID, position).
		Scan(&aq.ID, &aq.AttemptID, &aq.QuestionID, &aq.Position, &aq.Answered,
			&sel, &aq.IsCorrect, &orderJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	aq.SelectedAnswer = sel.String
	if err := json.Unmarshal(orderJSON, &aq.OptionOrder); err != nil {
		return nil, nil, err
	}

	q := &models.Question{}
	err = r.pool.QueryRow(ctx, `
		SELECT id, subject_id, question_text, option_a, option_b, option_c,
		       option_d, correct_answer, topic, difficulty
		FROM questions WHERE id = $1`, aq.QuestionID).
		Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB, &q.OptionC,
			&q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty)
	if err != nil {
		return nil, nil, err
	}
	return &aq, q, nil
}

// SubmitAnswer records the user's answer atomically:
//   - verifies the attempt belongs to the user and is in progress;
//   - marks the attempt_question answered exactly once (guarded by
//     "answered = FALSE", so a repeated callback cannot double-count);
//   - updates attempt counters and position;
//   - upserts user_question_progress applying knowledge-system rules;
//   - completes the attempt when it was the last question.
func (r *AttemptRepository) SubmitAnswer(ctx context.Context, userID, attemptID int64, position int, selected string) (*AnswerResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 1. Lock the attempt and verify ownership and status.
	var a models.TestAttempt
	err = tx.QueryRow(ctx, `
		SELECT id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at
		FROM test_attempts
		WHERE id = $1 AND user_id = $2
		FOR UPDATE`, attemptID, userID).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.Status != models.AttemptInProgress {
		return nil, errors.New("attempt is not in progress")
	}

	// 2. Load the attempt question at this position.
	var aq models.AttemptQuestion
	var sel sql.NullString
	var orderJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT id, attempt_id, question_id, position, answered, selected_answer, is_correct, option_order
		FROM attempt_questions
		WHERE attempt_id = $1 AND position = $2
		FOR UPDATE`, attemptID, position).
		Scan(&aq.ID, &aq.AttemptID, &aq.QuestionID, &aq.Position, &aq.Answered,
			&sel, &aq.IsCorrect, &orderJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	aq.SelectedAnswer = sel.String

	// 3. Guard against duplicate answers (repeated callback).
	if aq.Answered {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &AnswerResult{AlreadyAnswered: true}, nil
	}

	// 4. Load the question and evaluate the answer.
	q := &models.Question{}
	if err := tx.QueryRow(ctx, `
		SELECT id, subject_id, question_text, option_a, option_b, option_c,
		       option_d, correct_answer, topic, difficulty
		FROM questions WHERE id = $1`, aq.QuestionID).
		Scan(&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB, &q.OptionC,
			&q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty); err != nil {
		return nil, err
	}
	correct := selected == q.CorrectAnswer

	// 5. Mark the question answered (optimistic guard).
	tag, err := tx.Exec(ctx, `
		UPDATE attempt_questions
		SET answered = TRUE, selected_answer = $3, is_correct = $4
		WHERE attempt_id = $1 AND position = $2 AND answered = FALSE`,
		attemptID, position, selected, correct)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		// Lost the race against a duplicate callback.
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &AnswerResult{AlreadyAnswered: true}, nil
	}

	// 6. Update user knowledge progress.
	// Lock the (user, question) progress row, then apply the knowledge
	// transition rules to the PREVIOUS status:
	//   correct:   🔴→🟡, 🟡→🟢, 🟢→🟢
	//   incorrect: 🔴→🔴, 🟡→🔴, 🟢→🟡
	correctInc, wrongInc := 0, 0
	if correct {
		correctInc = 1
	} else {
		wrongInc = 1
	}
	prevStatus := models.DefaultStatus // a never-seen question starts at 🔴
	err = tx.QueryRow(ctx, `
		SELECT status FROM user_question_progress
		WHERE user_id = $1 AND question_id = $2
		FOR UPDATE`, userID, aq.QuestionID).Scan(&prevStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	newStatus := models.NextStatus(prevStatus, correct)
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_question_progress (user_id, question_id, status, correct_count, wrong_count)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, question_id) DO UPDATE SET
			status        = EXCLUDED.status,
			correct_count = user_question_progress.correct_count + EXCLUDED.correct_count,
			wrong_count   = user_question_progress.wrong_count   + EXCLUDED.wrong_count,
			updated_at    = now()`,
		userID, aq.QuestionID, newStatus, correctInc, wrongInc); err != nil {
		return nil, err
	}

	// 6b. Per-TOPIC statistics (the source of weak topics) — in the same
	// transaction, so the answer and the topic statistics never diverge.
	if countsForTopic(prevStatus, correct) {
		if err := recordTopicAnswer(ctx, tx, userID, q.SubjectID, q.Topic, correct); err != nil {
			return nil, err
		}
	}

	// 7. Advance the attempt counters and position.
	var total int
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM attempt_questions WHERE attempt_id = $1`, attemptID).Scan(&total); err != nil {
		return nil, err
	}
	finished := position >= total

	err = tx.QueryRow(ctx, `
		UPDATE test_attempts
		SET correct_count = correct_count + $3,
		    wrong_count   = wrong_count   + $4,
		    current_position = $5,
		    status = CASE WHEN $6 THEN 'completed' ELSE status END,
		    completed_at = CASE WHEN $6 THEN now() ELSE completed_at END
		WHERE id = $1 AND user_id = $2
		RETURNING id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at`,
		attemptID, userID, correctInc, wrongInc, position, finished).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &AnswerResult{
		Question:       q,
		Attempt:        &a,
		SelectedAnswer: selected,
		Correct:        correct,
		Finished:       finished,
		NewStatus:      newStatus,
	}, nil
}

// AssertAttemptForUser verifies that the attempt exists and belongs to the
// user. Used by the "Выйти" flow, which keeps the attempt in progress so it
// can be resumed later.
func (r *AttemptRepository) AssertAttemptForUser(ctx context.Context, attemptID, userID int64) error {
	_, err := r.GetAttemptForUser(ctx, attemptID, userID)
	return err
}

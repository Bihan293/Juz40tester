package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// Charged: this answer completed the attempt and one test of today's
	// quota was charged (subscriptions on, non-personal test).
	Charged bool
}

// ErrAnswerOutOfOrder: the answered position is not the first unanswered
// question of the attempt (stale callback, forged client, race).
var ErrAnswerOutOfOrder = errors.New("answer is not for the current question")

// ErrAttemptClosed: the attempt is no longer in progress (completed by an
// earlier answer, or abandoned by «🔄 Пройти ещё раз» / the stale-attempt
// reaper) — a late or stale answer callback must be told so, not reported
// as a failure to save the answer.
var ErrAttemptClosed = errors.New("attempt is not in progress")

// AttemptRepository manages test attempts, answers and knowledge progress.
type AttemptRepository struct {
	pool *pgxpool.Pool
	// quota (optional) is the daily test quota (subscriptions): the start
	// gate runs inside the attempt-creation transaction, the charge inside
	// the final-answer transaction. nil = no quota (the old behaviour).
	quota AttemptQuota
}

// AttemptQuota is the daily quota of completed tests (BillingRepository).
type AttemptQuota interface {
	// CheckStartTx refuses (ErrQuotaExceeded) a NEW attempt when the user
	// has nothing left today. Runs in the creating transaction.
	CheckStartTx(ctx context.Context, tx pgx.Tx, userID, testID int64) error
	// ChargeCompletionTx charges one completion for the attempt (at most
	// once per attempt). Runs in the final-answer transaction.
	ChargeCompletionTx(ctx context.Context, tx pgx.Tx, userID, attemptID, testID int64) (bool, error)
	// LockUserTx serialises the attempt creation of one user. It is taken
	// FIRST in the creating transaction (before the «replace» UPDATE): if
	// the per-user lock were taken after row locks, two concurrent
	// restarts of the same test would deadlock (row lock vs. advisory lock).
	LockUserTx(ctx context.Context, tx pgx.Tx, userID int64) error
	// Now is the quota clock. A new attempt's started_at is stamped with
	// it, so the «started today» count of the start gate and the quota day
	// the completion is charged to use the SAME clock.
	Now() time.Time
}

// WithQuota installs the daily quota (nil = off).
func (r *AttemptRepository) WithQuota(q AttemptQuota) *AttemptRepository {
	r.quota = q
	return r
}

func NewAttemptRepository(pool *pgxpool.Pool) *AttemptRepository {
	return &AttemptRepository{pool: pool}
}

// CreateAttempt atomically creates a new attempt with shuffled questions and
// shuffled answer options for every question.
//
// At most ONE in-progress attempt per (user, test) exists (unique partial
// index idx_attempts_in_progress_unique):
//   - replace = false (opening a test): an already open attempt is returned
//     as is — a double tap never creates a second attempt;
//   - replace = true («🔄 Пройти ещё раз»): the open attempt (if any) is
//     closed as 'abandoned' in the same transaction, then the new one is
//     inserted.
//
// A concurrent creation that loses the race on the unique index gets the
// winner's attempt instead of an error.
func (r *AttemptRepository) CreateAttempt(ctx context.Context, userID, testID int64, questionIDs []int64, optionOrders [][]string, replace bool) (*models.TestAttempt, error) {
	return r.createAttempt(ctx, userID, testID, questionIDs, optionOrders, replace, true)
}

// CreateFreshAttempt is CreateAttempt(replace = false) for a caller that has
// JUST looked the active attempt up and found none (A5): the second lookup
// inside the transaction is skipped. A concurrent creation still loses on
// the unique index and gets the winner's attempt, exactly as before.
func (r *AttemptRepository) CreateFreshAttempt(ctx context.Context, userID, testID int64, questionIDs []int64, optionOrders [][]string) (*models.TestAttempt, error) {
	return r.createAttempt(ctx, userID, testID, questionIDs, optionOrders, false, false)
}

func (r *AttemptRepository) createAttempt(ctx context.Context, userID, testID int64, questionIDs []int64, optionOrders [][]string, replace, lookup bool) (*models.TestAttempt, error) {
	if len(questionIDs) != len(optionOrders) {
		return nil, errors.New("questions and option orders length mismatch")
	}
	a, err := r.createAttemptTx(ctx, userID, testID, questionIDs, optionOrders, replace, lookup)
	if isUniqueViolation(err) || (errors.Is(err, ErrQuotaExceeded) && !replace) {
		// Lost a double-tap race (the other tap created the attempt — and
		// may have taken the last quota slot with it): resume the winner.
		existing, gerr := r.GetActiveAttempt(ctx, userID, testID)
		if gerr != nil {
			return nil, gerr
		}
		if existing != nil {
			return existing, nil
		}
	}
	return a, err
}

func (r *AttemptRepository) createAttemptTx(ctx context.Context, userID, testID int64, questionIDs []int64, optionOrders [][]string, replace, lookup bool) (*models.TestAttempt, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if r.quota != nil {
		if err := r.quota.LockUserTx(ctx, tx, userID); err != nil {
			return nil, err
		}
	}
	if replace {
		if _, err := tx.Exec(ctx, `
			UPDATE test_attempts SET status = 'abandoned', updated_at = now()
			WHERE user_id = $1 AND test_id = $2 AND status = 'in_progress'`, userID, testID); err != nil {
			return nil, err
		}
	} else if lookup {
		var a models.TestAttempt
		err := tx.QueryRow(ctx, `
			SELECT id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at
			FROM test_attempts
			WHERE user_id = $1 AND test_id = $2 AND status = 'in_progress'
			ORDER BY id DESC
			LIMIT 1`, userID, testID).
			Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
				&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt)
		if err == nil {
			return &a, tx.Commit(ctx)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}

	if r.quota != nil {
		if err := r.quota.CheckStartTx(ctx, tx, userID, testID); err != nil {
			return nil, err
		}
	}

	// With a quota the start time comes from the quota clock (see
	// AttemptQuota.Now); otherwise the database clock, as before.
	var startedAt *time.Time
	if r.quota != nil {
		now := r.quota.Now()
		startedAt = &now
	}
	var a models.TestAttempt
	if err := tx.QueryRow(ctx, `
		INSERT INTO test_attempts (user_id, test_id, started_at)
		VALUES ($1, $2, COALESCE($3::timestamptz, now()))
		RETURNING id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at`,
		userID, testID, startedAt).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt); err != nil {
		return nil, err
	}

	// R-2: ONE batch INSERT for all questions of the attempt (it used to be
	// one INSERT per question — 20 round trips inside the transaction).
	// unnest() zips the three arrays row by row; WITH ORDINALITY yields the
	// 1-based position in the shuffled order.
	orders := make([]string, len(questionIDs))
	for i := range questionIDs {
		enc, err := encodeOptionOrder(optionOrders[i])
		if err != nil {
			return nil, err
		}
		orders[i] = enc
	}
	if len(questionIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO attempt_questions (attempt_id, question_id, position, option_order)
			SELECT $1, u.qid, u.pos, u.ord::jsonb
			FROM unnest($2::bigint[], $3::text[]) WITH ORDINALITY AS u(qid, ord, pos)`,
			a.ID, questionIDs, orders); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &a, nil
}

// isUniqueViolation reports a PostgreSQL unique_violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// AbandonStaleAttempts closes in-progress attempts that were not touched
// for olderThan (the user left the test and never came back). Such attempts
// would otherwise keep their questions "busy" for the quality sweep forever.
func (r *AttemptRepository) AbandonStaleAttempts(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE test_attempts SET status = 'abandoned', updated_at = now()
		WHERE status = 'in_progress' AND updated_at < now() - make_interval(secs => $1)`, durationSecs(olderThan))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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

// ActiveAttemptTests reports which of the given tests have an in-progress
// attempt of the user — one query for a whole tests-grid page instead of
// GetActiveAttempt per cell. Tests without an active attempt are absent.
func (r *AttemptRepository) ActiveAttemptTests(ctx context.Context, userID int64, testIDs []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(testIDs))
	if len(testIDs) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT test_id FROM test_attempts
		WHERE user_id = $1 AND test_id = ANY($2) AND status = 'in_progress'`, userID, testIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
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

// QuestionRow is everything rendering one question of an attempt needs,
// loaded by LoadQuestionView in ONE query.
type QuestionRow struct {
	Attempt  *models.TestAttempt
	AQ       *models.AttemptQuestion // nil when no question matched (all answered / bad position)
	Question *models.Question
	// Meta is filled only when requested (withMeta). It is immutable for
	// the lifetime of an attempt (question count, subject name), except
	// Translated, which can only grow while a translation completes.
	Meta *TestViewMeta
	// Translation is the cached translation of THIS question in the
	// requested language (nil when lang is "" or there is none).
	Translation *models.QuestionTranslation
}

// LoadQuestionView loads, in a single round trip:
//   - the attempt, with the OWNERSHIP check (a.user_id = userID) — a foreign
//     or missing attempt yields ErrNotFound, exactly like GetAttemptForUser;
//   - the attempt question (position > 0: that position; position == 0: the
//     first unanswered one) joined with its master question row;
//   - optionally the TestViewMeta of the test (withMeta) — callers that
//     already have it for this attempt pass false and reuse it;
//   - optionally the cached translation of the question (lang != "").
//
// It replaces GetAttemptForUser + CurrentQuestion/QuestionAtPosition (2
// queries) + TestViewMeta + TranslationForQuestion (R-2).
func (r *AttemptRepository) LoadQuestionView(ctx context.Context, attemptID, userID int64, position int, withMeta bool, lang string) (*QuestionRow, error) {
	var a models.TestAttempt
	var aqID, aqAttempt, aqQID, qID, qSubject sql.NullInt64
	var aqPos, qDiff sql.NullInt32
	var aqAnswered sql.NullBool
	var sel sql.NullString
	var orderJSON []byte
	var isCorrect *bool
	var qText, qA, qB, qC, qD, qCorrect, qTopic sql.NullString
	var subjName string
	var total, translated int
	var trText, trA, trB, trC, trD, trTopic sql.NullString

	err := r.pool.QueryRow(ctx, `
		SELECT a.id, a.user_id, a.test_id, a.status, a.current_position, a.correct_count, a.wrong_count, a.started_at, a.completed_at,
		       aq.id, aq.attempt_id, aq.question_id, aq.position, aq.answered, aq.selected_answer, aq.is_correct, aq.option_order,
		       q.id, q.subject_id, q.question_text, q.option_a, q.option_b, q.option_c, q.option_d, q.correct_answer, q.topic, q.difficulty,
		       CASE WHEN $4::bool THEN (SELECT s.name FROM tests t JOIN subjects s ON s.id = t.subject_id WHERE t.id = a.test_id) ELSE '' END,
		       CASE WHEN $4::bool THEN (SELECT COUNT(*) FROM test_questions tq WHERE tq.test_id = a.test_id) ELSE 0 END,
		       CASE WHEN $4::bool THEN (SELECT COUNT(DISTINCT tr2.question_id)
		                                  FROM test_questions tq
		                                  JOIN question_translations tr2
		                                    ON tr2.question_id = tq.question_id AND tr2.lang = $6
		                                 WHERE tq.test_id = a.test_id) ELSE 0 END,
		       tr.question_text, tr.option_a, tr.option_b, tr.option_c, tr.option_d, tr.topic
		FROM test_attempts a
		LEFT JOIN LATERAL (
			SELECT * FROM attempt_questions x
			WHERE x.attempt_id = a.id
			  AND (CASE WHEN $3::int = 0 THEN NOT x.answered ELSE x.position = $3::int END)
			ORDER BY x.position
			LIMIT 1
		) aq ON TRUE
		LEFT JOIN questions q ON q.id = aq.question_id
		LEFT JOIN question_translations tr ON tr.question_id = q.id AND tr.lang = $5 AND $5 <> ''
		WHERE a.id = $1 AND a.user_id = $2`,
		attemptID, userID, position, withMeta, lang, models.TestLangKK).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition, &a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt,
			&aqID, &aqAttempt, &aqQID, &aqPos, &aqAnswered, &sel, &isCorrect, &orderJSON,
			&qID, &qSubject, &qText, &qA, &qB, &qC, &qD, &qCorrect, &qTopic, &qDiff,
			&subjName, &total, &translated,
			&trText, &trA, &trB, &trC, &trD, &trTopic)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	row := &QuestionRow{Attempt: &a}
	if withMeta {
		row.Meta = &TestViewMeta{SubjectName: subjName, Total: total, Translated: translated}
	}
	if !aqID.Valid || !qID.Valid {
		return row, nil
	}
	aq := &models.AttemptQuestion{
		ID: aqID.Int64, AttemptID: aqAttempt.Int64, QuestionID: aqQID.Int64,
		Position: int(aqPos.Int32), Answered: aqAnswered.Bool, SelectedAnswer: sel.String, IsCorrect: isCorrect,
	}
	if aq.OptionOrder, err = decodeOptionOrder(orderJSON); err != nil {
		return nil, err
	}
	row.AQ = aq
	row.Question = &models.Question{
		ID: qID.Int64, SubjectID: qSubject.Int64, Text: qText.String,
		OptionA: qA.String, OptionB: qB.String, OptionC: qC.String, OptionD: qD.String,
		CorrectAnswer: qCorrect.String, Topic: qTopic.String, Difficulty: int(qDiff.Int32),
	}
	if trText.Valid {
		row.Translation = &models.QuestionTranslation{
			QuestionID: qID.Int64, Lang: lang, Text: trText.String,
			OptionA: trA.String, OptionB: trB.String, OptionC: trC.String, OptionD: trD.String,
			Topic: trTopic.String, CorrectAnswer: qCorrect.String,
		}
	}
	return row, nil
}

// SummaryRow is everything the result screen needs (see LoadSummary).
type SummaryRow struct {
	Attempt      *models.TestAttempt
	Test         *models.Test
	Subject      *models.Subject
	Total        int
	StatusCounts map[int]int
}

// LoadSummary loads the attempt (ownership-checked), its test, the subject,
// the question count and the user's CURRENT 🔴🟡🟢 counts over the test's
// questions in ONE query (it used to be 5: attempt, test, subject, test
// questions, statuses). A question without a progress row counts as 🔴,
// exactly as before.
func (r *AttemptRepository) LoadSummary(ctx context.Context, attemptID, userID int64) (*SummaryRow, error) {
	var a models.TestAttempt
	var t models.Test
	var s models.Subject
	var topicsJSON []byte
	var owner sql.NullInt64
	var total, red, yellow, green int
	err := r.pool.QueryRow(ctx, `
		SELECT a.id, a.user_id, a.test_id, a.status, a.current_position, a.correct_count, a.wrong_count, a.started_at, a.completed_at,
		       t.id, t.subject_id, t.test_number, t.title, t.is_active, t.kind, t.topics, t.owner_user_id,
		       s.id, s.name, s.position,
		       st.total, st.red, st.yellow, st.green
		FROM test_attempts a
		JOIN tests t ON t.id = a.test_id
		JOIN subjects s ON s.id = t.subject_id
		CROSS JOIN LATERAL (
			SELECT COUNT(*)::int                                                AS total,
			       COUNT(*) FILTER (WHERE COALESCE(p.status, 0) = 0)::int      AS red,
			       COUNT(*) FILTER (WHERE p.status = 1)::int                   AS yellow,
			       COUNT(*) FILTER (WHERE p.status = 2)::int                   AS green
			FROM test_questions tq
			LEFT JOIN user_question_progress p
			       ON p.user_id = a.user_id AND p.question_id = tq.question_id
			WHERE tq.test_id = t.id
		) st
		WHERE a.id = $1 AND a.user_id = $2`, attemptID, userID).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition, &a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt,
			&t.ID, &t.SubjectID, &t.TestNumber, &t.Title, &t.IsActive, &t.Kind, &topicsJSON, &owner,
			&s.ID, &s.Name, &s.Position,
			&total, &red, &yellow, &green)
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
	return &SummaryRow{
		Attempt: &a, Test: &t, Subject: &s, Total: total,
		StatusCounts: map[int]int{models.StatusNone: red, models.StatusPartial: yellow, models.StatusMastered: green},
	}, nil
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
		return nil, ErrAttemptClosed
	}

	// 2. ONE query (A4): the attempt question at this position (locked) JOINed
	// with its question row, plus the lowest unanswered position and the
	// number of unanswered questions of the attempt. The attempt row is
	// locked above, so these numbers cannot change until commit.
	var aq models.AttemptQuestion
	var sel sql.NullString
	var orderJSON []byte
	q := &models.Question{}
	var minUnanswered, unanswered int
	var topicKey string // B2: catalog topic of the question ("" = not mapped)
	var testKind string // a custom test never feeds the topic statistics
	err = tx.QueryRow(ctx, `
		SELECT aq.id, aq.attempt_id, aq.question_id, aq.position, aq.answered, aq.selected_answer, aq.is_correct, aq.option_order,
		       q.id, q.subject_id, q.question_text, q.option_a, q.option_b, q.option_c,
		       q.option_d, q.correct_answer, q.topic, q.difficulty, COALESCE(q.topic_key, ''),
		       u.min_pos, u.cnt, COALESCE((SELECT kind FROM tests WHERE id = $3), '')
		FROM attempt_questions aq
		JOIN questions q ON q.id = aq.question_id
		CROSS JOIN (
			SELECT COALESCE(MIN(position), 0) AS min_pos, COUNT(*)::int AS cnt
			FROM attempt_questions WHERE attempt_id = $1 AND NOT answered
		) u
		WHERE aq.attempt_id = $1 AND aq.position = $2
		FOR UPDATE OF aq`, attemptID, position, a.TestID).
		Scan(&aq.ID, &aq.AttemptID, &aq.QuestionID, &aq.Position, &aq.Answered,
			&sel, &aq.IsCorrect, &orderJSON,
			&q.ID, &q.SubjectID, &q.Text, &q.OptionA, &q.OptionB, &q.OptionC,
			&q.OptionD, &q.CorrectAnswer, &q.Topic, &q.Difficulty, &topicKey,
			&minUnanswered, &unanswered, &testKind)
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

	// 3b. Only the CURRENT question (the lowest unanswered position) may be
	// answered — a stale/forged callback for a later position must neither
	// skip questions nor finish the attempt early.
	if position != minUnanswered {
		return nil, ErrAnswerOutOfOrder
	}
	correct := selected == q.CorrectAnswer
	// The attempt is finished only when NO unanswered question is left
	// after this one (not merely because this position is the last one).
	finished := unanswered <= 1

	// 4. Mark the question answered (optimistic guard).
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

	// 5. Update user knowledge progress in ONE upsert (A4). The previous
	// status is read (and locked) in the CTE; the new one is computed in SQL
	// from the stored row with the knowledge transition rules
	// (models.NextStatus):
	//   correct:   🔴→🟡, 🟡→🟢, 🟢→🟢
	//   incorrect: 🔴→🔴, 🟡→🔴, 🟢→🟡
	correctInc, wrongInc := 0, 0
	if correct {
		correctInc = 1
	} else {
		wrongInc = 1
	}
	var prevStatus, newStatus int
	if err := tx.QueryRow(ctx, `
		WITH prev AS (
			SELECT status FROM user_question_progress
			WHERE user_id = $1 AND question_id = $2
			FOR UPDATE
		), up AS (
			INSERT INTO user_question_progress (user_id, question_id, status, correct_count, wrong_count)
			SELECT $1, $2,
			       CASE WHEN $3::bool THEN LEAST(COALESCE(prev.status, $4::int) + 1, $6::int)
			            ELSE GREATEST(COALESCE(prev.status, $4::int) - 1, $5::int) END,
			       $7, $8
			FROM (SELECT 1) one LEFT JOIN prev ON TRUE
			ON CONFLICT (user_id, question_id) DO UPDATE SET
				status        = CASE WHEN $3::bool THEN LEAST(user_question_progress.status + 1, $6::int)
				                     ELSE GREATEST(user_question_progress.status - 1, $5::int) END,
				correct_count = user_question_progress.correct_count + EXCLUDED.correct_count,
				wrong_count   = user_question_progress.wrong_count   + EXCLUDED.wrong_count,
				updated_at    = now()
			RETURNING status
		)
		SELECT (SELECT status FROM up), COALESCE((SELECT status FROM prev), $4::int)`,
		userID, aq.QuestionID, correct, models.DefaultStatus, models.StatusNone,
		models.StatusMastered, correctInc, wrongInc).
		Scan(&newStatus, &prevStatus); err != nil {
		return nil, err
	}

	// 5b. Per-TOPIC statistics (the source of weak topics) — in the same
	// transaction, so the answer and the topic statistics never diverge.
	// A «✨ Свой тест» test is excluded: its topics come from the student's
	// own request, not from the subject programme — they must not create or
	// move weak topics.
	if countsForTopic(prevStatus) && testKind != models.TestKindCustom {
		if err := recordTopicAnswer(ctx, tx, userID, q.SubjectID, q.Topic, topicKey, correct); err != nil {
			return nil, err
		}
	}

	// 6. Advance the attempt counters and position.
	err = tx.QueryRow(ctx, `
		UPDATE test_attempts
		SET correct_count = correct_count + $3,
		    wrong_count   = wrong_count   + $4,
		    current_position = $5,
		    status = CASE WHEN $6 THEN 'completed' ELSE status END,
		    completed_at = CASE WHEN $6 THEN now() ELSE completed_at END,
		    updated_at = now()
		WHERE id = $1 AND user_id = $2
		RETURNING id, user_id, test_id, status, current_position, correct_count, wrong_count, started_at, completed_at`,
		attemptID, userID, correctInc, wrongInc, position, finished).
		Scan(&a.ID, &a.UserID, &a.TestID, &a.Status, &a.CurrentPosition,
			&a.CorrectCount, &a.WrongCount, &a.StartedAt, &a.CompletedAt)
	if err != nil {
		return nil, err
	}

	// 7. Subscriptions: the completed test is charged against today's quota
	// in the SAME transaction (idempotent per attempt).
	charged := false
	if finished && r.quota != nil {
		if charged, err = r.quota.ChargeCompletionTx(ctx, tx, userID, attemptID, a.TestID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &AnswerResult{
		Charged:        charged,
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

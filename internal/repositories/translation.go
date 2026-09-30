package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bihan293/Juz40-test2/internal/models"
)

// TranslationRepository manages the cached per-question translations of test
// content (question_translations table). The Russian question row is always
// the master version; a translation is written ONCE per question and then
// reused by every user — this is the cost ceiling of the whole feature.
type TranslationRepository struct {
	pool *pgxpool.Pool
}

func NewTranslationRepository(pool *pgxpool.Pool) *TranslationRepository {
	return &TranslationRepository{pool: pool}
}

// TranslationsForTest returns the translations of a test's questions into the
// given language, keyed by question id. Only the questions of THIS test are
// loaded (translations are stored per question, shared across tests).
func (r *TranslationRepository) TranslationsForTest(ctx context.Context, testID int64, lang string) (map[int64]*models.QuestionTranslation, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT tr.question_id, tr.lang, tr.question_text, tr.option_a, tr.option_b,
		       tr.option_c, tr.option_d, tr.topic, q.correct_answer
		FROM test_questions tq
		JOIN question_translations tr ON tr.question_id = tq.question_id AND tr.lang = $2
		JOIN questions q ON q.id = tr.question_id
		WHERE tq.test_id = $1`, testID, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]*models.QuestionTranslation)
	for rows.Next() {
		var tr models.QuestionTranslation
		if err := rows.Scan(&tr.QuestionID, &tr.Lang, &tr.Text, &tr.OptionA, &tr.OptionB,
			&tr.OptionC, &tr.OptionD, &tr.Topic, &tr.CorrectAnswer); err != nil {
			return nil, err
		}
		out[tr.QuestionID] = &tr
	}
	return out, rows.Err()
}

// TranslationForQuestion returns the cached translation of ONE question, or
// (nil, nil) when it has not been translated yet.
func (r *TranslationRepository) TranslationForQuestion(ctx context.Context, questionID int64, lang string) (*models.QuestionTranslation, error) {
	var tr models.QuestionTranslation
	err := r.pool.QueryRow(ctx, `
		SELECT tr.question_id, tr.lang, tr.question_text, tr.option_a, tr.option_b,
		       tr.option_c, tr.option_d, tr.topic, q.correct_answer
		FROM question_translations tr
		JOIN questions q ON q.id = tr.question_id
		WHERE tr.question_id = $1 AND tr.lang = $2`, questionID, lang).
		Scan(&tr.QuestionID, &tr.Lang, &tr.Text, &tr.OptionA, &tr.OptionB,
			&tr.OptionC, &tr.OptionD, &tr.Topic, &tr.CorrectAnswer)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tr, nil
}

// SaveTranslations atomically stores translations for a batch of questions.
// The model translates the OPTION TEXTS only — the correct-answer letter
// never leaves the server: the answer key is read back from the master
// questions row at query time (TranslationsForTest / TranslationForQuestion
// JOIN questions), so it can never be lost or altered in translation.
// correctByQuestionID validates that every stored row maps to a known
// master question.
// ON CONFLICT DO NOTHING makes concurrent translation runs safe (the loser
// keeps the winner's row — both came from the same master text anyway).
func (r *TranslationRepository) SaveTranslations(ctx context.Context, trs []models.QuestionTranslation, correctByQuestionID map[int64]string) error {
	if len(trs) == 0 {
		return nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, tr := range trs {
		correct := correctByQuestionID[tr.QuestionID]
		if correct == "" {
			return fmt.Errorf("no master correct_answer for question %d", tr.QuestionID)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO question_translations
			    (question_id, lang, question_text, option_a, option_b, option_c, option_d, topic)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (question_id, lang) DO NOTHING`,
			tr.QuestionID, tr.Lang, tr.Text, tr.OptionA, tr.OptionB, tr.OptionC, tr.OptionD, tr.Topic); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// TranslatedQuestionCount returns how many of the given questions already
// have a translation in the language (used to decide whether a test needs a
// translation run at all — zero cost when everything is cached).
func (r *TranslationRepository) TranslatedQuestionCount(ctx context.Context, questionIDs []int64, lang string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM question_translations
		WHERE lang = $1 AND question_id = ANY($2)`, lang, questionIDs).Scan(&n)
	return n, err
}

// QuestionIDsForTest returns the question ids of a test in their canonical
// order (test_questions.position). The generator uses it to pair the fresh
// question rows of a CLONED personal test with the cached translations of
// the clone source.
func (r *TranslationRepository) QuestionIDsForTest(ctx context.Context, testID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT question_id FROM test_questions
		WHERE test_id = $1 ORDER BY position`, testID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CopyTranslationsToTest copies the cached translations of a SOURCE test's
// questions onto the given (freshly cloned) question ids, matched by the
// master question TEXT. Personal weak-topics tests are cloned with NEW
// question rows (so the clone has no shared progress), which means the
// cached Kazakh translations — keyed by question id — would otherwise be
// lost and every Kazakh-speaking user would pay for a NEW DeepSeek
// translation of a test that is word-for-word identical to an already
// translated one. Copying the cached rows costs zero API calls.
//
// Idempotent (ON CONFLICT DO NOTHING) and race-safe: a row is written only
// when the source question is an exact text match of the clone's question
// (the option texts then come from the source row whose question matched —
// exactly the right pairing).
func (r *TranslationRepository) CopyTranslationsToTest(ctx context.Context, srcTestID int64, dstQuestionIDs []int64, lang string) (int64, error) {
	if len(dstQuestionIDs) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO question_translations
		    (question_id, lang, question_text, option_a, option_b, option_c, option_d, topic)
		SELECT dq.id, tr.lang, tr.question_text, tr.option_a, tr.option_b, tr.option_c, tr.option_d, tr.topic
		FROM test_questions stq
		JOIN questions sq ON sq.id = stq.question_id
		JOIN question_translations tr ON tr.question_id = sq.id AND tr.lang = $3
		JOIN questions dq ON dq.question_text = sq.question_text AND dq.id = ANY($2)
		WHERE stq.test_id = $1
		ON CONFLICT (question_id, lang) DO NOTHING`, srcTestID, dstQuestionIDs, lang)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

package repositories

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// BankQuota splits total questions over the topics as evenly as possible
// (B3: 5 topics x 4; fewer weak topics get more questions each, so the test
// still has total questions). The first topics (the weakest) get the
// remainder.
func BankQuota(topicKeys []string, total int) map[string]int {
	out := make(map[string]int, len(topicKeys))
	if len(topicKeys) == 0 || total <= 0 {
		return out
	}
	per, rest := total/len(topicKeys), total%len(topicKeys)
	for i, k := range topicKeys {
		n := per
		if i < rest {
			n++
		}
		out[k] = n
	}
	return out
}

// bankCandidatesSQL picks, per requested topic_key, up to its quota of
// quality-checked bank questions the user has not seen (no progress row) or
// still has 🔴, in random order.
const bankCandidatesSQL = `
	WITH quota AS (
		SELECT * FROM unnest($3::text[], $4::int[]) AS t(topic_key, n)
	), c AS (
		SELECT q.id, q.topic_key,
		       row_number() OVER (PARTITION BY q.topic_key ORDER BY random()) AS rn
		FROM questions q
		JOIN quota ON quota.topic_key = q.topic_key
		LEFT JOIN user_question_progress p ON p.user_id = $2 AND p.question_id = q.id
		WHERE q.subject_id = $1
		  AND q.quality_checked_at IS NOT NULL
		  AND (p.question_id IS NULL OR p.status = 0)
	)
	SELECT c.id, c.topic_key FROM c JOIN quota ON quota.topic_key = c.topic_key
	WHERE c.rn <= quota.n
	ORDER BY random()`

// BankShortage is a topic whose bank quota could not be filled for the user
// (B4b): Missing questions are lacking.
type BankShortage struct {
	TopicKey string
	Missing  int
}

// ShortageKeys returns the topic_keys of the shortages (in order).
func ShortageKeys(s []BankShortage) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.TopicKey
	}
	return out
}

// BankCandidates returns the bank questions selected for the quotas
// (question ids in display order) and the topics whose quota could not be
// filled with the number of lacking questions (in the order of topicKeys).
func (r *GenerationRepository) BankCandidates(ctx context.Context, subjectID, userID int64, topicKeys []string, quota map[string]int) (ids []int64, missing []BankShortage, err error) {
	keys := make([]string, 0, len(topicKeys))
	ns := make([]int32, 0, len(topicKeys))
	for _, k := range topicKeys {
		keys = append(keys, k)
		ns = append(ns, int32(quota[k]))
	}
	rows, err := r.pool.Query(ctx, bankCandidatesSQL, subjectID, userID, keys, ns)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	got := make(map[string]int, len(keys))
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		got[key]++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, k := range topicKeys {
		if got[k] < quota[k] {
			missing = append(missing, BankShortage{TopicKey: k, Missing: quota[k] - got[k]})
		}
	}
	return ids, missing, nil
}

// CreateBankPersonalTest (B3) creates ONE personal test row owned by the
// user that links the given EXISTING bank questions (no new questions rows,
// no AI call). The number comes from personal_test_number_seq — no
// per-subject lock. When the user already has a personal test of the
// subject (concurrent creation, unique index idx_tests_personal_owner),
// that test is returned instead.
func (r *GenerationRepository) CreateBankPersonalTest(ctx context.Context, subjectID, userID int64, title string, topics []string, questionIDs []int64) (*models.Test, error) {
	if userID <= 0 || len(questionIDs) == 0 {
		return nil, errors.New("bank test: owner and questions are required")
	}
	topicsJSON, err := json.Marshal(topics)
	if err != nil {
		return nil, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var testID int64
	var number int
	err = tx.QueryRow(ctx, `
		INSERT INTO tests (subject_id, test_number, title, is_active, kind, topics, owner_user_id, from_bank)
		VALUES ($1, nextval('personal_test_number_seq'), $2, TRUE, 'personal', $3, $4, TRUE)
		ON CONFLICT DO NOTHING
		RETURNING id, test_number`,
		subjectID, title, topicsJSON, userID).Scan(&testID, &number)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		existing, err := r.FindPersonalTest(ctx, subjectID, userID)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			return nil, errors.New("bank test conflict but no existing personal test found")
		}
		return existing, nil
	}
	if err != nil {
		return nil, err
	}
	// Questions are re-checked inside the INSERT (subject, quality audit):
	// a question rewritten/deleted meanwhile is never linked.
	tag, err := tx.Exec(ctx, `
		INSERT INTO test_questions (test_id, question_id, position)
		SELECT $1, q.id, u.ord
		FROM unnest($2::bigint[]) WITH ORDINALITY AS u(qid, ord)
		JOIN questions q ON q.id = u.qid
		WHERE q.subject_id = $3 AND q.quality_checked_at IS NOT NULL`,
		testID, questionIDs, subjectID)
	if err != nil {
		return nil, err
	}
	if int(tag.RowsAffected()) != len(questionIDs) {
		return nil, errBankChanged
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &models.Test{
		ID: testID, SubjectID: subjectID, TestNumber: number, Title: title,
		IsActive: true, Kind: models.TestKindPersonal, Topics: topics,
		OwnerUserID: userID, FromBank: true,
	}, nil
}

// errBankChanged: a selected bank question disappeared or lost its audit
// mark between selection and creation; the caller falls back.
var errBankChanged = errors.New("bank questions changed during assembly")

// AssembleBankPersonalTest (B3) builds the user's personal test from the
// question bank: quotas over topicKeys (total questions), only audited
// questions the user has not seen or has 🔴. Returns (nil, missing, nil)
// when the bank can not fill every quota — the caller then uses the old
// path (clone / AI generation). Zero AI calls, zero new questions rows.
func (r *GenerationRepository) AssembleBankPersonalTest(ctx context.Context, subjectID, userID int64, topicKeys, titles []string, total int, title string) (*models.Test, []BankShortage, error) {
	if len(topicKeys) == 0 || total <= 0 {
		return nil, nil, nil
	}
	quota := BankQuota(topicKeys, total)
	ids, missing, err := r.BankCandidates(ctx, subjectID, userID, topicKeys, quota)
	if err != nil || len(missing) > 0 {
		return nil, missing, err
	}
	test, err := r.CreateBankPersonalTest(ctx, subjectID, userID, title, titles, ids)
	if errors.Is(err, errBankChanged) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return test, nil, nil
}

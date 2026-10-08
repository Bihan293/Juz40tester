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
// still has 🔴.
//
// The order is a stable pseudo-random permutation seeded by the weak-topics
// FINGERPRINT ($5, models.WeakTopicsFingerprint) instead of random(): two
// users with the same weak topics and the same history get the SAME test
// (identical questions — the same shared rows, zero AI calls, zero new
// rows), while different weak-topic sets still get differently shuffled
// selections. Users who have already seen some questions simply get the
// next unseen ones of the same permutation.
//
// The bank is de-duplicated by CONTENT (normalised question text): template
// clones (personal and chain) store identical questions as separate rows,
// so selecting by id alone could put the same question into one test twice
// and hand a question the user already solved (🟡/🟢) on another row out
// again as "unseen". Exactly one row per text is a candidate, and a text the
// user has progress > 🔴 on (on any row) is excluded.
const bankCandidatesSQL = `
	WITH quota AS (
		SELECT * FROM unnest($3::text[], $4::int[]) AS t(topic_key, n)
	), seen AS (
		SELECT DISTINCT lower(btrim(sq.question_text)) AS t
		FROM user_question_progress sp
		JOIN questions sq ON sq.id = sp.question_id
		WHERE sp.user_id = $2 AND sp.status > 0 AND sq.subject_id = $1
	), u AS (
		SELECT DISTINCT ON (lower(btrim(q.question_text))) q.id, q.topic_key
		FROM questions q
		JOIN quota ON quota.topic_key = q.topic_key
		LEFT JOIN user_question_progress p ON p.user_id = $2 AND p.question_id = q.id
		WHERE q.subject_id = $1
		  AND q.quality_checked_at IS NOT NULL
		  AND (p.question_id IS NULL OR p.status = 0)
		  AND lower(btrim(q.question_text)) NOT IN (SELECT t FROM seen)
		ORDER BY lower(btrim(q.question_text)), md5($5::text || ':' || q.id::text)
	), c AS (
		SELECT u.id, u.topic_key,
		       row_number() OVER (PARTITION BY u.topic_key ORDER BY md5($5::text || ':' || u.id::text)) AS rn
		FROM u
	)
	SELECT c.id, c.topic_key FROM c JOIN quota ON quota.topic_key = c.topic_key
	WHERE c.rn <= quota.n
	ORDER BY md5($5::text || '/' || c.id::text)`

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
//
// seed is the weak-topics fingerprint (identical selection for identical
// weak-topic sets); "" falls back to a per-subject seed.
func (r *GenerationRepository) BankCandidates(ctx context.Context, subjectID, userID int64, topicKeys []string, quota map[string]int, seed string) (ids []int64, missing []BankShortage, err error) {
	if seed == "" {
		seed = models.WeakTopicsFingerprint(subjectID, topicKeys)
	}
	keys := make([]string, 0, len(topicKeys))
	ns := make([]int32, 0, len(topicKeys))
	for _, k := range topicKeys {
		keys = append(keys, k)
		ns = append(ns, int32(quota[k]))
	}
	rows, err := r.pool.Query(ctx, bankCandidatesSQL, subjectID, userID, keys, ns, seed)
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
	return r.createBankPersonalTest(ctx, subjectID, userID, title, topics, questionIDs, "")
}

// createBankPersonalTest is CreateBankPersonalTest that also records the
// weak-topics fingerprint of the test (tests.topics_fingerprint).
func (r *GenerationRepository) createBankPersonalTest(ctx context.Context, subjectID, userID int64, title string, topics []string, questionIDs []int64, fingerprint string) (*models.Test, error) {
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
		INSERT INTO tests (subject_id, test_number, title, is_active, kind, topics, owner_user_id, from_bank, topics_fingerprint)
		VALUES ($1, nextval('personal_test_number_seq'), $2, TRUE, 'personal', $3, $4, TRUE, NULLIF($5, ''))
		ON CONFLICT DO NOTHING
		RETURNING id, test_number`,
		subjectID, title, topicsJSON, userID, fingerprint).Scan(&testID, &number)
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
		OwnerUserID: userID, FromBank: true, TopicsFingerprint: fingerprint,
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
	fp := models.WeakTopicsFingerprint(subjectID, topicKeys)
	ids, missing, err := r.BankCandidates(ctx, subjectID, userID, topicKeys, quota, fp)
	if err != nil || len(missing) > 0 {
		return nil, missing, err
	}
	test, err := r.createBankPersonalTest(ctx, subjectID, userID, title, titles, ids, fp)
	if errors.Is(err, errBankChanged) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return test, nil, nil
}

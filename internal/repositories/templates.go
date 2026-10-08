package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// TestTemplate is the validated content of an AI-generated test, stored
// under the fingerprint of its input (see models.WeakTopicsFingerprint /
// models.ChainFingerprint). «Generate once, clone for everybody else»:
// a further user with the same fingerprint gets a CLONE — a fresh tests row
// with NEW question rows of identical content — without any AI call.
type TestTemplate struct {
	ID           int64
	SubjectID    int64
	Kind         string
	Fingerprint  string
	TestNumber   int
	Topics       []string
	Questions    []models.SeedQuestion
	SourceTestID int64
	Strategy     string
	Uses         int
}

// templateQuestion is the JSON shape of one stored template question.
type templateQuestion struct {
	Text       string    `json:"q"`
	Options    [4]string `json:"o"`
	Correct    int       `json:"c"`
	Topic      string    `json:"t"`
	Difficulty int       `json:"d"`
}

func encodeTemplateQuestions(qs []models.SeedQuestion) ([]byte, error) {
	out := make([]templateQuestion, len(qs))
	for i, q := range qs {
		out[i] = templateQuestion{Text: q.Text, Options: q.Options, Correct: q.Correct, Topic: q.Topic, Difficulty: q.Difficulty}
	}
	return json.Marshal(out)
}

func decodeTemplateQuestions(raw []byte) ([]models.SeedQuestion, error) {
	var in []templateQuestion
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]models.SeedQuestion, len(in))
	for i, q := range in {
		if q.Correct < 0 || q.Correct > 3 {
			return nil, fmt.Errorf("template question %d: bad correct index %d", i+1, q.Correct)
		}
		out[i] = models.SeedQuestion{
			Text: q.Text, Options: q.Options, Correct: q.Correct,
			Topic: q.Topic, Difficulty: q.Difficulty,
			// Templates are written only after the full validation +
			// quality audit + repair of a generated test.
			QualityChecked: true,
		}
	}
	return out, nil
}

// SaveTemplate stores the content of a freshly generated test under its
// fingerprint. Several variants per fingerprint are allowed (a user who
// already received variant 1 and still has the same weak topics triggers a
// new generation, which becomes variant 2 for the next users).
func (r *GenerationRepository) SaveTemplate(ctx context.Context, t *TestTemplate) (int64, error) {
	if t.Fingerprint == "" || len(t.Questions) == 0 {
		return 0, errors.New("template without fingerprint or questions")
	}
	qs, err := encodeTemplateQuestions(t.Questions)
	if err != nil {
		return 0, err
	}
	topics, err := json.Marshal(t.Topics)
	if err != nil {
		return 0, err
	}
	var src any
	if t.SourceTestID > 0 {
		src = t.SourceTestID
	}
	var id int64
	err = r.pool.QueryRow(ctx, `
		INSERT INTO test_templates (subject_id, kind, fingerprint, test_number, topics, questions, source_test_id, strategy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		t.SubjectID, t.Kind, t.Fingerprint, t.TestNumber, topics, qs, src, t.Strategy).Scan(&id)
	if err != nil {
		return 0, err
	}
	t.ID = id
	return id, nil
}

// FindTemplate returns the newest template of (subject, kind, fingerprint)
// the user has NOT received yet (userID 0 = any user, e.g. the chain).
// nil when there is none.
func (r *GenerationRepository) FindTemplate(ctx context.Context, subjectID int64, kind, fingerprint string, userID int64) (*TestTemplate, error) {
	var t TestTemplate
	var topics, qs []byte
	var src *int64
	err := r.pool.QueryRow(ctx, `
		SELECT t.id, t.subject_id, t.kind, t.fingerprint, t.test_number, t.topics, t.questions,
		       t.source_test_id, t.strategy, t.uses
		FROM test_templates t
		WHERE t.subject_id = $1 AND t.kind = $2 AND t.fingerprint = $3
		  AND ($4::bigint = 0 OR NOT EXISTS (
		        SELECT 1 FROM test_template_uses u WHERE u.template_id = t.id AND u.user_id = $4))
		ORDER BY t.id DESC
		LIMIT 1`, subjectID, kind, fingerprint, userID).
		Scan(&t.ID, &t.SubjectID, &t.Kind, &t.Fingerprint, &t.TestNumber, &topics, &qs, &src, &t.Strategy, &t.Uses)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if src != nil {
		t.SourceTestID = *src
	}
	if len(topics) > 0 {
		if err := json.Unmarshal(topics, &t.Topics); err != nil {
			return nil, err
		}
	}
	if t.Questions, err = decodeTemplateQuestions(qs); err != nil {
		return nil, err
	}
	return &t, nil
}

// MarkTemplateUsed records that the user received the template (idempotent)
// and bumps its usage counter.
func (r *GenerationRepository) MarkTemplateUsed(ctx context.Context, templateID, userID, testID int64) error {
	if userID > 0 {
		var tid any
		if testID > 0 {
			tid = testID
		}
		if _, err := r.pool.Exec(ctx, `
			INSERT INTO test_template_uses (template_id, user_id, test_id)
			VALUES ($1, $2, $3)
			ON CONFLICT (template_id, user_id) DO NOTHING`, templateID, userID, tid); err != nil {
			return err
		}
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE test_templates SET uses = uses + 1, last_used_at = now() WHERE id = $1`, templateID)
	return err
}

// ClonePersonalFromTemplate creates the user's personal test as a CLONE of
// the template: a new tests row (kind personal, owner = user, the template
// fingerprint) with NEW question rows of identical content. Progress on
// clone questions is therefore strictly personal, and «🏁 Закончить тест»
// deletes the clone's rows like any generated personal test. The (subject,
// owner) uniqueness is handled by createTest (the loser of a race gets the
// winner's test). Returns the stored test and the new question ids in
// order (to carry over cached translations).
func (r *GenerationRepository) ClonePersonalFromTemplate(ctx context.Context, tpl *TestTemplate, userID int64, title string) (*models.Test, []int64, error) {
	test := &models.Test{
		SubjectID:         tpl.SubjectID,
		TestNumber:        0,
		Title:             title,
		Kind:              models.TestKindPersonal,
		Topics:            tpl.Topics,
		OwnerUserID:       userID,
		TopicsFingerprint: tpl.Fingerprint,
		OriginTestID:      tpl.SourceTestID,
	}
	stored, err := r.CreateGeneratedTest(ctx, test, tpl.Questions)
	if err != nil {
		return nil, nil, err
	}
	ids, err := r.testQuestionIDs(ctx, stored.ID)
	if err != nil {
		return nil, nil, err
	}
	return stored, ids, nil
}

// CloneChainFromTemplate stores chain test number tpl.TestNumber from the
// template (new question rows). The UNIQUE (subject_id, test_number)
// constraint keeps it idempotent.
func (r *GenerationRepository) CloneChainFromTemplate(ctx context.Context, tpl *TestTemplate, testNumber int, title string) (*models.Test, error) {
	test := &models.Test{
		SubjectID:         tpl.SubjectID,
		TestNumber:        testNumber,
		Title:             title,
		Kind:              models.TestKindChain,
		Topics:            tpl.Topics,
		TopicsFingerprint: tpl.Fingerprint,
	}
	return r.CreateGeneratedTest(ctx, test, tpl.Questions)
}

func (r *GenerationRepository) testQuestionIDs(ctx context.Context, testID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `SELECT question_id FROM test_questions WHERE test_id = $1 ORDER BY position`, testID)
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

// DeleteUnusedTestTemplates removes up to limit templates that were neither
// created nor used during olderThan (bounded table growth).
func (c *CleanupRepository) DeleteUnusedTestTemplates(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	tag, err := c.pool.Exec(ctx, `
		DELETE FROM test_templates
		WHERE id IN (
			SELECT id FROM test_templates
			WHERE COALESCE(last_used_at, created_at) < now() - make_interval(secs => $1)
			ORDER BY id
			LIMIT $2)`, durationSecs(olderThan), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

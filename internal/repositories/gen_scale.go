package repositories

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bihan293/Juz40tester/internal/models"
)

// ---------------------------------------------------------------------------
// Batch reuse (gen_job_batches)
// ---------------------------------------------------------------------------

// SaveJobBatch persists one validated batch of a running job, so a retry of
// the job (failure, timeout, deploy) reuses it instead of paying again.
// Idempotent per (job, slot): the newest batch wins.
func (r *GenerationRepository) SaveJobBatch(ctx context.Context, jobID int64, slot int, qs []models.SeedQuestion, provider string) error {
	raw, err := encodeTemplateQuestions(qs)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO gen_job_batches (job_id, slot, questions, provider)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (job_id, slot) DO UPDATE SET questions = EXCLUDED.questions,
			provider = EXCLUDED.provider, created_at = now()`, jobID, slot, raw, provider)
	return err
}

// SaveJobSlots persists accepted questions of a running job, one row per
// slot (slot -> one question), in a single round trip.
func (r *GenerationRepository) SaveJobSlots(ctx context.Context, jobID int64, qs map[int]models.SeedQuestion, provider string) error {
	if len(qs) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for slot, q := range qs {
		raw, err := encodeTemplateQuestions([]models.SeedQuestion{q})
		if err != nil {
			return err
		}
		b.Queue(`
			INSERT INTO gen_job_batches (job_id, slot, questions, provider)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (job_id, slot) DO UPDATE SET questions = EXCLUDED.questions,
				provider = EXCLUDED.provider, created_at = now()`, jobID, slot, raw, provider)
	}
	return r.pool.SendBatch(ctx, b).Close()
}

// JobBatches returns the stored batches of a job, by slot (empty map when
// nothing was stored).
func (r *GenerationRepository) JobBatches(ctx context.Context, jobID int64) (map[int][]models.SeedQuestion, error) {
	rows, err := r.pool.Query(ctx, `SELECT slot, questions FROM gen_job_batches WHERE job_id = $1 ORDER BY slot`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]models.SeedQuestion{}
	for rows.Next() {
		var slot int
		var raw []byte
		if err := rows.Scan(&slot, &raw); err != nil {
			return nil, err
		}
		qs, err := decodeTemplateQuestions(raw)
		if err != nil {
			continue // a broken row is simply regenerated
		}
		out[slot] = qs
	}
	return out, rows.Err()
}

// DeleteJobBatches drops the stored batches of a finished job.
func (r *GenerationRepository) DeleteJobBatches(ctx context.Context, jobID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM gen_job_batches WHERE job_id = $1`, jobID)
	return err
}

// ---------------------------------------------------------------------------
// A/B strategies
// ---------------------------------------------------------------------------

// SetJobStrategy records which generation strategy a job runs with.
func (r *GenerationRepository) SetJobStrategy(ctx context.Context, jobID int64, strategy string) error {
	_, err := r.pool.Exec(ctx, `UPDATE generation_jobs SET strategy = $2 WHERE id = $1`, jobID, strategy)
	return err
}

// StrategyOutcome is the result of one job, aggregated per day/strategy/kind.
type StrategyOutcome struct {
	Strategy             string
	Kind                 string
	Done                 bool
	AICalls              int
	AIRejects            int
	DifficultyViolations int
	RepeatRejects        int
	TemplateHit          bool
	PromptTokens         int
	CompletionTokens     int
	Duration             time.Duration
}

// RecordStrategyOutcome adds one job outcome to gen_strategy_stats.
func (r *GenerationRepository) RecordStrategyOutcome(ctx context.Context, day time.Time, o StrategyOutcome) error {
	done, failed, hit := 0, 1, 0
	if o.Done {
		done, failed = 1, 0
	}
	if o.TemplateHit {
		hit = 1
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO gen_strategy_stats (day, strategy, kind, jobs_done, jobs_failed, ai_calls, ai_rejects,
		                                difficulty_violations, repeat_rejects, template_hits,
		                                prompt_tokens, completion_tokens, gen_ms)
		VALUES ($1::date, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (day, strategy, kind) DO UPDATE SET
			jobs_done             = gen_strategy_stats.jobs_done + EXCLUDED.jobs_done,
			jobs_failed           = gen_strategy_stats.jobs_failed + EXCLUDED.jobs_failed,
			ai_calls              = gen_strategy_stats.ai_calls + EXCLUDED.ai_calls,
			ai_rejects            = gen_strategy_stats.ai_rejects + EXCLUDED.ai_rejects,
			difficulty_violations = gen_strategy_stats.difficulty_violations + EXCLUDED.difficulty_violations,
			repeat_rejects        = gen_strategy_stats.repeat_rejects + EXCLUDED.repeat_rejects,
			template_hits         = gen_strategy_stats.template_hits + EXCLUDED.template_hits,
			prompt_tokens         = gen_strategy_stats.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens     = gen_strategy_stats.completion_tokens + EXCLUDED.completion_tokens,
			gen_ms                = gen_strategy_stats.gen_ms + EXCLUDED.gen_ms`,
		day.UTC().Format("2006-01-02"), o.Strategy, o.Kind, done, failed, o.AICalls, o.AIRejects,
		o.DifficultyViolations, o.RepeatRejects, hit, o.PromptTokens, o.CompletionTokens, o.Duration.Milliseconds())
	return err
}

// StrategyStatsRow is one row of gen_strategy_stats.
type StrategyStatsRow struct {
	Day                  time.Time
	Strategy, Kind       string
	JobsDone, JobsFailed int
	AICalls, AIRejects   int
	DifficultyViolations int
	TemplateHits         int
}

// StrategyStats returns the per-day strategy outcomes since `since`.
func (r *GenerationRepository) StrategyStats(ctx context.Context, since time.Time) ([]StrategyStatsRow, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT day, strategy, kind, jobs_done, jobs_failed, ai_calls, ai_rejects, difficulty_violations, template_hits
		FROM gen_strategy_stats WHERE day >= $1::date ORDER BY day, strategy, kind`, since.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StrategyStatsRow
	for rows.Next() {
		var s StrategyStatsRow
		if err := rows.Scan(&s.Day, &s.Strategy, &s.Kind, &s.JobsDone, &s.JobsFailed, &s.AICalls, &s.AIRejects, &s.DifficultyViolations, &s.TemplateHits); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Load protection
// ---------------------------------------------------------------------------

// CountActiveJobs returns how many jobs of the kind are pending or running
// (served by the partial index idx_genjobs_active_kind).
func (r *GenerationRepository) CountActiveJobs(ctx context.Context, kind string) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM generation_jobs
		WHERE kind = $1 AND status IN ('pending','running')`, kind).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------------------
// Subject-wide weak topics (chain generation)
// ---------------------------------------------------------------------------

// SubjectWeakTopic is a topic that is weak for a noticeable share of the
// subject's active students.
type SubjectWeakTopic struct {
	Key       string
	Title     string
	Users     int     // students with enough answers in the window
	WeakUsers int     // of them, with the topic 🔴/🟡
	WeakShare float64 // WeakUsers / Users
}

// subjectWeakTopicsSQL classifies every (user, topic) row of the subject
// touched within the window exactly like models.TopicStat.Level (last
// TopicWindow answers, ≥ TopicMinAnswers answers, ≥ TopicMinWrong wrong and
// < TopicOKFrom % correct) and aggregates per topic. Only rows updated
// recently count (the current cohort), served by
// idx_user_topic_stats_subject_updated.
const subjectWeakTopicsSQL = `
	WITH w AS (
		SELECT topic_key, topic, right(recent, $3) AS r
		FROM user_topic_stats
		WHERE subject_id = $1 AND updated_at > now() - make_interval(secs => $2)
	), c AS (
		SELECT topic_key, topic, length(r) AS n,
		       length(r) - length(replace(r, '1', '')) AS ok
		FROM w
	), agg AS (
		SELECT topic_key,
		       max(topic) AS topic,
		       COUNT(*) FILTER (WHERE n >= $4) AS users,
		       COUNT(*) FILTER (WHERE n >= $4 AND n - ok >= $5 AND ok * 100 < $6 * n) AS weak_users
		FROM c
		GROUP BY topic_key
	)
	SELECT agg.topic_key, COALESCE(st.title, agg.topic), agg.users, agg.weak_users
	FROM agg
	LEFT JOIN subject_topics st ON st.subject_id = $1 AND st.topic_key = agg.topic_key
	WHERE agg.users >= $7 AND agg.weak_users > 0
	ORDER BY agg.weak_users::float / agg.users DESC, agg.weak_users DESC, agg.topic_key
	LIMIT $8`

// SubjectWeakTopics returns the subject's topics that are weak for the
// largest share of students active within `window` (worst first). Topics
// with fewer than minUsers classified students are ignored (noise).
func (r *GenerationRepository) SubjectWeakTopics(ctx context.Context, subjectID int64, window time.Duration, minUsers, limit int) ([]SubjectWeakTopic, error) {
	if limit <= 0 {
		limit = 5
	}
	if minUsers <= 0 {
		minUsers = 1
	}
	rows, err := r.pool.Query(ctx, subjectWeakTopicsSQL, subjectID, durationSecs(window),
		models.TopicWindow, models.TopicMinAnswers, models.TopicMinWrong, models.TopicOKFrom, minUsers, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SubjectWeakTopic
	for rows.Next() {
		var s SubjectWeakTopic
		if err := rows.Scan(&s.Key, &s.Title, &s.Users, &s.WeakUsers); err != nil {
			return nil, err
		}
		if s.Users > 0 {
			s.WeakShare = float64(s.WeakUsers) / float64(s.Users)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

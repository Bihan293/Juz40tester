-- Generation at scale: fingerprint templates, batch reuse, strategy stats,
-- indexes for subject-wide topic aggregation and queue backpressure.
-- Only new objects / IF NOT EXISTS — safe on a database that already has
-- 000001…000023 applied, nothing existing is altered destructively.

-- test_templates — the validated content of an AI-generated test, keyed by
-- the fingerprint of its INPUT (weak-topic set of a personal test; chain
-- slot + previous test + weak topics of a chain test). A further user with
-- the same fingerprint gets a CLONE: a new tests row with NEW question rows
-- of identical content (no AI call). The content is self-contained JSON, so
-- the template survives the deletion of the test it came from.
CREATE TABLE IF NOT EXISTS test_templates (
    id             BIGSERIAL   PRIMARY KEY,
    subject_id     BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    kind           TEXT        NOT NULL CHECK (kind IN ('chain','personal')),
    fingerprint    TEXT        NOT NULL,
    test_number    INT         NOT NULL DEFAULT 0,
    topics         JSONB       NOT NULL DEFAULT '[]',
    questions      JSONB       NOT NULL,
    source_test_id BIGINT,
    strategy       TEXT        NOT NULL DEFAULT '',
    uses           INT         NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_test_templates_lookup
    ON test_templates(subject_id, kind, fingerprint, id DESC);

-- Which user already received which template: a user never gets the same
-- cloned content twice (the next request falls through to a new variant).
CREATE TABLE IF NOT EXISTS test_template_uses (
    template_id BIGINT      NOT NULL REFERENCES test_templates(id) ON DELETE CASCADE,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    test_id     BIGINT,
    used_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (template_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_test_template_uses_user ON test_template_uses(user_id);

-- gen_job_batches — validated question batches of a running job. A retried
-- job (failure, timeout, deploy) reuses its finished batches instead of
-- paying for them again. Rows go away with the job (cleanup of old jobs).
CREATE TABLE IF NOT EXISTS gen_job_batches (
    job_id     BIGINT      NOT NULL REFERENCES generation_jobs(id) ON DELETE CASCADE,
    slot       INT         NOT NULL,
    questions  JSONB       NOT NULL,
    provider   TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, slot)
);

-- A/B testing of generation strategies: which strategy a job used.
ALTER TABLE generation_jobs ADD COLUMN IF NOT EXISTS strategy TEXT NOT NULL DEFAULT '';

-- Durable per-day outcome of every generation strategy (A/B analysis).
CREATE TABLE IF NOT EXISTS gen_strategy_stats (
    day                   DATE   NOT NULL,
    strategy              TEXT   NOT NULL,
    kind                  TEXT   NOT NULL,
    jobs_done             INT    NOT NULL DEFAULT 0,
    jobs_failed           INT    NOT NULL DEFAULT 0,
    ai_calls              INT    NOT NULL DEFAULT 0,
    ai_rejects            INT    NOT NULL DEFAULT 0,
    difficulty_violations INT    NOT NULL DEFAULT 0,
    repeat_rejects        INT    NOT NULL DEFAULT 0,
    template_hits         INT    NOT NULL DEFAULT 0,
    prompt_tokens         BIGINT NOT NULL DEFAULT 0,
    completion_tokens     BIGINT NOT NULL DEFAULT 0,
    gen_ms                BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, strategy, kind)
);

-- Subject-wide weak-topic aggregation for chain generation
-- (SubjectWeakTopics: WHERE subject_id = $1 AND updated_at > …). The PK
-- (user_id, subject_id, topic_key) cannot serve a lookup by subject.
CREATE INDEX IF NOT EXISTS idx_user_topic_stats_subject_updated
    ON user_topic_stats(subject_id, updated_at);

-- Queue backpressure: COUNT of active jobs per kind without a scan.
CREATE INDEX IF NOT EXISTS idx_genjobs_active_kind
    ON generation_jobs(kind)
    WHERE status IN ('pending','running');

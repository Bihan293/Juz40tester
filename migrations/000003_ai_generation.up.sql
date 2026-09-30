-- AI test generation: test metadata, per-user subject state, generation jobs.
--
-- tests.kind:
--   'chain' — the main linear chain of tests (Тест 1, Тест 2, ...) generated
--             one after another with increasing difficulty;
--   'weak'  — personal tests built from the user's weak topics (cached per
--             topic fingerprint so identical weakness profiles share one
--             already-generated test).
-- tests.topics — the list of topics the test covers (filled by the
-- generator), used for weak-topic analysis and for the fingerprint cache.
ALTER TABLE tests
    ADD COLUMN IF NOT EXISTS kind   TEXT NOT NULL DEFAULT 'chain'
        CHECK (kind IN ('chain','weak')),
    ADD COLUMN IF NOT EXISTS topics JSONB NOT NULL DEFAULT '[]';

-- Fingerprint of the weak-topic set a 'weak' test was generated for
-- (sha256 of sorted unique topics). Two users with the same weak topics get
-- the same cached test. NULL for chain tests.
ALTER TABLE tests
    ADD COLUMN IF NOT EXISTS topics_fingerprint TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_tests_weak_fingerprint
    ON tests(subject_id, topics_fingerprint)
    WHERE kind = 'weak' AND topics_fingerprint IS NOT NULL;

-- Per-user UI/state data that must survive restarts and re-logins.
CREATE TABLE IF NOT EXISTS user_subject_state (
    user_id            BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject_id         BIGINT NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    tests_page         INT    NOT NULL DEFAULT 0,  -- last opened page of the tests grid
    last_test_number   INT    NOT NULL DEFAULT 0,  -- highest chain test the user completed
    requested_up_to    INT    NOT NULL DEFAULT 0,  -- chain tests queued/requested for generation
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, subject_id)
);

-- Queue of AI test-generation jobs. The background worker picks pending
-- jobs, generates the test (DeepSeek chat -> reasoner polish) and stores it.
-- kind: 'chain' (the next numbered test) or 'weak' (weak-topics test).
CREATE TABLE IF NOT EXISTS generation_jobs (
    id                 BIGSERIAL PRIMARY KEY,
    kind               TEXT NOT NULL CHECK (kind IN ('chain','weak')),
    subject_id         BIGINT NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    test_number        INT,                          -- chain jobs only
    topics_fingerprint TEXT,                         -- weak jobs only
    status             TEXT NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending','running','done','failed')),
    attempts           INT  NOT NULL DEFAULT 0,
    last_error         TEXT NOT NULL DEFAULT '',
    test_id            BIGINT REFERENCES tests(id) ON DELETE SET NULL,
    not_before         TIMESTAMPTZ NOT NULL DEFAULT now(),  -- off-peak deferral
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A chain test number is generated once per subject.
CREATE UNIQUE INDEX IF NOT EXISTS idx_genjobs_chain_unique
    ON generation_jobs(subject_id, test_number)
    WHERE kind = 'chain' AND status IN ('pending','running','done');

-- A weak-topics set is generated once per subject.
CREATE UNIQUE INDEX IF NOT EXISTS idx_genjobs_weak_unique
    ON generation_jobs(subject_id, topics_fingerprint)
    WHERE kind = 'weak' AND status IN ('pending','running','done');

CREATE INDEX IF NOT EXISTS idx_genjobs_pick
    ON generation_jobs(status, not_before)
    WHERE status = 'pending';

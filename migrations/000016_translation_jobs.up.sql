-- R-4: background translation queue.
--
-- Before: the Kazakh translation of a test ran SYNCHRONOUSLY inside the
-- Telegram update handler, serialised by an in-memory per-test mutex. 32
-- Kazakh students opening the same untranslated test blocked all 32 webhook
-- slots for up to a minute, and the lock was not shared between instances
-- (zero-downtime deploys run two).
--
-- Now a translation is a job in this table, processed by a background
-- worker (FOR UPDATE SKIP LOCKED claim, heartbeat, stuck-job reaper).
-- UNIQUE (test_id, lang) is the cross-instance guard: a test is translated
-- into a language by exactly one job row — never twice, on any instance.
-- A finished row is re-armed (status back to 'pending') only when the
-- translation turns out to be incomplete again (e.g. a question was
-- rewritten by the quality sweep, which drops its cached translation).
CREATE TABLE IF NOT EXISTS translation_jobs (
    id          BIGSERIAL PRIMARY KEY,
    test_id     BIGINT      NOT NULL REFERENCES tests(id) ON DELETE CASCADE,
    lang        TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','running','done','failed')),
    attempts    INT         NOT NULL DEFAULT 0,
    last_error  TEXT        NOT NULL DEFAULT '',
    not_before  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_trjobs_test_lang
    ON translation_jobs(test_id, lang);

-- Claim path: the oldest due pending job.
CREATE INDEX IF NOT EXISTS idx_trjobs_pick
    ON translation_jobs(not_before, id)
    WHERE status = 'pending';

-- Reaper path: running jobs whose heartbeat stopped.
CREATE INDEX IF NOT EXISTS idx_trjobs_running
    ON translation_jobs(updated_at)
    WHERE status = 'running';

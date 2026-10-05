-- A3: dead schema.
--
-- user_subject_state.requested_up_to is no longer read or written by code.
ALTER TABLE user_subject_state DROP COLUMN IF EXISTS requested_up_to;

-- Retention cleanup of finished attempts (ATTEMPT_TTL_DAYS).
CREATE INDEX IF NOT EXISTS idx_attempts_finished_updated
    ON test_attempts(updated_at)
    WHERE status IN ('completed','abandoned');

-- The legacy 'weak' kind: its indexes and CHECK values are removed ONLY when
-- no row of that kind exists (otherwise nothing changes and the legacy rows
-- keep working).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM tests WHERE kind = 'weak')
       AND NOT EXISTS (SELECT 1 FROM generation_jobs WHERE kind = 'weak') THEN
        DROP INDEX IF EXISTS idx_tests_weak_fingerprint;
        DROP INDEX IF EXISTS idx_genjobs_weak_unique;
        ALTER TABLE tests DROP CONSTRAINT IF EXISTS tests_kind_check;
        ALTER TABLE tests
            ADD CONSTRAINT tests_kind_check CHECK (kind IN ('chain','personal'));
        ALTER TABLE generation_jobs DROP CONSTRAINT IF EXISTS generation_jobs_kind_check;
        ALTER TABLE generation_jobs
            ADD CONSTRAINT generation_jobs_kind_check CHECK (kind IN ('chain','personal'));
    END IF;
END $$;

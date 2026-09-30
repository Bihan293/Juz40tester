-- Personal weak-topics tests + urgent generation jobs.
--
-- Weak-topics tests are no longer shared between users by a topic-set
-- fingerprint: every user gets their own test (kind = 'personal',
-- owner_user_id = the user). The user can FINISH such a test once it is
-- mastered (15 green + 5 yellow) — finishing deletes the test, so the next
-- "Слабые темы" run generates a fresh one.
ALTER TABLE tests DROP CONSTRAINT IF EXISTS tests_kind_check;
ALTER TABLE tests
    ADD CONSTRAINT tests_kind_check CHECK (kind IN ('chain','weak','personal'));

ALTER TABLE tests
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id) ON DELETE CASCADE;

-- One active personal test per user per subject: a finished personal test is
-- deleted, so a new one can always be generated.
CREATE UNIQUE INDEX IF NOT EXISTS idx_tests_personal_owner
    ON tests(subject_id, owner_user_id)
    WHERE kind = 'personal' AND owner_user_id IS NOT NULL;

-- generation_jobs.urgent: urgent jobs are pulled by the worker immediately
-- regardless of not_before (a test the user can already open must not wait
-- for the off-peak window). Deferred (non-urgent) jobs wait for off-peak as
-- before — that is where the API savings come from.
ALTER TABLE generation_jobs
    ADD COLUMN IF NOT EXISTS urgent BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS owner_user_id BIGINT REFERENCES users(id) ON DELETE CASCADE;

-- One queued/running personal-generation job per user per subject. 'done' is
-- deliberately NOT in the predicate: a finished personal test is deleted and
-- must be re-generatable, which requires a fresh job row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_genjobs_personal_unique
    ON generation_jobs(subject_id, owner_user_id)
    WHERE kind = 'personal' AND status IN ('pending','running');

CREATE INDEX IF NOT EXISTS idx_genjobs_pick_urgent
    ON generation_jobs(urgent, id)
    WHERE status = 'pending';

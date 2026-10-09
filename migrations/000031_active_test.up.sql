-- «Test mode»: while a test is being taken the main menu is hidden and no
-- other test / menu section can be opened. The state lives in the database
-- (the next update of the user may be handled by another instance —
-- ROLE=web / ROLE=worker), not in process memory.
--
-- users.active_attempt_id — the attempt the user is inside right now
-- (NULL = no test open). Set when a test is opened / resumed / answered,
-- cleared on «✅ Да, выйти» and when the test is finished. A pointer to an
-- attempt that is no longer 'in_progress' (reaped, restarted) is treated as
-- NULL and cleared lazily; a deleted attempt (finished personal / custom
-- test, cleanup) clears it through ON DELETE SET NULL.
--
-- The «paused» attempts (exit keeps an attempt in_progress so it can be
-- resumed with ⏸) are NOT active: only the one the user is inside is.
--
-- Additive only: a nullable column (no table rewrite, every existing row
-- is NULL = «not in a test», the old behaviour) and a partial index that
-- keeps ON DELETE SET NULL cheap for test_attempts deletes.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS active_attempt_id BIGINT
        REFERENCES test_attempts(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_users_active_attempt
    ON users (active_attempt_id) WHERE active_attempt_id IS NOT NULL;

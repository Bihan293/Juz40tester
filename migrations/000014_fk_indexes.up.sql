-- P0-3: indexes for foreign keys / hot lookups that previously had to scan
-- whole tables.
--
-- Why plain CREATE INDEX (not CONCURRENTLY): the migration runner applies
-- every file inside a transaction (and records it in schema_migrations in
-- the same transaction), and CREATE INDEX CONCURRENTLY cannot run inside a
-- transaction block. The tables are still small, so the short SHARE lock of
-- a regular build is acceptable. For a large production database the
-- operator can build the very same indexes beforehand, outside a
-- transaction, with
--     CREATE INDEX CONCURRENTLY IF NOT EXISTS <same name> ON <same def>;
-- — every statement below uses IF NOT EXISTS with identical names, so the
-- migration then becomes a no-op and takes no long locks.

-- attempt_questions.question_id: FK ON DELETE CASCADE from questions,
-- questionBusySQL (WHERE aq.question_id = $1 ...), question deletion.
-- The existing index covers only attempt_id.
CREATE INDEX IF NOT EXISTS idx_attempt_questions_question
    ON attempt_questions(question_id);

-- user_question_progress: PK is (user_id, question_id), useless for lookups
-- by question_id alone (FK cascade, AverageQuestionStatuses,
-- GreenLeaderboard, ReplaceQuestionContent). status is the second column so
-- AverageQuestionStatuses / GreenLeaderboard (status = 2) can be answered
-- from the index; the leading question_id still serves the FK cascade.
CREATE INDEX IF NOT EXISTS idx_uqp_question_status
    ON user_question_progress(question_id, status);

-- test_questions: PK is (test_id, question_id); lookups by question_id
-- (DeletePersonalTest, GreenLeaderboard join, FK cascade) need their own.
CREATE INDEX IF NOT EXISTS idx_test_questions_question
    ON test_questions(question_id);

-- test_attempts.test_id: FK ON DELETE CASCADE when a test is deleted. The
-- only existing index containing test_id is (user_id, test_id) partial on
-- in-progress attempts, which cannot serve it.
CREATE INDEX IF NOT EXISTS idx_attempts_test
    ON test_attempts(test_id);

-- FindPersonalTestByFingerprint:
--   WHERE subject_id = $1 AND kind = 'personal' AND topics_fingerprint = $2
--   ORDER BY id LIMIT 1
-- Partial on personal tests only (fingerprint = $2 implies NOT NULL); id is
-- appended so the ORDER BY id LIMIT 1 is satisfied by the index order.
CREATE INDEX IF NOT EXISTS idx_tests_personal_fingerprint
    ON tests(subject_id, topics_fingerprint, id)
    WHERE kind = 'personal' AND topics_fingerprint IS NOT NULL;

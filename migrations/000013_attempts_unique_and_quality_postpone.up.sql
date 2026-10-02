-- 1. Quality sweep: a flagged question that is on screen in an unfinished
--    attempt is postponed (no paid AI rewrite until the time passes) instead
--    of being rewritten and thrown away on every sweep tick.
ALTER TABLE questions
    ADD COLUMN IF NOT EXISTS quality_postponed_until TIMESTAMPTZ;

-- 2. At most ONE in-progress attempt per (user, test). Older duplicates
--    (double taps on a test / «Пройти ещё раз») are closed as abandoned —
--    the newest one is kept so the user resumes where they were.
UPDATE test_attempts a
SET status = 'abandoned'
WHERE a.status = 'in_progress'
  AND EXISTS (
      SELECT 1 FROM test_attempts b
      WHERE b.user_id = a.user_id AND b.test_id = a.test_id
        AND b.status = 'in_progress' AND b.id > a.id
  );

DROP INDEX IF EXISTS idx_attempts_in_progress;
CREATE UNIQUE INDEX IF NOT EXISTS idx_attempts_in_progress_unique
    ON test_attempts(user_id, test_id) WHERE status = 'in_progress';

-- 3. Activity timestamp of an attempt (the stale-attempt reaper closes
--    attempts untouched for a long time).
ALTER TABLE test_attempts
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- 4. Chain unlock watermark: last_test_number now means "highest chain test
--    that reached the unlock bar (15🟢 + 5🟡)" and is read by the unlock
--    logic. The old value was bumped on ANY completion, so it is recomputed
--    from the current knowledge marks: a test that meets the bar now was
--    necessarily open to the user, so it is a valid permanent watermark.
UPDATE user_subject_state SET last_test_number = 0;

INSERT INTO user_subject_state (user_id, subject_id, last_test_number)
SELECT p.user_id, t.subject_id, MAX(t.test_number)
FROM (
    SELECT p.user_id, tq.test_id,
           COUNT(*) FILTER (WHERE p.status = 2) AS green,
           COUNT(*) FILTER (WHERE p.status = 1) AS yellow
    FROM test_questions tq
    JOIN user_question_progress p ON p.question_id = tq.question_id
    GROUP BY p.user_id, tq.test_id
) p
JOIN tests t ON t.id = p.test_id AND t.kind = 'chain' AND t.is_active
WHERE p.green >= 15 AND p.green + p.yellow >= 20
GROUP BY p.user_id, t.subject_id
ON CONFLICT (user_id, subject_id) DO UPDATE SET
    last_test_number = EXCLUDED.last_test_number;

-- Personal weak-topics jobs: constraint fix + stuck-job recovery.
--
-- BUG FIX: migration 000005 added the 'personal' job kind and the
-- owner_user_id column to generation_jobs, but never extended the
-- generation_jobs_kind_check constraint, which still allowed only
-- ('chain','weak'). Every INSERT with kind='personal' therefore failed with
-- a check-violation at the database level — personal weak-topics tests
-- could never even be QUEUED (the UI showed «Минуточку...» forever).
ALTER TABLE generation_jobs DROP CONSTRAINT IF EXISTS generation_jobs_kind_check;
ALTER TABLE generation_jobs
    ADD CONSTRAINT generation_jobs_kind_check CHECK (kind IN ('chain','weak','personal'));

-- RECOVERY: jobs that were claimed by a worker which then died (deploy,
-- restart, OOM) stay 'running' forever — the affected tests would hang in
-- ⏳ forever. Anything running for over 10 minutes is by definition stuck
-- (a real generation takes ~1-2 minutes), so re-queue it. The worker also
-- reaps such jobs continuously at runtime; this is the one-time cleanup.
UPDATE generation_jobs
SET status = 'pending', not_before = now(), updated_at = now()
WHERE status = 'running'
  AND updated_at < now() - interval '10 minutes';

-- RECOVERY: chain tests the user can ALREADY open but whose generation job
-- exhausted all retries (status='failed') must be re-queued urgently —
-- otherwise the first tests of a subject stay in ⏳ forever. We can only
-- re-queue test numbers that are still missing from tests; the partial
-- unique index (pending/running/done) keeps this idempotent.
INSERT INTO generation_jobs (kind, subject_id, test_number, not_before, urgent)
SELECT 'chain', j.subject_id, j.test_number, now(), TRUE
FROM generation_jobs j
WHERE j.kind = 'chain'
  AND j.status = 'failed'
  AND j.test_number IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM tests t
      WHERE t.subject_id = j.subject_id AND t.test_number = j.test_number
  )
ON CONFLICT DO NOTHING;

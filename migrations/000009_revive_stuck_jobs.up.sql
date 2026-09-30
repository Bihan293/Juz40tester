-- One-time queue reanimation: tests stuck in ⏳ «Минуточку, тест
-- генерируется» forever (e.g. Тест 1 of «Русский язык»).
--
-- Root causes fixed in code together with this migration:
--   1. a panic inside the generation worker killed the worker goroutine —
--      with nothing left to consume the queue, every pending job waited
--      forever (the worker now recovers and keeps draining the queue);
--   2. an ALREADY-QUEUED deferred (non-urgent) job was never upgraded to
--      urgent when the test became openable, so the user waited for the
--      off-peak window (EnqueueChainJob now upgrades on conflict);
--   3. tapping the ⏳ button only showed a toast — a 'failed' job was never
--      re-enqueued (the tap now revives the job; this migration is the
--      one-time cleanup for everything stuck BEFORE the fix shipped);
--   4. reaped 'running' jobs kept their old deferral and went back to the
--      END of the off-peak queue (they are now re-queued as urgent).
--
-- This migration re-queues every abandoned job whose test is still missing.

-- (a) 'running' jobs abandoned by a dead worker (deploy/restart/OOM/panic):
-- a real generation takes ~1–2 minutes, so anything running for over 10
-- minutes is by definition stuck. Re-queued as URGENT — these tests were
-- already wanted (a worker was actively processing them).
UPDATE generation_jobs
SET status = 'pending', urgent = TRUE, not_before = now(), updated_at = now()
WHERE status = 'running'
  AND updated_at < now() - interval '10 minutes'
  AND (
      -- chain job whose test is still missing
      (kind = 'chain' AND NOT EXISTS (
          SELECT 1 FROM tests t
          WHERE t.subject_id = generation_jobs.subject_id
            AND t.test_number = generation_jobs.test_number
      ))
      OR
      -- personal job whose owner still has no personal test in the subject
      (kind = 'personal' AND NOT EXISTS (
          SELECT 1 FROM tests t
          WHERE t.subject_id = generation_jobs.subject_id
            AND t.kind = 'personal'
            AND t.owner_user_id = generation_jobs.owner_user_id
      ))
  );

-- (b) 'failed' jobs (all retries exhausted) whose test is still missing:
-- re-queued as fresh URGENT jobs with reset retry counters — the failure
-- cause (an API outage, the broken request format, ...) may be long gone,
-- and users are staring at a permanent ⏳. Updating the rows in place is
-- race-free: the partial unique indexes only cover active
-- (pending/running[/done]) rows, so flipping a failed row back to pending
-- can never collide with a concurrently inserted fresh job.
UPDATE generation_jobs
SET status = 'pending', urgent = TRUE, not_before = now(),
    attempts = 0, last_error = '', updated_at = now()
WHERE status = 'failed'
  AND (
      (kind = 'chain' AND test_number IS NOT NULL AND NOT EXISTS (
          SELECT 1 FROM tests t
          WHERE t.subject_id = generation_jobs.subject_id
            AND t.test_number = generation_jobs.test_number
      ))
      OR
      (kind = 'personal' AND owner_user_id IS NOT NULL AND NOT EXISTS (
          SELECT 1 FROM tests t
          WHERE t.subject_id = generation_jobs.subject_id
            AND t.kind = 'personal'
            AND t.owner_user_id = generation_jobs.owner_user_id
      ))
  );

-- (c) deferred (non-urgent) chain jobs whose time never comes fast enough
-- for a test the user can ALREADY open: anything pending for over an hour
-- is pulled to the head of the queue. The off-peak deferral still applies to
-- freshly queued pre-generation of LOCKED tests — only long-overdue jobs are
-- forced urgent here.
UPDATE generation_jobs
SET urgent = TRUE, not_before = now(), updated_at = now()
WHERE status = 'pending'
  AND NOT urgent
  AND updated_at < now() - interval '1 hour';

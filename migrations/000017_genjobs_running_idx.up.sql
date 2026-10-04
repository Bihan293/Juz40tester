-- R-5c: the stuck-job reaper (ResetStuckRunningJobs) filters on
-- status = 'running' AND updated_at < ...; this tiny partial index makes the
-- periodic UPDATE an index probe that touches nothing when no job is stuck.
CREATE INDEX IF NOT EXISTS idx_genjobs_running
    ON generation_jobs(updated_at)
    WHERE status = 'running';

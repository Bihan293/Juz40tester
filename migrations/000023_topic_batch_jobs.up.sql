-- B4 (part 1): 'topic_batch' generation jobs — ~10 bank questions on ONE
-- catalog topic of a subject (no test row).
--
-- generation_jobs.topic_key — the catalog topic of a topic_batch job.
-- idx_genjobs_topic_batch_unique — at most one ACTIVE batch per
--   (subject_id, topic_key): concurrent enqueues (ON CONFLICT DO NOTHING)
--   never create duplicate jobs and therefore duplicate questions.
ALTER TABLE generation_jobs ADD COLUMN IF NOT EXISTS topic_key TEXT;

DO $$
BEGIN
    ALTER TABLE generation_jobs DROP CONSTRAINT IF EXISTS generation_jobs_kind_check;
    IF EXISTS (SELECT 1 FROM generation_jobs WHERE kind = 'weak') THEN
        ALTER TABLE generation_jobs ADD CONSTRAINT generation_jobs_kind_check
            CHECK (kind IN ('chain','weak','personal','topic_batch'));
    ELSE
        ALTER TABLE generation_jobs ADD CONSTRAINT generation_jobs_kind_check
            CHECK (kind IN ('chain','personal','topic_batch'));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_genjobs_topic_batch_unique
    ON generation_jobs(subject_id, topic_key)
    WHERE kind = 'topic_batch' AND status IN ('pending','running');

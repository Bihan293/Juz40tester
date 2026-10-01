-- Post-generation quality audit of questions.
--
-- quality_checked_at — when the question last passed the local quality
-- audit (no answer revealed by the options' format, no duplicate options,
-- no catch-all options, ...). NULL = not audited yet: the background sweep
-- audits such questions and rewrites the flagged ones in place.
-- quality_attempts — failed repair attempts of a flagged question; the
-- sweep gives up after a few to never spend AI money on it forever.
ALTER TABLE questions
    ADD COLUMN IF NOT EXISTS quality_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS quality_attempts   INT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_questions_quality_unchecked
    ON questions(id) WHERE quality_checked_at IS NULL;

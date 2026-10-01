-- Weak topics are determined by the user's ACCUMULATED statistics per topic
-- of a subject — not by the status of individual questions.
--
-- user_topic_stats — one row per (user, subject, normalised topic):
--   correct_count / wrong_count — all-time counters;
--   recent — the last ≤10 answers of the topic, oldest first ('1' = correct,
--            '0' = wrong). The 🔴/🟡 level is computed from this window (see
--            models.TopicStat.Level), so fixed topics leave the weak list.
-- There is deliberately NO foreign key to questions: personal tests (and
-- their questions) are deleted on «🏁 Закончить тест», but the statistics of
-- the practice must survive. subject_id keeps subjects strictly separated.
CREATE TABLE IF NOT EXISTS user_topic_stats (
    user_id       BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject_id    BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    topic_key     TEXT        NOT NULL,
    topic         TEXT        NOT NULL,
    correct_count INT         NOT NULL DEFAULT 0,
    wrong_count   INT         NOT NULL DEFAULT 0,
    recent        TEXT        NOT NULL DEFAULT '',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, subject_id, topic_key)
);

-- Clone lineage of personal tests: a clone remembers the ROOT test it was
-- copied from, so a user who already finished a test of that lineage gets
-- freshly generated questions next time instead of the same clone again.
ALTER TABLE tests ADD COLUMN IF NOT EXISTS origin_test_id BIGINT;

CREATE TABLE IF NOT EXISTS user_personal_done (
    user_id      BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    root_test_id BIGINT      NOT NULL,
    subject_id   BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    finished_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, root_test_id)
);

-- One-off data backfills executed by the application (they need Go-side
-- topic normalisation). A row = the backfill has been done.
CREATE TABLE IF NOT EXISTS app_backfills (
    name    TEXT PRIMARY KEY,
    done_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

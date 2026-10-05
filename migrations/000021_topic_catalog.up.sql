-- B1: topic catalog of every subject + topic_key on questions.
--
-- subject_topics — canonical topics of a subject. topic_key is the
--   normalised form (models.NormalizeTopic) of the canonical title.
-- topic_aliases — every known normalised spelling of a topic (the canonical
--   one included) -> topic_key. ResolveTopicKey looks up here.
-- questions.topic_key — catalog topic of the question (NULL = not mapped).
--
-- The seed from existing questions and the topic_key backfill are done by
-- the application (Go-side normalisation, see BackfillTopicCatalog): SQL
-- lower() depends on the database ctype and may not lowercase Cyrillic.
CREATE TABLE IF NOT EXISTS subject_topics (
    subject_id BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    topic_key  TEXT        NOT NULL,
    title      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subject_id, topic_key)
);

CREATE TABLE IF NOT EXISTS topic_aliases (
    subject_id BIGINT NOT NULL,
    alias_norm TEXT   NOT NULL,
    topic_key  TEXT   NOT NULL,
    PRIMARY KEY (subject_id, alias_norm),
    FOREIGN KEY (subject_id, topic_key)
        REFERENCES subject_topics(subject_id, topic_key) ON DELETE CASCADE
);

ALTER TABLE questions ADD COLUMN IF NOT EXISTS topic_key TEXT;

CREATE INDEX IF NOT EXISTS idx_questions_subject_topic_key
    ON questions(subject_id, topic_key)
    WHERE topic_key IS NOT NULL;

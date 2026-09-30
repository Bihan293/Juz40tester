-- Test content language: users pick 🇷🇺 Русский or 🇰🇿 Қазақша in the
-- settings; the BOT INTERFACE stays in Russian, only the test content
-- (questions, options, topic names) is shown in the chosen language.
--
-- users.test_lang: 'ru' (default) or 'kk'.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS test_lang TEXT NOT NULL DEFAULT 'ru'
        CHECK (test_lang IN ('ru','kk'));

-- Kazakh translations of questions. The Russian question row is ALWAYS the
-- master version: a translation is produced once per question (DeepSeek
-- Flash), stored here and then reused by every user — no repeated API calls.
-- UNIQUE (question_id, lang) also makes concurrent translation runs safe:
-- the loser of the race simply re-reads the winner's row.
CREATE TABLE IF NOT EXISTS question_translations (
    id             BIGSERIAL PRIMARY KEY,
    question_id    BIGINT     NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
    lang           TEXT       NOT NULL CHECK (lang IN ('kk')),
    question_text  TEXT       NOT NULL,
    option_a       TEXT       NOT NULL,
    option_b       TEXT       NOT NULL,
    option_c       TEXT       NOT NULL,
    option_d       TEXT       NOT NULL,
    topic          TEXT       NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (question_id, lang)
);

CREATE INDEX IF NOT EXISTS idx_question_translations_q
    ON question_translations(question_id, lang);

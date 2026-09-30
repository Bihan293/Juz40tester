CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    telegram_id   BIGINT      NOT NULL UNIQUE,
    username      TEXT        NOT NULL DEFAULT '',
    first_name    TEXT        NOT NULL DEFAULT '',
    last_name     TEXT        NOT NULL DEFAULT '',
    language_code TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS subjects (
    id       BIGSERIAL PRIMARY KEY,
    name     TEXT NOT NULL UNIQUE,
    position INT  NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS questions (
    id             BIGSERIAL PRIMARY KEY,
    subject_id     BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    question_text  TEXT        NOT NULL,
    option_a       TEXT        NOT NULL,
    option_b       TEXT        NOT NULL,
    option_c       TEXT        NOT NULL,
    option_d       TEXT        NOT NULL,
    correct_answer CHAR(1)     NOT NULL CHECK (correct_answer IN ('A','B','C','D')),
    topic          TEXT        NOT NULL DEFAULT '',
    difficulty     INT         NOT NULL DEFAULT 1 CHECK (difficulty BETWEEN 1 AND 5)
);

CREATE INDEX IF NOT EXISTS idx_questions_subject ON questions(subject_id);

CREATE TABLE IF NOT EXISTS tests (
    id          BIGSERIAL PRIMARY KEY,
    subject_id  BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    test_number INT         NOT NULL DEFAULT 1,
    title       TEXT        NOT NULL,
    is_active   BOOLEAN     NOT NULL DEFAULT TRUE,
    UNIQUE (subject_id, test_number)
);

CREATE TABLE IF NOT EXISTS test_questions (
    test_id     BIGINT NOT NULL REFERENCES tests(id) ON DELETE CASCADE,
    question_id BIGINT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
    position    INT    NOT NULL,
    PRIMARY KEY (test_id, question_id)
);

CREATE INDEX IF NOT EXISTS idx_test_questions_test ON test_questions(test_id);

CREATE TABLE IF NOT EXISTS test_attempts (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    test_id          BIGINT      NOT NULL REFERENCES tests(id) ON DELETE CASCADE,
    status           TEXT        NOT NULL DEFAULT 'in_progress'
                     CHECK (status IN ('in_progress','completed','abandoned')),
    current_position INT         NOT NULL DEFAULT 0,
    correct_count    INT         NOT NULL DEFAULT 0,
    wrong_count      INT         NOT NULL DEFAULT 0,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_attempts_user ON test_attempts(user_id);
CREATE INDEX IF NOT EXISTS idx_attempts_in_progress
    ON test_attempts(user_id, test_id) WHERE status = 'in_progress';

CREATE TABLE IF NOT EXISTS attempt_questions (
    id              BIGSERIAL PRIMARY KEY,
    attempt_id      BIGINT   NOT NULL REFERENCES test_attempts(id) ON DELETE CASCADE,
    question_id     BIGINT   NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
    position        INT      NOT NULL,
    answered        BOOLEAN  NOT NULL DEFAULT FALSE,
    selected_answer CHAR(1),
    is_correct      BOOLEAN,
    option_order    JSONB    NOT NULL DEFAULT '[]',
    UNIQUE (attempt_id, position)
);

CREATE INDEX IF NOT EXISTS idx_attempt_questions_attempt ON attempt_questions(attempt_id);

CREATE TABLE IF NOT EXISTS user_question_progress (
    user_id       BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    question_id   BIGINT      NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
    status        INT         NOT NULL DEFAULT 0 CHECK (status IN (0,1,2)),
    correct_count INT         NOT NULL DEFAULT 0,
    wrong_count   INT         NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, question_id)
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

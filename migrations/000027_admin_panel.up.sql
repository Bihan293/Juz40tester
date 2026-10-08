-- Admin panel (docs/ADMIN.md): statistics, user search / subscription
-- changes, admin-bypass purchases and broadcasts. Only ADDITIVE changes
-- (new nullable/defaulted columns, new tables, new indexes) — safe on a
-- database with 000001…000026 applied; nothing existing is rewritten.

-- users.blocked_at — the user blocked the bot (Telegram 403 on a
-- broadcast). Such users are skipped by later broadcasts; the next message
-- or tap of the user clears the mark (UserRepository.Upsert).
ALTER TABLE users ADD COLUMN IF NOT EXISTS blocked_at TIMESTAMPTZ;

-- User search by @username (case-insensitive) and the statistics screen
-- (new users per day/week, active users).
CREATE INDEX IF NOT EXISTS idx_users_username_lower ON users (lower(username)) WHERE username <> '';
CREATE INDEX IF NOT EXISTS idx_users_created_at ON users (created_at);
CREATE INDEX IF NOT EXISTS idx_users_last_active ON users (last_active_date);

-- payments.admin_bypass — a «purchase» made by an administrator (ADMIN_IDS)
-- without an invoice and without real Stars (test mode of the payments).
-- Such rows are never refunded through Telegram and are excluded from the
-- revenue figures.
ALTER TABLE payments ADD COLUMN IF NOT EXISTS admin_bypass BOOLEAN NOT NULL DEFAULT FALSE;

-- admin_actions — the audit log of administrator actions (plan changes,
-- broadcasts, bypass purchases).
CREATE TABLE IF NOT EXISTS admin_actions (
    id             BIGSERIAL   PRIMARY KEY,
    admin_tg_id    BIGINT      NOT NULL,
    action         TEXT        NOT NULL,
    target_user_id BIGINT,
    details        TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_admin_actions_target ON admin_actions (target_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_actions_created ON admin_actions (created_at DESC);

-- admin_sessions — the step of a multi-step admin dialog (user search,
-- broadcast wizard). Kept in PostgreSQL, not in process memory: with web +
-- N workers the next message of the admin may be handled by another
-- instance.
CREATE TABLE IF NOT EXISTS admin_sessions (
    admin_tg_id BIGINT      PRIMARY KEY,
    state       TEXT        NOT NULL DEFAULT '',
    data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- broadcasts — one mass mailing.
--   draft    — being composed (preview shown, not confirmed);
--   queued   — confirmed, recipients materialised, waiting for a sender;
--   sending  — a worker is sending (lease: locked_by / locked_until);
--   done     — every recipient processed (counters final);
--   canceled — cancelled by an admin (pending recipients are skipped).
CREATE TABLE IF NOT EXISTS broadcasts (
    id            BIGSERIAL   PRIMARY KEY,
    admin_tg_id   BIGINT      NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','queued','sending','done','canceled')),
    text          TEXT        NOT NULL DEFAULT '',
    entities      JSONB,
    media_type    TEXT        NOT NULL DEFAULT ''
                  CHECK (media_type IN ('','photo','video')),
    media_file_id TEXT        NOT NULL DEFAULT '',
    buttons       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    total         INT         NOT NULL DEFAULT 0,
    sent          INT         NOT NULL DEFAULT 0,
    failed        INT         NOT NULL DEFAULT 0,
    blocked       INT         NOT NULL DEFAULT 0,
    locked_by     TEXT,
    locked_until  TIMESTAMPTZ,
    last_error    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued_at     TIMESTAMPTZ,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    canceled_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_broadcasts_active ON broadcasts (queued_at) WHERE status IN ('queued','sending');
CREATE INDEX IF NOT EXISTS idx_broadcasts_created ON broadcasts (created_at DESC);

-- broadcast_recipients — one row per (broadcast, user): the durable
-- progress. pending → sending (the HTTP call is in flight) → sent | failed
-- | blocked | canceled. A row is sent at most once: after a crash a row
-- left in 'sending' is NOT re-sent (it may have been delivered) but closed
-- as failed; a pending row (never attempted) is simply picked up again.
CREATE TABLE IF NOT EXISTS broadcast_recipients (
    broadcast_id BIGINT      NOT NULL REFERENCES broadcasts(id) ON DELETE CASCADE,
    user_id      BIGINT      NOT NULL,
    telegram_id  BIGINT      NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','sending','sent','failed','blocked','canceled')),
    attempts     INT         NOT NULL DEFAULT 0,
    next_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    error        TEXT        NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (broadcast_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_bc_recipients_pending
    ON broadcast_recipients (broadcast_id, next_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_bc_recipients_sending
    ON broadcast_recipients (broadcast_id, updated_at) WHERE status = 'sending';

-- Statistics: completed tests per day / week without scanning every attempt.
CREATE INDEX IF NOT EXISTS idx_attempts_completed_at ON test_attempts (completed_at) WHERE status = 'completed';
CREATE INDEX IF NOT EXISTS idx_attempts_started_at ON test_attempts (started_at);

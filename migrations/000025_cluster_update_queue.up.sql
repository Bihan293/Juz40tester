-- Cluster mode (ROLE=web|worker): durable Telegram update queue and the
-- registry of live instances. Only NEW objects (IF NOT EXISTS) — safe on a
-- database that already has 000001…000024 applied; nothing existing is
-- altered. With ROLE=all and QUEUE_BACKEND=memory (the defaults) these
-- tables simply stay empty.

-- tg_update_queue — every accepted Telegram update, until it is handled.
--   * update_id is the PRIMARY KEY: a re-delivered update (our 200 was lost,
--     a deploy, a second web instance) is a no-op INSERT … ON CONFLICT DO
--     NOTHING — idempotency lives in the database, not in process memory.
--     Handled rows are kept UPDATE_DONE_TTL_HOURS (48 h > Telegram's 24 h
--     re-delivery window) and then purged.
--   * user_key serialises the processing per user: a row is claimed only
--     when no EARLIER row of the same user is still pending/processing, so
--     two updates of one user are never handled in parallel and always in
--     update_id order (FOR UPDATE SKIP LOCKED keeps workers from fighting).
--   * status: pending → processing → done; a crashed handler goes back to
--     pending with backoff (available_at); after UPDATE_MAX_ATTEMPTS it is
--     'dead' (the dead-letter queue, kept UPDATE_DEAD_TTL_DAYS).
--   * heartbeat_at: refreshed by the worker while it handles the row; the
--     reaper returns rows with a stale heartbeat (worker died) to pending.
CREATE TABLE IF NOT EXISTS tg_update_queue (
    update_id    BIGINT      PRIMARY KEY,
    user_key     BIGINT      NOT NULL,
    payload      JSONB       NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','processing','done','dead')),
    attempts     INT         NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_by    TEXT,
    heartbeat_at TIMESTAMPTZ,
    last_error   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);

-- Claim: the oldest pending rows.
CREATE INDEX IF NOT EXISTS idx_tg_update_queue_pending
    ON tg_update_queue (update_id) WHERE status = 'pending';
-- Per-user ordering check («is there an earlier unfinished row of this user?»).
CREATE INDEX IF NOT EXISTS idx_tg_update_queue_user_open
    ON tg_update_queue (user_key, update_id) WHERE status IN ('pending','processing');
-- Reaper: processing rows by heartbeat.
CREATE INDEX IF NOT EXISTS idx_tg_update_queue_processing
    ON tg_update_queue (heartbeat_at) WHERE status = 'processing';
-- Purge of handled rows / dead letters.
CREATE INDEX IF NOT EXISTS idx_tg_update_queue_finished
    ON tg_update_queue (status, finished_at) WHERE status IN ('done','dead');

-- cluster_instances — live processes (heartbeat every 15 s). Used to split
-- TG_MAX_RPS between the instances that send Telegram messages
-- (RATELIMIT_BACKEND=postgres) and shown in /metrics. Rows of stopped
-- instances are removed on graceful shutdown and expire otherwise.
CREATE TABLE IF NOT EXISTS cluster_instances (
    id           TEXT        PRIMARY KEY,
    role         TEXT        NOT NULL,
    sends        BOOLEAN     NOT NULL DEFAULT TRUE,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cluster_instances_heartbeat
    ON cluster_instances (heartbeat_at);

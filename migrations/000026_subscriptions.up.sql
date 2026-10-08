-- Telegram Stars monetisation: monthly subscriptions (Free / Plus / Pro /
-- Premium), the daily quota of completed tests and paid weak-topics tests.
-- Only NEW objects (IF NOT EXISTS) — nothing existing is altered, so the
-- migration is safe on a database with 000001…000025 applied. With
-- SUBSCRIPTIONS_ENABLED=0 these tables simply stay empty.

-- user_subscriptions — the CURRENT plan of a user (one row per user). The
-- plan is in force while expires_at (+ SUB_GRACE_MIN) is in the future;
-- afterwards the user is on Free again — no job has to "switch it off"
-- (cancellation / expiry = the date simply passes).
--   sub_charge_id: telegram_payment_charge_id of the FIRST payment of the
--   Telegram subscription — the id editUserStarSubscription needs to cancel
--   the auto-renewal when the user upgrades.
CREATE TABLE IF NOT EXISTS user_subscriptions (
    user_id       BIGINT      PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    plan          TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','revoked')),
    expires_at    TIMESTAMPTZ NOT NULL,
    charge_id     TEXT,
    sub_charge_id TEXT,
    source        TEXT        NOT NULL DEFAULT 'stars'
                  CHECK (source IN ('stars','admin')),
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- payments — every successful Stars payment (the payment log). The
-- telegram_payment_charge_id is the PRIMARY KEY: a re-delivered
-- successful_payment (queue retry, Telegram re-delivery) is an
-- INSERT … ON CONFLICT DO NOTHING — a payment is applied exactly once.
--   status: paid → refund_pending → refunded (refunds are retried by the
--   billing reconciler until refundStarPayment succeeds).
CREATE TABLE IF NOT EXISTS payments (
    charge_id               TEXT        PRIMARY KEY,
    provider_charge_id      TEXT        NOT NULL DEFAULT '',
    user_id                 BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    telegram_user_id        BIGINT      NOT NULL,
    kind                    TEXT        NOT NULL CHECK (kind IN ('subscription','weak_test')),
    plan                    TEXT,
    order_id                BIGINT,
    amount                  INT         NOT NULL,
    currency                TEXT        NOT NULL,
    payload                 TEXT        NOT NULL,
    is_recurring            BOOLEAN     NOT NULL DEFAULT FALSE,
    is_first_recurring      BOOLEAN     NOT NULL DEFAULT FALSE,
    subscription_expires_at TIMESTAMPTZ,
    decision                TEXT        NOT NULL DEFAULT '',
    status                  TEXT        NOT NULL DEFAULT 'paid'
                            CHECK (status IN ('paid','refund_pending','refunded')),
    refund_reason           TEXT        NOT NULL DEFAULT '',
    refund_attempts         INT         NOT NULL DEFAULT 0,
    last_error              TEXT        NOT NULL DEFAULT '',
    next_check_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    refunded_at             TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payments_user ON payments(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_payments_refund_due ON payments(next_check_at)
    WHERE status = 'refund_pending';

-- weak_test_orders — one paid weak-topics test (10⭐ by default).
--   created   — invoice sent, not paid yet (re-used for 24 h: one open
--               invoice per user and subject);
--   paid      — successful_payment received; the reconciler builds the test
--               (bank / clone / AI generation) without a second payment,
--               retrying failed generations up to WEAK_ORDER_MAX_GEN times;
--   fulfilled — the test exists (test_id);
--   refunded  — generation failed / timed out: the payment is refunded
--               automatically (payments.status).
CREATE TABLE IF NOT EXISTS weak_test_orders (
    id               BIGSERIAL   PRIMARY KEY,
    user_id          BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    telegram_user_id BIGINT      NOT NULL,
    subject_id       BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    amount           INT         NOT NULL,
    status           TEXT        NOT NULL DEFAULT 'created'
                     CHECK (status IN ('created','paid','fulfilled','refunded','canceled')),
    charge_id        TEXT        UNIQUE,
    replace_test_id  BIGINT,
    test_id          BIGINT,
    gen_attempts     INT         NOT NULL DEFAULT 0,
    last_error       TEXT        NOT NULL DEFAULT '',
    next_check_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at          TIMESTAMPTZ,
    fulfilled_at     TIMESTAMPTZ,
    refunded_at      TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One open invoice per (user, subject): a repeated tap re-uses it.
CREATE UNIQUE INDEX IF NOT EXISTS idx_weak_orders_open
    ON weak_test_orders(user_id, subject_id) WHERE status = 'created';
-- One paid order in flight per (user, subject).
CREATE UNIQUE INDEX IF NOT EXISTS idx_weak_orders_inflight
    ON weak_test_orders(user_id, subject_id) WHERE status = 'paid';
CREATE INDEX IF NOT EXISTS idx_weak_orders_due
    ON weak_test_orders(next_check_at) WHERE status = 'paid';
CREATE INDEX IF NOT EXISTS idx_weak_orders_subject_paid
    ON weak_test_orders(subject_id) WHERE status = 'paid';

-- daily_usage — completed (charged) tests per user per quota day
-- (QUOTA_TZ calendar, Asia/Almaty by default). A new day is a new row, so
-- the limit "resets" at local midnight without any job.
CREATE TABLE IF NOT EXISTS daily_usage (
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day        DATE        NOT NULL,
    used       INT         NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);
CREATE INDEX IF NOT EXISTS idx_daily_usage_day ON daily_usage(day);

-- test_completions — the fact "attempt X was completed" (one row per
-- attempt, PRIMARY KEY attempt_id): the charge of the daily quota happens
-- in the same transaction as the final answer and at most once per
-- attempt, whatever retries / double taps happen. No foreign keys: the
-- record must survive the cleanup of old attempts and deleted personal
-- tests.
CREATE TABLE IF NOT EXISTS test_completions (
    attempt_id   BIGINT      PRIMARY KEY,
    user_id      BIGINT      NOT NULL,
    test_id      BIGINT      NOT NULL,
    day          DATE        NOT NULL,
    charged      BOOLEAN     NOT NULL,
    plan         TEXT        NOT NULL DEFAULT '',
    completed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_test_completions_user_day ON test_completions(user_id, day);
CREATE INDEX IF NOT EXISTS idx_test_completions_day ON test_completions(day);

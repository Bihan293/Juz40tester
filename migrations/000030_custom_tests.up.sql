-- «✨ Свой тест»: a student describes the test they want (≤ 500 chars), an
-- AI pre-check accepts or refuses the request (no charge on refusal), the
-- student pays CUSTOM_TEST_PRICE_STARS (15⭐ by default; free for ADMIN_IDS)
-- and the generator writes a personal 20-question test of kind 'custom'.
-- Like a weak-topics test it is finished («🏁 Закончить тест») at 15🟢 + 5🟡
-- and then deleted. Custom tests are isolated: no topic catalog / bank /
-- templates, no user_topic_stats, no chain unlocks, no daily quota.
--
-- Only additive changes (the kind CHECKs are widened to a superset of every
-- value an earlier migration allowed) — safe on a database with
-- 000001…000029 applied.

ALTER TABLE tests DROP CONSTRAINT IF EXISTS tests_kind_check;
ALTER TABLE tests
    ADD CONSTRAINT tests_kind_check CHECK (kind IN ('chain','weak','personal','custom'));

ALTER TABLE generation_jobs DROP CONSTRAINT IF EXISTS generation_jobs_kind_check;
ALTER TABLE generation_jobs
    ADD CONSTRAINT generation_jobs_kind_check CHECK (kind IN ('chain','weak','personal','topic_batch','custom'));

ALTER TABLE payments DROP CONSTRAINT IF EXISTS payments_kind_check;
ALTER TABLE payments
    ADD CONSTRAINT payments_kind_check CHECK (kind IN ('subscription','weak_test','custom_test'));

-- custom_test_orders — one requested custom test.
--   created   — the description passed the AI pre-check, invoice sent (one
--               open order per user: a new description cancels the old one);
--   paid      — paid (or free: admin / price 0); the generation job runs.
--               At most ONE paid order per user (one generation in flight);
--   fulfilled — the test exists (test_id);
--   refunded  — the generation failed / timed out: the payment is refunded
--               automatically (exactly once — payments.status);
--   canceled  — replaced by a newer description before payment.
CREATE TABLE IF NOT EXISTS custom_test_orders (
    id               BIGSERIAL   PRIMARY KEY,
    user_id          BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    telegram_user_id BIGINT      NOT NULL,
    subject_id       BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    amount           INT         NOT NULL,
    description      TEXT        NOT NULL,
    title            TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL DEFAULT 'created'
                     CHECK (status IN ('created','paid','fulfilled','refunded','canceled')),
    charge_id        TEXT        UNIQUE,
    free             BOOLEAN     NOT NULL DEFAULT FALSE,
    job_id           BIGINT,
    test_id          BIGINT,
    last_error       TEXT        NOT NULL DEFAULT '',
    next_check_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at          TIMESTAMPTZ,
    fulfilled_at     TIMESTAMPTZ,
    refunded_at      TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One generation in flight per user (double payment / spam guard).
CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_orders_inflight
    ON custom_test_orders(user_id) WHERE status = 'paid';
CREATE INDEX IF NOT EXISTS idx_custom_orders_due
    ON custom_test_orders(next_check_at) WHERE status = 'paid';
CREATE INDEX IF NOT EXISTS idx_custom_orders_user
    ON custom_test_orders(user_id, status);

-- custom_test_drafts — «waiting for the description» step of a user (the
-- next text message is the description). In PostgreSQL, not in memory: the
-- next message may be handled by another instance.
CREATE TABLE IF NOT EXISTS custom_test_drafts (
    user_id    BIGINT      PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    subject_id BIGINT      NOT NULL REFERENCES subjects(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- generation_jobs.custom_order_id — the order a 'custom' job generates for
-- (one job per order).
ALTER TABLE generation_jobs ADD COLUMN IF NOT EXISTS custom_order_id BIGINT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_genjobs_custom_order
    ON generation_jobs(custom_order_id) WHERE custom_order_id IS NOT NULL;

-- tests.custom_order_id — the order a custom test was generated for: a
-- retried job never stores a second test for the same order.
ALTER TABLE tests ADD COLUMN IF NOT EXISTS custom_order_id BIGINT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_tests_custom_order
    ON tests(custom_order_id) WHERE custom_order_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tests_custom_owner
    ON tests(owner_user_id, subject_id) WHERE kind = 'custom';

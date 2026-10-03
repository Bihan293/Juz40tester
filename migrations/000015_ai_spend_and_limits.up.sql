-- R-9: per-day DeepSeek spending ledger + index for the per-user daily
-- personal-generation limit.
--
-- ai_spend_daily is fed by the SAME cost estimate the DeepSeek client
-- already logs for every call (deepseek.estimateCost): before a call a
-- worst-case amount is RESERVED (atomic UPDATE ... WHERE spent + reserved +
-- x <= cap), after the call the reservation is replaced by the estimated
-- real cost. The cap is therefore strict across goroutines AND instances.
CREATE TABLE IF NOT EXISTS ai_spend_daily (
    provider     TEXT             NOT NULL,
    day          DATE             NOT NULL,          -- UTC calendar day
    cost_usd     DOUBLE PRECISION NOT NULL DEFAULT 0, -- settled estimated cost
    reserved_usd DOUBLE PRECISION NOT NULL DEFAULT 0, -- in-flight reservations
    calls        INT              NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, day)
);

-- PersonalJobsSince: COUNT(*) of a user's personal generation jobs created
-- since the start of the day.
CREATE INDEX IF NOT EXISTS idx_genjobs_personal_owner_created
    ON generation_jobs(owner_user_id, created_at)
    WHERE kind = 'personal';

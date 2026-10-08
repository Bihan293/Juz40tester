-- Backoff for chain-test generations that keep failing. Only an ADDITIVE
-- column with a default — safe on a database with 000001…000028 applied.
--
--   revivals — how many times a FAILED chain job (all its attempts spent)
--     was put back into the queue (subject screen open, ⏳ tap, next-test
--     unlock). Every re-queue after the first one waits an exponentially
--     growing pause after the last failure (30 min, 1 h, 2 h … capped at
--     12 h), so a test whose generation fails every time no longer starts a
--     new round of paid AI attempts on each open of the subject screen.
ALTER TABLE generation_jobs ADD COLUMN IF NOT EXISTS revivals INT NOT NULL DEFAULT 0;

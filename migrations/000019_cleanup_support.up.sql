-- A3: data needed by the background cleanup.
--
-- tests.created_at: the age of ownerless personal-test templates
-- (TEMPLATE_TTL_DAYS). Existing rows get the migration time, so old
-- templates are removed only TEMPLATE_TTL_DAYS after this deploy.
ALTER TABLE tests ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS idx_tests_personal_templates
    ON tests(created_at)
    WHERE kind = 'personal' AND owner_user_id IS NULL AND NOT is_active;

-- Lineage lookups of the user_personal_done orphan cleanup.
CREATE INDEX IF NOT EXISTS idx_tests_origin
    ON tests(origin_test_id)
    WHERE origin_test_id IS NOT NULL;

-- B3: personal tests assembled from the question bank.
--
-- tests.from_bank — the test links EXISTING bank questions (no own rows):
--   «🏁 Закончить тест» deletes only the test row; the questions and the
--   user's progress on them stay (progress = «already seen»).
-- personal_test_number_seq — personal test numbers without a per-subject
--   advisory lock + MAX(test_number)+1. Starts far above chain numbers
--   (<= 200) and the legacy personal numbers (9000+).
ALTER TABLE tests ADD COLUMN IF NOT EXISTS from_bank BOOLEAN NOT NULL DEFAULT FALSE;

CREATE SEQUENCE IF NOT EXISTS personal_test_number_seq START WITH 1000000;
SELECT setval('personal_test_number_seq',
              GREATEST(1000000, (SELECT COALESCE(MAX(test_number), 0) + 1 FROM tests)),
              false)
WHERE NOT EXISTS (SELECT 1 FROM tests WHERE test_number >= 1000000);

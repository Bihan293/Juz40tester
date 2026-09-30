-- Clean legacy subject seed. The bot now uses a single stub subject
-- ("Биология" with 3 placeholder tests of 20 questions each) seeded
-- programmatically on startup. Deleting subjects cascades to questions,
-- tests, test_questions, test_attempts, attempt_questions and
-- user_question_progress, so any previously seeded data is wiped and the
-- stub seed always recreates a consistent baseline.
DELETE FROM subjects;

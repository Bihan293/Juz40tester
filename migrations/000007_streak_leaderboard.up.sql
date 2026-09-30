-- Daily streak ("огонёк") for the leaderboard.
--
-- The streak counts consecutive CALENDAR days (UTC) with any bot activity:
-- a message or a button tap. It grows by 1 per day, survives a first visit
-- (day 1 -> streak 1, the flame shows from day 2), and resets to 1 after a
-- missed day. One extra UPDATE per user per day — negligible DB load.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS streak_days  INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_active_date DATE;

-- Leaderboard lookup: unlocked tests per (user, subject) is derived from
-- user_question_progress + test_questions, which are already indexed by
-- their primary keys — no extra indexes needed at this scale.

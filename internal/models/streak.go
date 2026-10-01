package models

import "time"

// StreakLocation is the calendar the daily streak (🔥 «огонёк») is counted
// in. The bot serves students in Kazakhstan, which uses a single UTC+5 zone
// nationwide since 2024-03-01. A fixed zone is used on purpose: it does not
// depend on the tzdata of the container or of the database. Previously the
// streak used the database CURRENT_DATE (UTC on Neon), so the «day» switched
// at 05:00 local time and a session after midnight counted as yesterday.
var StreakLocation = time.FixedZone("UTC+5", 5*60*60)

// StreakToday returns the current streak calendar date (midnight, UTC-based
// time.Time so it maps 1:1 to a PostgreSQL DATE parameter).
func StreakToday(now time.Time) time.Time {
	y, m, d := now.In(StreakLocation).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// NextStreak applies the streak rules to one activity on day today:
//   - first activity ever (no last date): 1;
//   - same day: unchanged;
//   - the next calendar day: +1;
//   - a missed day: reset to 1;
//   - an out-of-order update dated before the last activity: unchanged.
//
// It mirrors the SQL in UserRepository.Upsert (kept there for atomicity).
func NextStreak(streak int, lastActive, today time.Time) int {
	if lastActive.IsZero() || streak <= 0 {
		return 1
	}
	switch d := daysBetween(lastActive, today); {
	case d <= 0: // same day, or an out-of-order update dated earlier
		return streak
	case d == 1:
		return streak + 1
	default:
		return 1
	}
}

// EffectiveStreak is the streak as it should be SHOWN on day today: the
// stored counter only changes when the user is active, so for a user who
// has not been around since before yesterday the flame is already out (0).
func EffectiveStreak(streak int, lastActive, today time.Time) int {
	if streak <= 0 || lastActive.IsZero() {
		return 0
	}
	if d := daysBetween(lastActive, today); d < 0 || d > 1 {
		return 0
	}
	return streak
}

func daysBetween(from, to time.Time) int {
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

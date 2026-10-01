package models

import (
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextStreak(t *testing.T) {
	cases := []struct {
		streak int
		last   string
		today  string
		want   int
	}{
		{0, "", "2026-10-01", 1},           // first visit
		{1, "2026-10-01", "2026-10-01", 1}, // same day, many tests
		{5, "2026-10-01", "2026-10-01", 5},
		{5, "2026-09-30", "2026-10-01", 6}, // next day
		{5, "2026-09-29", "2026-10-01", 1}, // missed a day
		{5, "2026-12-31", "2027-01-01", 6}, // year boundary
		{5, "2028-02-28", "2028-02-29", 6}, // leap day
		{5, "2026-10-02", "2026-10-01", 5}, // out-of-order update never resets
	}
	for _, c := range cases {
		var last time.Time
		if c.last != "" {
			last = day(c.last)
		}
		if got := NextStreak(c.streak, last, day(c.today)); got != c.want {
			t.Errorf("NextStreak(%d, %s, %s) = %d, want %d", c.streak, c.last, c.today, got, c.want)
		}
	}
}

func TestEffectiveStreak(t *testing.T) {
	today := day("2026-10-01")
	if got := EffectiveStreak(7, day("2026-10-01"), today); got != 7 {
		t.Fatalf("active today: %d", got)
	}
	if got := EffectiveStreak(7, day("2026-09-30"), today); got != 7 {
		t.Fatalf("active yesterday must still burn: %d", got)
	}
	if got := EffectiveStreak(7, day("2026-09-29"), today); got != 0 {
		t.Fatalf("missed day must extinguish: %d", got)
	}
	if got := EffectiveStreak(3, time.Time{}, today); got != 0 {
		t.Fatalf("never active: %d", got)
	}
}

func TestStreakTodayKazakhstanMidnight(t *testing.T) {
	// 19:30 UTC on Sep 30 = 00:30 on Oct 1 in Kazakhstan (UTC+5).
	now := time.Date(2026, 9, 30, 19, 30, 0, 0, time.UTC)
	if got := StreakToday(now); !got.Equal(day("2026-10-01")) {
		t.Fatalf("StreakToday = %s, want 2026-10-01", got.Format("2006-01-02"))
	}
	// 18:59 UTC is still Sep 30 locally.
	now = time.Date(2026, 9, 30, 18, 59, 0, 0, time.UTC)
	if got := StreakToday(now); !got.Equal(day("2026-09-30")) {
		t.Fatalf("StreakToday = %s, want 2026-09-30", got.Format("2006-01-02"))
	}
}

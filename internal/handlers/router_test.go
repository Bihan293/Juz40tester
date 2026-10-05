package handlers

import (
	"reflect"
	"testing"
)

// TestIDRoutes (A7): every numeric-id prefix maps to its handler, the id is
// parsed, overlapping prefixes resolve as before, a malformed id is an error
// and non-id callbacks are left to the switch.
func TestIDRoutes(t *testing.T) {
	cases := []struct {
		data string
		fn   any
		ack  bool
	}{
		{cbWeakSubject + "7", (*Handler).openWeakSubject, false},
		{cbFinish + "7", (*Handler).finishPersonalTest, false},
		{cbSubject + "7", (*Handler).openSubject, true},
		{cbOpenTest + "7", (*Handler).openTest, false},
		{cbExitYes + "7", (*Handler).exitTest, false},
		{cbExitNo + "7", (*Handler).cancelExit, false},
		{cbExit + "7", (*Handler).confirmExit, true},
		{cbRetry + "7", (*Handler).retryTest, true},
		{cbProgSubject + "7", (*Handler).showSubjectProgress, true},
		{cbLbSubject + "7", (*Handler).showSubjectLeaderboardFor, true},
	}
	for _, c := range cases {
		r, id, err := matchIDRoute(c.data)
		if r == nil || err != nil || id != 7 {
			t.Fatalf("%s: route %v id %d err %v", c.data, r, id, err)
		}
		if reflect.ValueOf(r.fn).Pointer() != reflect.ValueOf(c.fn).Pointer() || r.ack != c.ack {
			t.Fatalf("%s: wrong handler or ack", c.data)
		}
		if r, _, err := matchIDRoute(c.data[:len(c.data)-1] + "x"); r == nil || err == nil {
			t.Fatalf("%s: malformed id must match the route with an error", c.data)
		}
	}
	for _, d := range []string{cbNoop, cbMainMenu, cbSubjects, cbSubjectPage + "1:2", cbPendingID + "1:2",
		cbWeakMenu, cbAnswer + "1:2:3", cbProgress, cbSettings, cbSetLang + "kk", cbLeaderboard, cbLbStreak} {
		if r, _, _ := matchIDRoute(d); r != nil {
			t.Fatalf("%s must not be an id route", d)
		}
	}
}

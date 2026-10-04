package models

import "testing"

func TestTopicLevel(t *testing.T) {
	cases := []struct {
		recent string
		want   int
	}{
		{"", TopicUnknown},
		{"00", TopicUnknown},                 // too few answers
		{"111", TopicOK},                     // all correct
		{"011", TopicOK},                     // a single slip is never weak
		{"1111111110", TopicOK},              // 90%
		{"001", TopicRed},                    // 33%, 2 mistakes
		{"0000", TopicRed},                   // systematic failure
		{"0011", TopicYellow},                // 50%
		{"1101101111", TopicOK},              // 80%, 2 mistakes → fine
		{"1101101101", TopicYellow},          // 70%
		{"0000011111", TopicYellow},          // fixing errors: 50%
		{"000001111111", TopicYellow},        // window = last 10 → "0001111111" = 70%
		{"0000000000" + "11111111", TopicOK}, // last 10: 0011111111 = 80%
	}
	for _, c := range cases {
		got := TopicStat{Recent: c.recent}.Level()
		if got != c.want {
			t.Errorf("Level(%q) = %d, want %d", c.recent, got, c.want)
		}
	}
}

func TestWeakTopicStatsOrder(t *testing.T) {
	stats := []TopicStat{
		{Key: "a", Recent: "0011"},       // 🟡 50%
		{Key: "b", Recent: "0001"},       // 🔴 25%
		{Key: "c", Recent: "111"},        // ok
		{Key: "d", Recent: "000"},        // 🔴 0%
		{Key: "e", Recent: "1101101101"}, // 🟡 70%
		{Key: "f", Recent: "0"},          // unknown
	}
	got := WeakTopicStats(stats, 0)
	want := []string{"d", "b", "a", "e"}
	if len(got) != len(want) {
		t.Fatalf("got %d weak topics, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Key != want[i] {
			t.Fatalf("order[%d] = %s, want %s", i, got[i].Key, want[i])
		}
	}
	if l := WeakTopicStats(stats, 2); len(l) != 2 || l[0].Key != "d" {
		t.Fatal("limit must keep the worst topics")
	}
}

func TestNormalizeTopic(t *testing.T) {
	if NormalizeTopic("  Клетка   и ТКАНИ ") != "клетка и ткани" {
		t.Fatal("normalisation must lower-case and collapse spaces")
	}
}

// R-10a: one AllTopicStats result grouped per subject gives the same lists
// as the old per-subject WeakTopicStats calls.
func TestWeakTopicStatsBySubject(t *testing.T) {
	stats := []TopicStat{
		{SubjectID: 1, Key: "a", Recent: "0000"},
		{SubjectID: 1, Key: "b", Recent: "1111111111"},
		{SubjectID: 2, Key: "c", Recent: "0101"},
		{SubjectID: 3, Key: "d", Recent: "1111111111"},
	}
	got := WeakTopicStatsBySubject(stats, 5)
	for _, sid := range []int64{1, 2} {
		var sub []TopicStat
		for _, s := range stats {
			if s.SubjectID == sid {
				sub = append(sub, s)
			}
		}
		want := WeakTopicStats(sub, 5)
		if len(got[sid]) != len(want) || len(want) == 0 || got[sid][0].Key != want[0].Key {
			t.Fatalf("subject %d: got %+v want %+v", sid, got[sid], want)
		}
	}
	if _, ok := got[3]; ok {
		t.Fatal("subject without weak topics must be absent")
	}
}

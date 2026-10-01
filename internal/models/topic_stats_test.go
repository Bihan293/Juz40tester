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

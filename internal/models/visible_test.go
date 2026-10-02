package models

import "testing"

// #36: the level shown to the user never exceeds the chain length.
func TestClampVisibleTests(t *testing.T) {
	cases := map[int]int{
		1:                    1,
		MaxVisibleTests - 1:  MaxVisibleTests - 1,
		MaxVisibleTests:      MaxVisibleTests,
		MaxVisibleTests + 1:  MaxVisibleTests, // «Открыто тестов: 201» bug
		MaxVisibleTests + 50: MaxVisibleTests,
	}
	for in, want := range cases {
		if got := ClampVisibleTests(in); got != want {
			t.Errorf("ClampVisibleTests(%d) = %d, want %d", in, got, want)
		}
	}
}

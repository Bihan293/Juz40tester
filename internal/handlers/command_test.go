package handlers

import "testing"

// #38: deep links (/start <payload>) and the /cmd@bot form must route to
// the same command; reply-keyboard texts and other commands are untouched.
func TestCommandKey(t *testing.T) {
	cases := map[string]string{
		"/start":                "/start",
		"/start ref_123":        "/start",
		"/start@Juz40Bot":       "/start",
		"/start@Juz40Bot ref_1": "/start",
		"/start\tpayload":       "/start",
		"/subjects":             "/subjects",
		"/weak@Juz40Bot":        "/weak",
		"/progress extra":       "/progress",
		"/started":              "/started", // a different command, not /start
		kbSubjects:              kbSubjects,
		kbSettings:              kbSettings,
		"привет /start":         "привет /start",
		"":                      "",
	}
	for in, want := range cases {
		if got := commandKey(in); got != want {
			t.Errorf("commandKey(%q) = %q, want %q", in, got, want)
		}
	}
}

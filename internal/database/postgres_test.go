package database

import "testing"

// TestDirectURL (audit #21): migrations must bypass the Neon pooler so the
// session-level advisory lock and unlock run on the same server connection.
func TestDirectURL(t *testing.T) {
	cases := map[string]string{
		"postgres://u:p@ep-cool-1234-pooler.eu-central-1.aws.neon.tech/db?sslmode=require": "postgres://u:p@ep-cool-1234.eu-central-1.aws.neon.tech/db?sslmode=require",
		"postgres://u:p@ep-cool-1234-pooler.eu.neon.tech:5432/db":                          "postgres://u:p@ep-cool-1234.eu.neon.tech:5432/db",
		"postgres://u:p@ep-cool-1234.eu.neon.tech/db":                                      "postgres://u:p@ep-cool-1234.eu.neon.tech/db",
		"postgres://postgres:pg@localhost:5433/juz?sslmode=disable":                        "postgres://postgres:pg@localhost:5433/juz?sslmode=disable",
	}
	for in, want := range cases {
		if got := DirectURL(in); got != want {
			t.Errorf("DirectURL(%q) = %q, want %q", in, got, want)
		}
	}
}

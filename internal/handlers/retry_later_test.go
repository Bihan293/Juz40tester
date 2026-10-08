package handlers

import (
	"strings"
	"testing"
	"time"
)

// A ⏳ tap during the retry pause of a failing chain test gets an honest
// alert (Telegram alerts are capped at 200 characters).
func TestRetryLaterText(t *testing.T) {
	for _, c := range []struct {
		wait time.Duration
		want string
	}{
		{30 * time.Second, "через пару минут"},
		{29 * time.Minute, "примерно через 29 мин"},
		{2*time.Hour + 40*time.Minute, "примерно через 3 ч"},
		{12 * time.Hour, "примерно через 12 ч"},
	} {
		got := retryLaterText(7, c.wait)
		if !strings.Contains(got, c.want) || !strings.Contains(got, "«Тест 7»") {
			t.Fatalf("wait %v: %q must mention %q", c.wait, got, c.want)
		}
		if n := len([]rune(got)); n > 200 {
			t.Fatalf("alert too long: %d runes", n)
		}
	}
}

package services

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

func TestParseBroadcastButtons(t *testing.T) {
	rows, err := ParseBroadcastButtons("Канал | https://t.me/juz40\n\n Сайт | https://example.com ;; Чат | tg://resolve?domain=x \n")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || len(rows[0]) != 1 || len(rows[1]) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[0][0] != (repositories.BroadcastButton{Text: "Канал", URL: "https://t.me/juz40"}) || rows[1][1].URL != "tg://resolve?domain=x" {
		t.Fatalf("rows: %+v", rows)
	}
	// «|» inside the text: the LAST one separates the URL.
	rows, err = ParseBroadcastButtons("A | B | https://x.kz")
	if err != nil || rows[0][0].Text != "A | B" {
		t.Fatalf("pipe in text: %+v %v", rows, err)
	}
	for _, bad := range []string{
		"",
		"Без ссылки",
		"Текст | ftp://x",
		"Текст | javascript:alert(1)",
		" | https://x.kz",
		"Текст | https://",
		"Текст | https://a b",
		"1 | https://a ;; 2 | https://b ;; 3 | https://c ;; 4 | https://d",
		strings.Repeat("x", 65) + " | https://a",
		strings.Repeat("a | https://a\n", 9),
	} {
		if _, err := ParseBroadcastButtons(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidateBroadcast(t *testing.T) {
	ok := []repositories.Broadcast{
		{Text: "Привет"},
		{Text: strings.Repeat("я", bot.MaxTextRunes)},
		{MediaType: "photo", MediaFileID: "f"},
		{MediaType: "video", MediaFileID: "f", Text: strings.Repeat("я", bot.MaxCaptionRunes)},
	}
	for i := range ok {
		if err := ValidateBroadcast(&ok[i]); err != nil {
			t.Errorf("%d: %v", i, err)
		}
	}
	bad := []repositories.Broadcast{
		{Text: "  "},
		{Text: strings.Repeat("я", bot.MaxTextRunes+1)},
		{MediaType: "photo"},
		{MediaType: "video", MediaFileID: "f", Text: strings.Repeat("я", bot.MaxCaptionRunes+1)},
		{MediaType: "audio", MediaFileID: "f"},
	}
	for i := range bad {
		if err := ValidateBroadcast(&bad[i]); err == nil {
			t.Errorf("%d accepted", i)
		}
	}
}

func TestBroadcastMessageKeyboard(t *testing.T) {
	m := BroadcastMessage(&repositories.Broadcast{Text: "x", MediaType: "photo", MediaFileID: "F",
		Buttons: [][]repositories.BroadcastButton{{{Text: "a", URL: "https://a"}, {Text: "b", URL: "https://b"}}}})
	if m.MediaType != "photo" || m.MediaFileID != "F" || m.Keyboard == nil || len(m.Keyboard.InlineKeyboard[0]) != 2 ||
		m.Keyboard.InlineKeyboard[0][1].URL != "https://b" {
		t.Fatalf("%+v", m)
	}
	if m := BroadcastMessage(&repositories.Broadcast{Text: "x"}); m.Keyboard != nil {
		t.Fatal("keyboard without buttons")
	}
}

func TestClassifyBroadcastError(t *testing.T) {
	cases := []struct {
		err     error
		attempt int
		status  string
		retry   bool
	}{
		{nil, 1, repositories.RecipientSent, false},
		{&bot.RateLimitError{RetryAfter: 7 * time.Second}, 9, repositories.RecipientPending, true},
		{&bot.APIError{Code: http.StatusForbidden, Description: "Forbidden: bot was blocked by the user"}, 1, repositories.RecipientBlocked, false},
		{&bot.APIError{Code: 400, Description: "Bad Request: user is deactivated"}, 1, repositories.RecipientBlocked, false},
		{&bot.APIError{Code: 400, Description: "Bad Request: chat not found"}, 1, repositories.RecipientFailed, false},
		{&bot.APIError{Code: 502, Description: "Bad Gateway"}, 1, repositories.RecipientPending, true},
		{&bot.APIError{Code: 502, Description: "Bad Gateway"}, 3, repositories.RecipientFailed, false},
		{errors.New("connection refused"), 1, repositories.RecipientPending, true},
		{errors.New("connection refused"), 3, repositories.RecipientFailed, false},
		{context.Canceled, 1, repositories.RecipientFailed, false},
	}
	for i, c := range cases {
		status, retry, _ := classifyBroadcastError(c.err, c.attempt, 3)
		if status != c.status || (retry > 0) != c.retry {
			t.Errorf("%d: %v → %s retry=%s", i, c.err, status, retry)
		}
	}
	if _, retry, _ := classifyBroadcastError(&bot.RateLimitError{RetryAfter: 7 * time.Second}, 1, 3); retry != 7*time.Second {
		t.Errorf("429 must wait retry_after, got %s", retry)
	}
}

func TestAdminBypassChargeID(t *testing.T) {
	a, b := AdminBypassChargePrefix+randomID(), AdminBypassChargePrefix+randomID()
	if a == b || !IsAdminBypassCharge(a) || IsAdminBypassCharge("stxABC") {
		t.Fatal(a, b)
	}
}

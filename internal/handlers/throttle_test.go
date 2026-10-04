package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/ratelimit"
)

// TestThrottledTapNoWork (R-9): a tap within the action interval is only
// acknowledged (one answerCallbackQuery) — no DB access at all (the handler
// has NO repositories here: any DB access would panic), no other Telegram
// call. Messages within the interval are ignored silently.
func TestThrottledTapNoWork(t *testing.T) {
	f := &kbFake{}
	srv := f.server(t)
	h := New(bot.NewClient("T").WithBaseURL(srv.URL), nil, nil).WithActionLimiter(ratelimit.New(time.Hour))
	// Consume the user's single allowed action.
	if !h.limiter.Allow(7) {
		t.Fatal("setup")
	}
	ctx := context.Background()
	cb := &bot.CallbackQuery{ID: "x", From: &bot.TgUser{ID: 7}, Data: "ans:1:1:0",
		Message: &bot.Message{MessageID: 1, Chat: bot.Chat{ID: 7, Type: "private"}}}
	for i := 0; i < 50; i++ {
		h.HandleUpdate(ctx, &bot.Update{CallbackQuery: cb})
		h.HandleUpdate(ctx, &bot.Update{Message: &bot.Message{From: &bot.TgUser{ID: 7}, Chat: bot.Chat{ID: 7, Type: "private"}, Text: "/start"}})
	}
	if f.calls != 50 {
		t.Fatalf("%d Telegram calls for 50 throttled taps + 50 messages, want 50 answerCallbackQuery", f.calls)
	}
	for _, s := range f.sent {
		if strings.TrimSpace(s) != "" {
			t.Fatalf("throttled actions must not send messages: %q", s)
		}
	}
}

// R-10b: callbacks from a group chat are ignored (no Telegram call, no DB:
// the handler has no repositories and would panic on any DB access).
func TestGroupCallbackIgnored(t *testing.T) {
	f := &kbFake{}
	srv := f.server(t)
	h := New(bot.NewClient("T").WithBaseURL(srv.URL), nil, nil)
	cb := &bot.CallbackQuery{ID: "g", From: &bot.TgUser{ID: 8}, Data: "menu",
		Message: &bot.Message{MessageID: 1, Chat: bot.Chat{ID: -100, Type: "supergroup"}}}
	h.HandleUpdate(context.Background(), &bot.Update{CallbackQuery: cb})
	if f.calls != 0 {
		t.Fatalf("%d Telegram calls for a group callback, want 0", f.calls)
	}
}

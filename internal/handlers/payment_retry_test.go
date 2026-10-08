package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

func TestRetryWithBackoff(t *testing.T) {
	ctx := context.Background()
	delays := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	first := errors.New("db down")

	calls := 0
	err := retryWithBackoff(ctx, delays, first, func() error {
		calls++
		if calls < 2 {
			return first
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("recovered on the 2nd retry: err=%v calls=%d", err, calls)
	}

	calls = 0
	err = retryWithBackoff(ctx, delays, first, func() error { calls++; return first })
	if !errors.Is(err, first) || calls != len(delays) {
		t.Fatalf("all retries fail: err=%v calls=%d", err, calls)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	calls = 0
	err = retryWithBackoff(cctx, []time.Duration{time.Hour}, first, func() error { calls++; return nil })
	if !errors.Is(err, first) || calls != 0 {
		t.Fatalf("cancelled context: err=%v calls=%d", err, calls)
	}
}

func TestIsPaymentUpdate(t *testing.T) {
	from := &bot.TgUser{ID: 1}
	cases := []struct {
		upd  *bot.Update
		want bool
	}{
		{&bot.Update{Message: &bot.Message{From: from, SuccessfulPayment: &bot.SuccessfulPayment{}}}, true},
		{&bot.Update{Message: &bot.Message{From: from, RefundedPayment: &bot.RefundedPayment{}}}, true},
		{&bot.Update{Message: &bot.Message{From: from, Text: "/start"}}, false},
		{&bot.Update{CallbackQuery: &bot.CallbackQuery{From: from}}, false},
	}
	for i, c := range cases {
		if got := isPaymentUpdate(c.upd); got != c.want {
			t.Fatalf("case %d: %v", i, got)
		}
	}
}

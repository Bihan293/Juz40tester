package bot

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateUserKey(t *testing.T) {
	cases := []struct {
		u    *Update
		want int64
	}{
		{nil, 0},
		{&Update{}, 0},
		{&Update{Message: &Message{From: &TgUser{ID: 5}, Chat: Chat{ID: 9}}}, 5},
		{&Update{Message: &Message{Chat: Chat{ID: 9}}}, 0},
		{&Update{CallbackQuery: &CallbackQuery{From: &TgUser{ID: 7}}}, 7},
		{&Update{PreCheckoutQuery: &PreCheckoutQuery{From: &TgUser{ID: 8}}}, 8},
		{&Update{PreCheckoutQuery: &PreCheckoutQuery{}}, 0},
	}
	for i, c := range cases {
		if got := c.u.UserKey(); got != c.want {
			t.Errorf("case %d: %d, want %d", i, got, c.want)
		}
	}
}

type fakeShared struct {
	calls  atomic.Int32
	err    error
	paused atomic.Int64
}

func (f *fakeShared) Wait(context.Context) error { f.calls.Add(1); return f.err }
func (f *fakeShared) Pause(d time.Duration)      { f.paused.Store(int64(d)) }

// The shared (cluster) limiter is used for limited calls only; when it
// fails the local limiter takes over and the call still goes out.
func TestSharedLimiterAndFallback(t *testing.T) {
	srv, calls := okTelegram(t)
	sh := &fakeShared{}
	c := NewClient("T").WithBaseURL(srv.URL).WithMaxRPS(1000).WithSharedLimiter(sh)
	if _, err := c.SendMessage(context.Background(), 1, "x", nil); err != nil {
		t.Fatal(err)
	}
	if sh.calls.Load() != 1 {
		t.Fatalf("shared limiter consulted %d times", sh.calls.Load())
	}
	_ = c.AnswerCallbackQuery(context.Background(), "id", "")
	if sh.calls.Load() != 1 {
		t.Fatal("answerCallbackQuery must bypass the limiter")
	}
	sh.err = errors.New("redis down")
	if _, err := c.SendMessage(context.Background(), 1, "y", nil); err != nil {
		t.Fatalf("fallback to the local limiter failed: %v", err)
	}
	if atomic.LoadInt32(calls) < 2 {
		t.Fatal("messages not sent")
	}
}

// SetMaxRPS (Postgres share of TG_MAX_RPS) changes the local rate.
func TestSetMaxRPS(t *testing.T) {
	c := NewClient("T").WithMaxRPS(25)
	c.SetMaxRPS(5)
	if c.limiter.interval != 200*time.Millisecond {
		t.Fatalf("interval %s, want 200ms", c.limiter.interval)
	}
	c.SetMaxRPS(0)
	if c.limiter.interval != time.Second {
		t.Fatalf("interval %s, want 1s (at least 1 rps)", c.limiter.interval)
	}
}

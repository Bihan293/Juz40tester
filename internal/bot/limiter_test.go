package bot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func okTelegram(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":1,"type":"private"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestLimiter100CallsTakeExpectedTime (A8): 100 sendMessage calls at
// 50 rps, burst 5, take ~(100-5)/50 = 1.9 s, never much faster.
func TestLimiter100CallsTakeExpectedTime(t *testing.T) {
	srv, calls := okTelegram(t)
	c := NewClient("T").WithBaseURL(srv.URL).WithMaxRPS(50)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.SendMessage(context.Background(), int64(i+1), "x", nil); err != nil {
				t.Errorf("send: %v", err)
			}
		}(i)
	}
	wg.Wait()
	el := time.Since(start)
	if atomic.LoadInt32(calls) != 100 {
		t.Fatalf("calls = %d", *calls)
	}
	if el < 1700*time.Millisecond || el > 4*time.Second {
		t.Fatalf("100 calls took %s, want ~1.9s", el)
	}
}

// TestLimiterAnswerCallbackNotLimited: answerCallbackQuery bypasses a
// saturated limiter.
func TestLimiterAnswerCallbackNotLimited(t *testing.T) {
	srv, _ := okTelegram(t)
	c := NewClient("T").WithBaseURL(srv.URL).WithMaxRPS(1)
	c.limiter.pause(10 * time.Second) // limited calls would wait 10 s
	start := time.Now()
	if err := c.AnswerCallbackQuery(context.Background(), "id", ""); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("answerCallbackQuery waited %s", el)
	}
}

// TestLimiterContextCancelStopsWait: a cancelled context ends the wait with
// an error and the request is not sent.
func TestLimiterContextCancelStopsWait(t *testing.T) {
	srv, calls := okTelegram(t)
	c := NewClient("T").WithBaseURL(srv.URL).WithMaxRPS(1)
	c.limiter.pause(10 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.SendMessage(ctx, 1, "x", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	if time.Since(start) > 2*time.Second || atomic.LoadInt32(calls) != 0 {
		t.Fatalf("wait not interrupted: %s, calls=%d", time.Since(start), *calls)
	}
	if strings.Contains(err.Error(), "T/") {
		t.Fatalf("error leaks URL: %v", err)
	}
}

// TestLimiter429PerChatDoesNotPauseOthers: a lone 429 is per-chat flood
// control — the chat itself waits retry_after, the other chats do not.
func TestLimiter429PerChatDoesNotPauseOthers(t *testing.T) {
	c, _ := fakeTelegram(t, 1, 1)
	start := time.Now()
	if _, err := c.SendMessage(context.Background(), 1, "a", nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("the flooded chat must wait retry_after before its retry")
	}
	t2 := time.Now()
	if _, err := c.SendMessage(context.Background(), 2, "b", nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t2); d > 500*time.Millisecond {
		t.Fatalf("another chat waited %s after a per-chat 429", d)
	}
}

// TestLimiter429ManyChatsPausesAll: 429s for several different chats at
// once are the bot-wide limit — everybody waits.
func TestLimiter429ManyChatsPausesAll(t *testing.T) {
	c, _ := fakeTelegram(t, globalFloodChats, 1)
	c.limiter = newRateLimiter(1000, 1000)
	var wg sync.WaitGroup
	defer wg.Wait()
	for chat := int64(1); chat <= globalFloodChats; chat++ {
		wg.Add(1)
		go func(chat int64) { defer wg.Done(); _, _ = c.SendMessage(context.Background(), chat, "x", nil) }(chat)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.limiter.reserve() <= 0 {
		c.limiter.cancel()
		if time.Now().After(deadline) {
			t.Fatal("429s of several chats did not pause the limiter")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFloodDetector(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := newFloodDetector()
	d.now = func() time.Time { return now }
	if !d.global(0) {
		t.Fatal("a 429 without a chat is bot-wide")
	}
	for i := 0; i < 10; i++ {
		if d.global(42) {
			t.Fatal("repeated 429s of ONE chat are per-chat flood control")
		}
	}
	if d.global(43) {
		t.Fatal("two chats are not enough")
	}
	if !d.global(44) {
		t.Fatalf("%d chats within %s must pause everybody", globalFloodChats, globalFloodWindow)
	}
	now = now.Add(globalFloodWindow + time.Millisecond)
	if d.global(45) {
		t.Fatal("old 429s must expire")
	}
	if len(d.recent) > globalFloodChats*4 {
		t.Fatalf("detector grows: %d", len(d.recent))
	}
}

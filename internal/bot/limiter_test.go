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

// TestLimiter429PausesAll: a 429 pauses the limiter for other chats too.
func TestLimiter429PausesAll(t *testing.T) {
	c, _ := fakeTelegram(t, 1, 1)
	start := time.Now()
	if _, err := c.SendMessage(context.Background(), 1, "a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SendMessage(context.Background(), 2, "b", nil); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("429 did not pause the limiter")
	}
}

package bot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakeTelegram(t *testing.T, fail429 int32, retryAfter int) (*Client, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if n <= fail429 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":` +
				itoa(retryAfter) + `}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"chat":{"id":1,"type":"private"}}}`))
	}))
	t.Cleanup(srv.Close)
	return NewClient("TOKEN").WithBaseURL(srv.URL), &calls
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// TestCall429RetriedAfterRetryAfter (audit #30): a 429 is retried after
// parameters.retry_after and the message is delivered.
func TestCall429RetriedAfterRetryAfter(t *testing.T) {
	c, calls := fakeTelegram(t, 1, 1)
	start := time.Now()
	id, err := c.SendMessage(context.Background(), 1, "hi", nil)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != 77 || atomic.LoadInt32(calls) != 2 {
		t.Fatalf("id=%d calls=%d, want 77 / 2", id, *calls)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("retry did not wait retry_after (%s)", time.Since(start))
	}
}

// TestCall429BoundedRetries: retries are bounded (1 + maxRateLimitRetries
// requests at most), never infinite.
func TestCall429BoundedRetries(t *testing.T) {
	c, calls := fakeTelegram(t, 100, 1)
	_, err := c.SendMessage(context.Background(), 1, "hi", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("want RateLimitError, got %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1+maxRateLimitRetries {
		t.Fatalf("calls = %d, want %d", got, 1+maxRateLimitRetries)
	}
}

// TestCall429TooLongWaitFailsFast: a flood ban longer than maxRetryAfter is
// not waited out.
func TestCall429TooLongWaitFailsFast(t *testing.T) {
	c, calls := fakeTelegram(t, 100, 3600)
	start := time.Now()
	_, err := c.SendMessage(context.Background(), 1, "hi", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != time.Hour {
		t.Fatalf("want RateLimitError(1h), got %v", err)
	}
	if atomic.LoadInt32(calls) != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("must fail fast: calls=%d", *calls)
	}
}

// TestCallNon429ErrorNotRetried: other API errors are returned at once.
func TestCallNon429ErrorNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()
	c := NewClient("T").WithBaseURL(srv.URL)
	if _, err := c.SendMessage(context.Background(), 1, "x", nil); err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

// TestTransportErrorDoesNotLeakToken (P0-1): a network failure returns a
// *url.Error whose text embeds https://.../bot<TOKEN>/method. The error
// returned to callers (and logged) must not contain the URL or the token.
func TestTransportErrorDoesNotLeakToken(t *testing.T) {
	const token = "123456:SECRET-bot-token"
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // connection refused from now on

	c := NewClient(token).WithBaseURL(base)
	_, err := c.SendMessage(context.Background(), 1, "hi", nil)
	if err == nil {
		t.Fatal("expected a transport error")
	}
	msg := err.Error()
	if strings.Contains(msg, token) || strings.Contains(msg, "/bot") || strings.Contains(msg, base) {
		t.Fatalf("error leaks URL/token: %q", msg)
	}
	if !strings.Contains(msg, "telegram sendMessage") {
		t.Fatalf("error lost the method name: %q", msg)
	}

	// Context cancellation must stay detectable through the wrapper.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.SendMessage(ctx, 1, "hi", nil)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("cancelled call: err=%v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(context.Canceled) lost: %v", err)
	}
}

// TestCall429LongRetryAfterDoesNotBlockHandler (R-3): a 429 with a
// retry_after longer than maxInlineRetryAfter is NOT slept in the calling
// handler: sendMessage returns at once with a deferred RateLimitError and
// the message is delivered by the background queue after retry_after.
func TestCall429LongRetryAfterDoesNotBlockHandler(t *testing.T) {
	c, calls := fakeTelegram(t, 1, 2) // first call: 429 retry_after=2s
	start := time.Now()
	_, err := c.SendMessage(context.Background(), 1, "hi", nil)
	if !IsDeferred(err) {
		t.Fatalf("want deferred RateLimitError, got %v", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("handler blocked for %s on a 429", el)
	}
	if c.PendingDeferred() != 1 {
		t.Fatalf("pending = %d, want 1", c.PendingDeferred())
	}
	// While the chat is banned, the next message to it is queued without an
	// HTTP call (keeps order, does not provoke another 429).
	if _, err := c.SendMessage(context.Background(), 1, "second", nil); !IsDeferred(err) {
		t.Fatalf("second send during ban: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("calls during ban = %d, want 1", got)
	}
	// retry_after is honoured: nothing is re-sent before ~2s.
	time.Sleep(1500 * time.Millisecond)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("re-sent before retry_after: %d calls", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 || c.PendingDeferred() != 0 {
		t.Fatalf("after flush calls=%d pending=%d, want 3 / 0", got, c.PendingDeferred())
	}
	if time.Since(start) < 1900*time.Millisecond {
		t.Fatal("deferred send did not wait retry_after")
	}
}

// TestCall429AnswerCallbackFailsFast: non-deferrable methods never sleep a
// long retry_after in the handler — they fail fast.
func TestCall429AnswerCallbackFailsFast(t *testing.T) {
	c, _ := fakeTelegram(t, 100, 5)
	start := time.Now()
	err := c.AnswerCallbackQuery(context.Background(), "id", "")
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.Deferred {
		t.Fatalf("want non-deferred RateLimitError, got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("answerCallbackQuery slept on a long retry_after")
	}
}

// TestDeleteMessagesBatch: several deletions are ONE request.
func TestDeleteMessagesBatch(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer srv.Close()
	c := NewClient("T").WithBaseURL(srv.URL)
	if err := c.DeleteMessages(context.Background(), 1, []int64{5, 6, 7}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "deleteMessages" {
		t.Fatalf("requests: %v, want one deleteMessages", paths)
	}
}

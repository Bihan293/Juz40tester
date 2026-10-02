package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// kbFake is a fake Bot API that numbers sent messages and records deletes.
type kbFake struct {
	mu      sync.Mutex
	next    int64
	sent    []string // texts of sendMessage, in order
	deleted []int64
}

func (f *kbFake) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			f.next++
			f.sent = append(f.sent, p["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":`+strconv.FormatInt(f.next, 10)+`,"chat":{"id":1,"type":"private"}}}`)
		case strings.HasSuffix(r.URL.Path, "/deleteMessage"):
			f.deleted = append(f.deleted, int64(p["message_id"].(float64)))
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// #35: finishing many tests must leave at most ONE «menu is back» note in
// the chat, and hiding the menu for the next test removes it.
func TestReplyKeyboardNoteNoSpam(t *testing.T) {
	f := &kbFake{}
	h := New(bot.NewClient("T").WithBaseURL(f.server(t).URL), nil, nil)
	ctx := context.Background()
	const chat = 42

	h.restoreReplyKeyboard(ctx, chat) // msg 1 — first test finished
	if len(f.deleted) != 0 {
		t.Fatalf("first note must stay (it carries the keyboard), deleted %v", f.deleted)
	}
	h.restoreReplyKeyboard(ctx, chat) // msg 2 — previous note 1 removed
	h.restoreReplyKeyboard(ctx, chat) // msg 3 — previous note 2 removed
	if want := []int64{1, 2}; !equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v (only the latest note may remain)", f.deleted, want)
	}

	// Next test starts: msg 4 = removal vehicle (deleted), note 3 deleted.
	h.hideReplyKeyboard(ctx, chat)
	if want := []int64{1, 2, 4, 3}; !equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v", f.deleted, want)
	}
	// Hiding again with no note is a no-op beyond its own vehicle message.
	h.hideReplyKeyboard(ctx, chat)
	if want := []int64{1, 2, 4, 3, 5}; !equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v", f.deleted, want)
	}
	// Notes are tracked per chat.
	h.restoreReplyKeyboard(ctx, chat)   // 6
	h.restoreReplyKeyboard(ctx, chat+1) // 7 — other chat, nothing deleted
	if want := []int64{1, 2, 4, 3, 5}; !equal(f.deleted, want) {
		t.Fatalf("other chat must not touch this one: %v", f.deleted)
	}
	for _, s := range f.sent {
		if strings.Contains(s, "Главное меню снова доступно") {
			t.Fatalf("old long note text still sent: %q", s)
		}
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

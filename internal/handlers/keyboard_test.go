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
	"github.com/Bihan293/Juz40tester/internal/models"
)

// kbFake is a fake Bot API that numbers sent messages and records deletes.
type kbFake struct {
	mu      sync.Mutex
	next    int64
	sent    []string // texts of sendMessage, in order
	deleted []int64
	calls   int // every Bot API request
}

func (f *kbFake) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			f.next++
			f.sent = append(f.sent, p["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":`+strconv.FormatInt(f.next, 10)+`,"chat":{"id":1,"type":"private"}}}`)
		case strings.HasSuffix(r.URL.Path, "/deleteMessages"):
			for _, id := range p["message_ids"].([]any) {
				f.deleted = append(f.deleted, int64(id.(float64)))
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
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

	// Next test starts: msg 4 = removal vehicle; it and note 3 are deleted
	// with ONE deleteMessages request (R-2: was two deleteMessage calls).
	before := f.calls
	h.hideReplyKeyboard(ctx, chat)
	if want := []int64{1, 2, 4, 3}; !equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v", f.deleted, want)
	}
	if got := f.calls - before; got != 2 {
		t.Fatalf("hide = %d Telegram calls, want 2 (send + deleteMessages)", got)
	}
	// Hiding again while the menu is already hidden: ZERO Telegram calls.
	before = f.calls
	h.hideReplyKeyboard(ctx, chat)
	if f.calls != before {
		t.Fatalf("repeated hide made %d Telegram calls, want 0", f.calls-before)
	}
	// Restore after a hide: the old note is already gone — ONE call.
	before = f.calls
	h.restoreReplyKeyboard(ctx, chat) // 5
	if f.calls-before != 1 {
		t.Fatalf("restore after hide = %d calls, want 1", f.calls-before)
	}
	// Notes are tracked per chat.
	h.restoreReplyKeyboard(ctx, chat+1) // 6 — other chat, nothing deleted
	if want := []int64{1, 2, 4, 3}; !equal(f.deleted, want) {
		t.Fatalf("other chat must not touch this one: %v", f.deleted)
	}
	// After the menu was shown again, the next test hides it again.
	before = f.calls
	h.hideReplyKeyboard(ctx, chat) // 7, deletes 7 and 5
	if want := []int64{1, 2, 4, 3, 7, 5}; !equal(f.deleted, want) || f.calls-before != 2 {
		t.Fatalf("hide after restore: deleted %v calls %d", f.deleted, f.calls-before)
	}
	// The main menu (with the reply keyboard) also makes the menu visible.
	h.sendMainMenu(ctx, chat, &models.User{FirstName: "A"}, false) // 8
	before = f.calls
	h.hideReplyKeyboard(ctx, chat) // 9: send + deleteMessage of the vehicle
	if f.calls-before != 2 {
		t.Fatalf("hide after main menu must hide again: %d calls, want 2", f.calls-before)
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

// Cluster mode: another worker may have restored the menu, so this
// process's «already hidden» memory must not skip the hide.
func TestHideReplyKeyboardSharedUpdates(t *testing.T) {
	f := &kbFake{}
	h := New(bot.NewClient("T").WithBaseURL(f.server(t).URL), nil, nil).WithSharedUpdates(true)
	ctx := context.Background()
	h.hideReplyKeyboard(ctx, 7)
	// (meanwhile another worker shows the menu for chat 7)
	before := f.calls
	h.hideReplyKeyboard(ctx, 7)
	if f.calls-before != 2 {
		t.Fatalf("hide in cluster mode = %d Telegram calls, want 2 (never skipped)", f.calls-before)
	}
}

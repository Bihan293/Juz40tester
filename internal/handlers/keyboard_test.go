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
	"github.com/Bihan293/Juz40tester/internal/services"
)

// kbFake is a fake Bot API that numbers sent messages and records every
// request body (to check which keyboards were sent).
type kbFake struct {
	mu      sync.Mutex
	next    int64
	sent    []string // texts of sendMessage, in order
	edited  []string // texts of editMessageText, in order
	bodies  []string // raw request bodies of every call
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
		f.bodies = append(f.bodies, string(body))
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			f.next++
			f.sent = append(f.sent, p["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":`+strconv.FormatInt(f.next, 10)+`,"chat":{"id":1,"type":"private"}}}`)
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			f.edited = append(f.edited, p["text"].(string))
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
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

// noPopupKeyboard fails when a request could make the phone keyboard (or
// the reply-menu panel) pop up during a test: ReplyKeyboardRemove,
// ForceReply, an input placeholder or a reply keyboard.
func noPopupKeyboard(t *testing.T, bodies []string) {
	t.Helper()
	for _, b := range bodies {
		for _, bad := range []string{"remove_keyboard", "force_reply", "input_field_placeholder", `"keyboard":`} {
			if strings.Contains(b, bad) {
				t.Fatalf("request contains %q (keyboard pop-up): %s", bad, b)
			}
		}
	}
}

// The result screen is ONE edit of the answered question — no extra
// message, no reply-keyboard change (it used to send «🏠 Меню снова
// доступно» with the reply keyboard, which popped the menu panel up).
func TestResultScreenNoKeyboardPopup(t *testing.T) {
	for _, kind := range []string{models.TestKindChain, models.TestKindPersonal} {
		t.Run(kind, func(t *testing.T) {
			f := &kbFake{}
			h := New(bot.NewClient("T").WithBaseURL(f.server(t).URL), nil, nil)
			sum := &services.AttemptSummary{
				Attempt:      &models.TestAttempt{CorrectCount: 1},
				Test:         &models.Test{ID: 5, SubjectID: 3, TestNumber: 1, Kind: kind},
				Total:        2,
				StatusCounts: map[int]int{models.StatusNone: 1, models.StatusMastered: 1},
			}
			cb := &bot.CallbackQuery{ID: "x", Message: &bot.Message{MessageID: 77, Chat: bot.Chat{ID: 42}}}
			h.renderSummaryInto(context.Background(), cb, "🟢 Правильно", sum, &models.User{ID: 1}, 7, 42, "", services.CompletionOutcome{})
			if f.calls != 1 || len(f.edited) != 1 || len(f.sent) != 0 {
				t.Fatalf("result = %d calls (%d edits, %d sends), want exactly 1 edit", f.calls, len(f.edited), len(f.sent))
			}
			noPopupKeyboard(t, f.bodies)
		})
	}
}

// Without the list (no quiz service) the screen still offers a way back
// that matches the test kind.
func TestAfterTestScreenFallback(t *testing.T) {
	h := New(bot.NewClient("T"), nil, nil)
	_, kb := h.afterTestScreen(context.Background(), &models.User{ID: 1}, &models.Test{Kind: models.TestKindChain}, "note", nil)
	if s := markupData(kb); !strings.Contains(s, cbSubjects) || !strings.Contains(s, cbMainMenu) {
		t.Fatalf("chain fallback: %s", s)
	}
	_, kb = h.afterTestScreen(context.Background(), &models.User{ID: 1}, &models.Test{Kind: models.TestKindPersonal}, "note",
		[][]bot.InlineKeyboardButton{bot.Row(bot.Btn("🏁", cbFinish+"5"))})
	if s := markupData(kb); !strings.HasPrefix(s, cbFinish+"5|") || !strings.Contains(s, cbWeakMenu) {
		t.Fatalf("personal fallback: %s", s)
	}
}

func markupData(kb *bot.InlineKeyboardMarkup) string {
	var parts []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			parts = append(parts, b.CallbackData)
		}
	}
	return strings.Join(parts, "|")
}

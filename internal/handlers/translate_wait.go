package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// --- «Please wait» notices ---------------------------------------------------

// translationNote is shown while the first-ever Kazakh translation of a
// test is produced in the background (R-4).
const translationNote = "⏳ Перевод готовится… 🇰🇿\n\nТест переводится на казахский язык. Никуда не уходи — перевод делается один раз, дальше этот тест будет открываться мгновенно. Первый вопрос появится сам 👇"

// translationFailedNote: the translation failed or took too long — the
// test opens in the Russian master version instead.
const translationFailedNote = "😔 Не получилось перевести тест на казахский — пока открываю его на русском. Попробуй позже ещё раз."

// translationContinueTimeout bounds the continuation (start the attempt and
// show the question) that runs after the translation wait.
const translationContinueTimeout = 30 * time.Second

// deferUntilTranslated decides whether the test can be opened right now.
// It returns false when nothing has to be waited for (Russian user,
// language subject, complete cached translation, or a failed request — then
// the test opens in Russian at once and the caller continues synchronously).
// It returns true when a background translation was queued (R-4): the user
// gets «⏳ Перевод готовится…», the handler returns at once, and `then`
// runs in a background goroutine once the shared per-test waiter reports
// the outcome. That goroutine only blocks on a channel — the DB is checked
// by ONE shared waiter per test, never per user. Repeated taps of the same
// user on the same test never stack waiters or notes.
func (h *Handler) deferUntilTranslated(ctx context.Context, cb *bot.CallbackQuery, user *models.User, test *models.Test, then func(context.Context)) bool {
	_, wait, err := h.quiz.PrepareTranslation(ctx, user, test)
	if err != nil {
		if !errors.Is(err, services.ErrTranslationUnavailable) {
			logf("prepare translation of test %d: %v", test.ID, err)
		}
		h.sendText(ctx, cb.Message.Chat.ID, translationFailedNote)
		return false
	}
	if wait == nil {
		return false
	}
	key := fmt.Sprintf("tr:%d:%d", user.ID, test.ID)
	if _, busy := h.watchers.LoadOrStore(key, struct{}{}); busy {
		// The waiter of the first tap will show the question — no second note.
		return true
	}
	chatID := cb.Message.Chat.ID
	noteID, serr := h.tg.SendMessage(ctx, chatID, translationNote, nil)
	if serr != nil {
		logf("send translation note: %v", serr)
	}
	// The continuation runs after this update finished: mark the callback
	// as answered for its duration, so any error notice goes to the chat
	// (the callback query itself was already answered above).
	cont := func(ctx context.Context) {
		h.answered.Store(cb.ID, struct{}{})
		defer h.answered.Delete(cb.ID)
		then(ctx)
	}
	go h.awaitTranslation(key, chatID, noteID, test.ID, wait, cont)
	return true
}

// awaitTranslation waits for the outcome of a background translation and
// then removes the note (or turns it into an honest failure notice) and
// runs the continuation — the question is shown in Kazakh when the
// translation is ready, in Russian otherwise.
func (h *Handler) awaitTranslation(key string, chatID, noteID, testID int64, wait <-chan services.TranslationOutcome, then func(context.Context)) {
	defer h.watchers.Delete(key)
	defer func() {
		if r := recover(); r != nil {
			logf("translation waiter %s panicked: %v", key, r)
		}
	}()
	out := <-wait // always delivered (at the latest after the wait timeout)
	ctx, cancel := context.WithTimeout(context.Background(), translationContinueTimeout)
	defer cancel()
	if out.Ready {
		if noteID != 0 {
			if err := h.tg.DeleteMessage(ctx, chatID, noteID); err != nil {
				logf("delete translation note: %v", err)
			}
		}
	} else {
		logf("translation of test %d not ready (timeout=%v err=%q) — opening the Russian master", testID, out.TimedOut, out.Err)
		if noteID != 0 {
			h.editNote(ctx, chatID, noteID, translationFailedNote, nil)
		} else {
			h.sendText(ctx, chatID, translationFailedNote)
		}
	}
	then(ctx)
}

// startGenerationWatch acknowledges the tap, posts a clearly visible chat
// message «⏳ тест генерируется, никуда не уходи…» and subscribes it to the
// shared generation watcher of the key (R-7: one poller per key for all
// users). The note is edited into «✅ Тест готов» with a start button once
// the test exists — or into an honest error / «🔄 Обновить» on failure or
// timeout. Repeated taps of the same chat never spam it.
func (h *Handler) startGenerationWatch(ctx context.Context, cb *bot.CallbackQuery, key, note string,
	check genCheck, retryData string) {
	chatID := cb.Message.Chat.ID
	if h.gen.isSubscribed(key, chatID) {
		h.answerAlert(ctx, cb, "⏳ Тест ещё генерируется. Никуда не уходи — как только он будет готов, я пришлю в чат кнопку, чтобы его начать.")
		return
	}
	h.answerCallback(ctx, cb, "")
	msgID, err := h.tg.SendMessage(ctx, chatID, note, nil)
	if err != nil {
		logf("send generation note: %v", err)
		return
	}
	h.subscribeGeneration(key, chatID, msgID, check, retryData)
}

// editNote edits a bot-sent note; when editing fails (e.g. the user deleted
// the message) the text is sent as a new message instead.
func (h *Handler) editNote(ctx context.Context, chatID, msgID int64, text string, kb *bot.InlineKeyboardMarkup) {
	if err := h.tg.EditMessageText(ctx, chatID, msgID, text, kb); err != nil {
		if bot.IsNotModified(err) || bot.IsDeferred(err) {
			return
		}
		if _, err2 := h.tg.SendMessage(ctx, chatID, text, kb); err2 != nil {
			logf("edit/send note: %v / %v", err, err2)
		}
	}
}

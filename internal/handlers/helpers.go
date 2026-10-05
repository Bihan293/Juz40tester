package handlers

import (
	"context"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// --- Helpers -------------------------------------------------------------------

func (h *Handler) editMessage(ctx context.Context, cb *bot.CallbackQuery, text string, kb *bot.InlineKeyboardMarkup) {
	if cb.Message == nil {
		return
	}
	if err := h.tg.EditMessageText(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, kb); err != nil {
		// "message is not modified" means the content on screen is ALREADY
		// exactly this — sending a fresh copy would duplicate messages in the
		// chat (it happened on repeated taps). Everything else (message too
		// old to edit, deleted message) falls back to a new message.
		if bot.IsNotModified(err) {
			return
		}
		// Rate limited: the edit is queued and will be applied later — a
		// fallback send now would duplicate the message (R-3).
		if bot.IsDeferred(err) {
			return
		}
		if _, err2 := h.tg.SendMessage(ctx, cb.Message.Chat.ID, text, kb); err2 != nil {
			logf("edit/send message: %v / %v", err, err2)
		}
	}
}

// answerAlert answers the callback with a modal alert (dialog with an OK
// button) — for notices that must actually be read, unlike the tiny toast.
func (h *Handler) answerAlert(ctx context.Context, cb *bot.CallbackQuery, text string) {
	if _, dup := h.answered.LoadOrStore(cb.ID, struct{}{}); dup {
		h.fallbackText(ctx, cb, text)
		return
	}
	if err := h.tg.AnswerCallbackAlert(ctx, cb.ID, text); err != nil {
		logf("answer callback alert: %v", err)
	}
}

// sendText sends a plain chat message — used for errors after the callback
// has already been answered (Telegram accepts exactly one answer).
func (h *Handler) sendText(ctx context.Context, chatID int64, text string) {
	if _, err := h.tg.SendMessage(ctx, chatID, text, nil); err != nil {
		logf("send text: %v", err)
	}
}

func (h *Handler) answerCallback(ctx context.Context, cb *bot.CallbackQuery, text string) {
	if _, dup := h.answered.LoadOrStore(cb.ID, struct{}{}); dup {
		// The router (or an earlier step) already consumed the single
		// allowed answer — a second answerCallbackQuery would be rejected
		// and the user would never see the text. Deliver it to the chat.
		h.fallbackText(ctx, cb, text)
		return
	}
	if err := h.tg.AnswerCallbackQuery(ctx, cb.ID, text); err != nil {
		logf("answer callback: %v", err)
	}
}

// fallbackText delivers a callback notice as a chat message once the
// callback has already been answered. Empty texts are dropped.
func (h *Handler) fallbackText(ctx context.Context, cb *bot.CallbackQuery, text string) {
	if text == "" || cb.Message == nil {
		return
	}
	h.sendText(ctx, cb.Message.Chat.ID, text)
}

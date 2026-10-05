// Package handlers routes Telegram updates to business logic and renders UI.
package handlers

import (
	"context"
	"sync"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
	"github.com/Bihan293/Juz40tester/internal/ratelimit"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

// Handler wires the Telegram client to the services.
type Handler struct {
	tg    *bot.Client
	users *repositories.UserRepository
	quiz  *services.QuizService
	// watchers tracks the active translation waiters (key -> struct{}), so
	// repeated taps never spawn duplicate waiters or flood the chat.
	watchers sync.Map
	// answered tracks callback_query IDs that have already been answered
	// during the current update. Telegram accepts exactly ONE answer per
	// callback; any later answer with a text is delivered as a chat message
	// instead, so error texts are never silently lost.
	answered sync.Map
	// kbNotes remembers, per chat, the message that currently carries the
	// bottom main-menu reply keyboard (chatID -> messageID). Telegram ties a
	// reply keyboard to the message that sent it, so that note must stay in
	// the chat while the menu is visible — but only the LATEST one: the
	// previous note is deleted whenever a new one is sent or the menu is
	// hidden for the next test, so at most one note exists per chat
	// (before, every finished test left another «🏠 Главное меню…» line).
	kbNotes sync.Map
	// kbHidden remembers chats where THIS process hid the reply keyboard
	// (chatID -> true) and nothing has shown it since — a repeated hide is
	// then skipped without any Telegram call (R-2).
	kbHidden sync.Map
	// limiter throttles actions per Telegram user (R-9): at most one every
	// cfg.UserActionInterval. In memory, per instance (see ratelimit).
	limiter *ratelimit.Limiter
	// gen is the registry of shared generation watchers (R-7).
	gen *genWatchers
}

// New creates a Handler.
func New(tg *bot.Client, users *repositories.UserRepository, quiz *services.QuizService) *Handler {
	return &Handler{tg: tg, users: users, quiz: quiz, gen: newGenWatchers()}
}

// WithActionLimiter installs the per-user tap throttle (R-9). nil = off.
func (h *Handler) WithActionLimiter(l *ratelimit.Limiter) *Handler {
	h.limiter = l
	return h
}

// throttled reports (and records) whether this action of the Telegram user
// comes too fast after the previous one. It is checked BEFORE any DB work,
// so tap-spam costs neither the user upsert nor any other query.
func (h *Handler) throttled(tgUserID int64) bool {
	return h.limiter != nil && !h.limiter.Allow(tgUserID)
}

// HandleUpdate processes a single webhook update. It never panics; errors are logged.
func (h *Handler) HandleUpdate(ctx context.Context, upd *bot.Update) {
	defer func() {
		if r := recover(); r != nil {
			logf("panic in update %d: %v", upd.UpdateID, r)
		}
	}()
	switch {
	case upd.Message != nil && upd.Message.From != nil:
		h.handleMessage(ctx, upd.Message)
	case upd.CallbackQuery != nil && upd.CallbackQuery.From != nil:
		h.handleCallback(ctx, upd.CallbackQuery)
	}
}

// ensureUser registers or refreshes the Telegram user.
func (h *Handler) ensureUser(ctx context.Context, from *bot.TgUser) (*models.User, error) {
	return h.users.Upsert(ctx, &models.User{
		TelegramID:   from.ID,
		Username:     from.Username,
		FirstName:    from.FirstName,
		LastName:     from.LastName,
		LanguageCode: from.LanguageCode,
	})
}

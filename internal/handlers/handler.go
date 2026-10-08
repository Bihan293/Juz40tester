// Package handlers routes Telegram updates to business logic and renders UI.
package handlers

import (
	"context"
	"fmt"
	"sync"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
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
	// cfg.UserActionInterval. In memory per instance (ratelimit.Limiter) or
	// shared by the cluster (Redis, RATELIMIT_BACKEND=redis).
	limiter ActionLimiter
	// gen is the registry of shared generation watchers (R-7).
	gen *genWatchers
	// billing (optional): Telegram Stars subscriptions, daily quota, paid
	// weak-topics tests. nil = off (the old behaviour).
	billing *services.BillingService
	// isAdmin (optional) recognises administrators (ADMIN_IDS).
	isAdmin func(tgUserID int64) bool
}

// New creates a Handler.
func New(tg *bot.Client, users *repositories.UserRepository, quiz *services.QuizService) *Handler {
	return &Handler{tg: tg, users: users, quiz: quiz, gen: newGenWatchers()}
}

// ActionLimiter allows at most one action per key per interval
// (ratelimit.Limiter in memory, cluster.RedisActionLimiter shared).
type ActionLimiter interface {
	Allow(key int64) bool
}

// WithActionLimiter installs the per-user tap throttle (R-9). nil = off.
func (h *Handler) WithActionLimiter(l ActionLimiter) *Handler {
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
	_ = h.ProcessUpdate(ctx, upd)
}

// ProcessUpdate is HandleUpdate for the durable update queue (cluster
// mode): it returns an error when the processing crashed (panic), so the
// queue retries the update and dead-letters it after the last attempt.
// Business errors are still answered to the user and logged, not returned.
func (h *Handler) ProcessUpdate(ctx context.Context, upd *bot.Update) (err error) {
	defer func() {
		if r := recover(); r != nil {
			logf("panic in update %d: %v", upd.UpdateID, r)
			err = fmt.Errorf("update %d panicked: %v", upd.UpdateID, r)
		}
	}()
	switch {
	case upd.PreCheckoutQuery != nil && upd.PreCheckoutQuery.From != nil:
		// Payments are never throttled and answered before anything else
		// (Telegram waits at most 10 seconds).
		h.handlePreCheckout(ctx, upd.PreCheckoutQuery)
	case upd.Message != nil && upd.Message.From != nil && upd.Message.SuccessfulPayment != nil:
		return h.handleSuccessfulPayment(ctx, upd.Message)
	case upd.Message != nil && upd.Message.From != nil && upd.Message.RefundedPayment != nil:
		if h.billing != nil {
			h.billing.OnRefundedPayment(ctx, upd.Message.RefundedPayment.TelegramPaymentChargeID)
		}
	case upd.Message != nil && upd.Message.From != nil:
		h.handleMessage(ctx, upd.Message)
	case upd.CallbackQuery != nil && upd.CallbackQuery.From != nil:
		h.handleCallback(ctx, upd.CallbackQuery)
	}
	return nil
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

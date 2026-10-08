// Package handlers routes Telegram updates to business logic and renders UI.
package handlers

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	// sharedUpdates: the updates of one user may be handled by DIFFERENT
	// processes (durable update queue, ROLE=worker × N). The per-process
	// kbHidden shortcut is then wrong — another worker may have shown the
	// menu again meanwhile — and is not used.
	sharedUpdates bool
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
	// admin (optional): the admin panel (statistics, users, broadcasts).
	admin *services.AdminService
	// bcWaker (optional) wakes the broadcast sender after a confirmation.
	bcWaker BroadcastWaker
}

// New creates a Handler.
func New(tg *bot.Client, users *repositories.UserRepository, quiz *services.QuizService) *Handler {
	return &Handler{tg: tg, users: users, quiz: quiz, gen: newGenWatchers()}
}

// WithSharedUpdates marks a process whose users' updates are also handled
// by other processes (cluster mode): per-process UI shortcuts are off.
func (h *Handler) WithSharedUpdates(on bool) *Handler {
	h.sharedUpdates = on
	return h
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
//
// It is the entry point of the in-process dispatcher (QUEUE_BACKEND=memory),
// where nothing retries a failed update: Telegram already got its 200. A
// payment update (successful_payment / refunded_payment) that failed on a
// transient database error is therefore retried here, within the update
// timeout — otherwise the user's Stars are charged and nothing is applied
// (the charge id keeps every retry idempotent).
func (h *Handler) HandleUpdate(ctx context.Context, upd *bot.Update) {
	err := h.ProcessUpdate(ctx, upd)
	if err == nil || !isPaymentUpdate(upd) {
		return
	}
	if err = retryWithBackoff(ctx, paymentRetryDelays, err, func() error { return h.ProcessUpdate(ctx, upd) }); err != nil {
		sp := upd.Message.SuccessfulPayment
		if sp != nil {
			logf("PAYMENT NOT APPLIED: update %d, tg user %d, charge %s, payload %q: %v — apply it by hand (/grant) or refund it (/refund)",
				upd.UpdateID, upd.Message.From.ID, sp.TelegramPaymentChargeID, sp.InvoicePayload, err)
			return
		}
		logf("refunded_payment of update %d not applied: %v", upd.UpdateID, err)
	}
}

// paymentRetryDelays are the pauses between in-process retries of a failed
// payment update (about 30 s in total, inside the default update timeout).
var paymentRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}

// isPaymentUpdate reports a successful_payment / refunded_payment update.
func isPaymentUpdate(upd *bot.Update) bool {
	return upd.Message != nil && upd.Message.From != nil &&
		(upd.Message.SuccessfulPayment != nil || upd.Message.RefundedPayment != nil)
}

// retryWithBackoff calls fn after each delay until it succeeds, the delays
// run out or ctx ends; err is the error of the first (failed) call. It
// returns nil on success, otherwise the last error.
func retryWithBackoff(ctx context.Context, delays []time.Duration, err error, fn func() error) error {
	for _, d := range delays {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		if err = fn(); err == nil {
			return nil
		}
	}
	return err
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
			return h.billing.OnRefundedPayment(ctx, upd.Message.RefundedPayment.TelegramPaymentChargeID)
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

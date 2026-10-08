package handlers

import (
	"context"

	"github.com/Bihan293/Juz40tester/internal/bot"
)

// AnswerPreCheckout answers a pre_checkout_query right away, outside the
// update queues: Telegram waits at most 10 seconds for the answer, and a
// pre_checkout_query that waited behind the user's other updates (the
// per-user order of the durable queue, a busy in-process queue) made the
// payment fail. The validation is exactly the one of the queued path
// (handlePreCheckout). Never panics.
func (h *Handler) AnswerPreCheckout(ctx context.Context, q *bot.PreCheckoutQuery) {
	if q == nil || q.From == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logf("panic in pre-checkout %s: %v", q.ID, r)
		}
	}()
	h.handlePreCheckout(ctx, q)
}

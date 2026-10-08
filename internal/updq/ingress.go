package updq

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/metrics"
)

// enqueueTimeout bounds the store write of one webhook request (Telegram
// waits for our answer; on timeout it gets 503 and re-delivers).
const enqueueTimeout = 5 * time.Second

// Ingress is the POST /telegram/webhook handler of the web role (and of
// ROLE=all with a durable queue): it validates the request, stores the
// update in the queue and answers at once — no update handling here.
//
//	200 — stored, a duplicate (update_id already accepted), or a payload
//	      that can never be handled (malformed / nothing the bot handles);
//	403 — wrong secret token;
//	503 — queue full (backpressure) or the store is unavailable, or the
//	      process is shutting down: Telegram re-delivers the update later.
type Ingress struct {
	secret string
	q      Queue
	// OnEnqueued (optional) is called after a new update was stored (ROLE=all
	// wakes its in-process consumer without waiting for the NOTIFY).
	OnEnqueued func()
	// PreCheckout (optional) answers a pre_checkout_query synchronously,
	// BEFORE the queue: Telegram gives the bot 10 s, and in the queue the
	// query would wait behind the user's earlier updates (per-user order)
	// and the backlog. Set on every role that serves the webhook.
	PreCheckout func(ctx context.Context, q *bot.PreCheckoutQuery)

	closing atomic.Bool
	errLast atomic.Int64
}

// NewIngress creates the webhook ingress.
func NewIngress(secret string, q Queue) *Ingress { return &Ingress{secret: secret, q: q} }

// Close makes the ingress answer 503 (graceful shutdown).
func (in *Ingress) Close() { in.closing.Store(true) }

// ServeHTTP implements POST /telegram/webhook.
func (in *Ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if in.secret != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(in.secret)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if in.closing.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var upd bot.Update
	if err := json.Unmarshal(body, &upd); err != nil || upd.UpdateID <= 0 {
		// Never retry a payload that can never succeed (audit #29).
		log.Printf("webhook: malformed update dropped (%d bytes): %v", len(body), err)
		w.WriteHeader(http.StatusOK)
		return
	}
	key := upd.UserKey()
	if key == 0 {
		// Nothing the bot handles (HandleUpdate would ignore it).
		metrics.Inc(MetricEnqueued, "result", "ignored")
		w.WriteHeader(http.StatusOK)
		return
	}
	if upd.PreCheckoutQuery != nil && in.PreCheckout != nil {
		AnswerPreCheckoutNow(r.Context(), &upd, in.PreCheckout)
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), enqueueTimeout)
	defer cancel()
	inserted, err := in.q.Enqueue(ctx, upd.UpdateID, key, body)
	switch {
	case errors.Is(err, ErrQueueFull):
		metrics.Inc(MetricEnqueued, "result", "full")
		in.logErr("update queue full — answering 503, Telegram will re-deliver")
		w.Header().Set("Retry-After", "1")
		http.Error(w, "busy", http.StatusServiceUnavailable)
	case err != nil:
		metrics.Inc(MetricEnqueued, "result", "error")
		in.logErr("enqueue failed: " + err.Error())
		w.Header().Set("Retry-After", "1")
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case !inserted:
		metrics.Inc(MetricEnqueued, "result", "duplicate")
		metrics.Inc(metrics.DuplicateUpdates)
		w.WriteHeader(http.StatusOK)
	default:
		metrics.Inc(MetricEnqueued, "result", "new")
		if in.OnEnqueued != nil {
			in.OnEnqueued()
		}
		w.WriteHeader(http.StatusOK)
	}
}

// PreCheckoutTimeout bounds the synchronous answer of a pre_checkout_query
// (Telegram's own limit is 10 s).
const PreCheckoutTimeout = 8 * time.Second

// AnswerPreCheckoutNow runs the fast path of a pre_checkout_query: answered
// at once with its own deadline, detached from the webhook request (a
// dropped connection must not cancel the answer).
func AnswerPreCheckoutNow(ctx context.Context, upd *bot.Update, answer func(context.Context, *bot.PreCheckoutQuery)) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PreCheckoutTimeout)
	defer cancel()
	metrics.Inc(MetricEnqueued, "result", "pre_checkout_direct")
	answer(actx, upd.PreCheckoutQuery)
}

func (in *Ingress) logErr(msg string) {
	now := time.Now().Unix()
	if last := in.errLast.Load(); now-last >= 10 && in.errLast.CompareAndSwap(last, now) {
		log.Printf("webhook: %s", msg)
	}
}

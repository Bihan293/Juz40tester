// Package bot implements a minimal Telegram Bot API client over net/http.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bihan293/Juz40tester/internal/httpx"
)

// Client is a thin Telegram Bot API client.
type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string

	// flood keeps 429 back-off state and the deferred-retry queue (R-3).
	flood *floodControl
	// limiter is the global token bucket of message-changing calls (A8,
	// TG_MAX_RPS). answerCallbackQuery and service calls bypass it.
	limiter *rateLimiter
	// shared (optional) is a limiter shared by every instance of the bot
	// (Redis, RATELIMIT_BACKEND=redis): TG_MAX_RPS then holds for the whole
	// cluster, not per process. When it fails (Redis unreachable) the call
	// falls back to the local limiter, so sending never stalls.
	shared SharedLimiter
	// testEnv: requests go to the Telegram TEST environment
	// (…/bot<token>/test/<method>) — Stars payments with test Stars.
	testEnv bool
}

// WithTestEnvironment switches the client to the Telegram test
// environment (TELEGRAM_TEST_ENV=1): test accounts, test Stars.
func (c *Client) WithTestEnvironment(on bool) *Client {
	c.testEnv = on
	return c
}

// NewClient creates a client for the given bot token.
func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: httpx.NewClient(15 * time.Second),
		baseURL:    "https://api.telegram.org",
		flood:      newFloodControl(defaultMaxDeferred),
		limiter:    newRateLimiter(DefaultMaxRPS, defaultBurst),
	}
}

// WithBaseURL points the client at another Bot API server (a local Bot API
// server, or a fake one in tests).
func (c *Client) WithBaseURL(u string) *Client {
	c.baseURL = strings.TrimRight(u, "/")
	return c
}

// --- Incoming update types -------------------------------------------------

// Update is a Telegram webhook update.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	// PreCheckoutQuery: Telegram asks whether a Stars payment may go
	// through (must be answered within 10 seconds).
	PreCheckoutQuery *PreCheckoutQuery `json:"pre_checkout_query,omitempty"`
}

// UserKey returns the key that serialises the processing of updates of
// one user (cluster mode: two updates of the same user are never handled
// in parallel, and they are handled in update_id order). It is the sender
// id; 0 means the update carries nothing the bot handles (HandleUpdate
// ignores it), so it can be acknowledged without queueing.
func (u *Update) UserKey() int64 {
	switch {
	case u == nil:
		return 0
	case u.Message != nil && u.Message.From != nil:
		return u.Message.From.ID
	case u.CallbackQuery != nil && u.CallbackQuery.From != nil:
		return u.CallbackQuery.From.ID
	case u.PreCheckoutQuery != nil && u.PreCheckoutQuery.From != nil:
		return u.PreCheckoutQuery.From.ID
	}
	return 0
}

// TgUser is a Telegram user.
type TgUser struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// Message is a Telegram message.
type Message struct {
	MessageID int64   `json:"message_id"`
	From      *TgUser `json:"from,omitempty"`
	Chat      Chat    `json:"chat"`
	Text      string  `json:"text,omitempty"`
	// SuccessfulPayment: service message about a received Stars payment.
	SuccessfulPayment *SuccessfulPayment `json:"successful_payment,omitempty"`
	// RefundedPayment: service message about a refunded payment.
	RefundedPayment *RefundedPayment `json:"refunded_payment,omitempty"`
}

// PreCheckoutQuery is the last confirmation before a payment.
type PreCheckoutQuery struct {
	ID             string  `json:"id"`
	From           *TgUser `json:"from"`
	Currency       string  `json:"currency"`
	TotalAmount    int     `json:"total_amount"`
	InvoicePayload string  `json:"invoice_payload"`
}

// SuccessfulPayment describes a successful payment (Telegram Stars).
type SuccessfulPayment struct {
	Currency                   string `json:"currency"`
	TotalAmount                int    `json:"total_amount"`
	InvoicePayload             string `json:"invoice_payload"`
	SubscriptionExpirationDate int64  `json:"subscription_expiration_date,omitempty"`
	IsRecurring                bool   `json:"is_recurring,omitempty"`
	IsFirstRecurring           bool   `json:"is_first_recurring,omitempty"`
	TelegramPaymentChargeID    string `json:"telegram_payment_charge_id"`
	ProviderPaymentChargeID    string `json:"provider_payment_charge_id,omitempty"`
}

// RefundedPayment describes a refunded payment.
type RefundedPayment struct {
	Currency                string `json:"currency"`
	TotalAmount             int    `json:"total_amount"`
	InvoicePayload          string `json:"invoice_payload"`
	TelegramPaymentChargeID string `json:"telegram_payment_charge_id"`
}

// LabeledPrice is one price line of an invoice (Stars: amount in ⭐).
type LabeledPrice struct {
	Label  string `json:"label"`
	Amount int    `json:"amount"`
}

// CallbackQuery is an inline-keyboard callback.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *TgUser  `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// --- Keyboards --------------------------------------------------------------

// InlineKeyboardMarkup is an inline keyboard.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton is a single inline button.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
	// Pay: the «Pay» button of an invoice message (must be the first one).
	Pay bool `json:"pay,omitempty"`
}

// URLBtn is a helper to build a link button (e.g. an invoice link).
func URLBtn(text, url string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: text, URL: url}
}

// Row is a helper to build a keyboard row.
func Row(buttons ...InlineKeyboardButton) []InlineKeyboardButton { return buttons }

// Btn is a helper to build a callback button.
func Btn(text, data string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: text, CallbackData: data}
}

// ChunkButtons splits buttons into keyboard rows of size n (last row may be
// shorter). Returns nil for empty input.
func ChunkButtons(buttons []InlineKeyboardButton, n int) [][]InlineKeyboardButton {
	if n <= 0 {
		n = 1
	}
	rows := make([][]InlineKeyboardButton, 0, len(buttons)/n+1)
	for i := 0; i < len(buttons); i += n {
		end := i + n
		if end > len(buttons) {
			end = len(buttons)
		}
		rows = append(rows, buttons[i:end])
	}
	return rows
}

// ReplyKeyboardMarkup is a regular (bottom-of-chat) keyboard.
type ReplyKeyboardMarkup struct {
	Keyboard       [][]KeyboardButton `json:"keyboard"`
	ResizeKeyboard bool               `json:"resize_keyboard"`
}

// ReplyKeyboardRemove hides the bottom-of-chat reply keyboard (used while a
// test is in progress so the menu buttons do not get in the way).
type ReplyKeyboardRemove struct {
	RemoveKeyboard bool `json:"remove_keyboard"`
}

// RemoveKeyboard is the singleton markup that hides the reply keyboard.
var RemoveKeyboard = &ReplyKeyboardRemove{RemoveKeyboard: true}

// KeyboardButton is a single reply-keyboard button.
type KeyboardButton struct {
	Text string `json:"text"`
}

// ReplyRow is a helper to build a reply-keyboard row.
func ReplyRow(texts ...string) []KeyboardButton {
	row := make([]KeyboardButton, 0, len(texts))
	for _, t := range texts {
		row = append(row, KeyboardButton{Text: t})
	}
	return row
}

// --- API calls ---------------------------------------------------------------

type apiResponse struct {
	OK          bool            `json:"ok"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after,omitempty"`
	} `json:"parameters,omitempty"`
}

// maxRateLimitRetries bounds the retries of one call after Telegram 429
// ("Too Many Requests: retry after N"). Never infinite.
const maxRateLimitRetries = 2

// maxRetryAfter caps how long one retry may wait: a longer flood-control
// ban is not worth retrying at all — the call fails instead.
var maxRetryAfter = 30 * time.Second

// maxInlineRetryAfter (R-3) is the longest 429 back-off that is waited out
// INSIDE the calling goroutine (an update-handler slot). A longer
// retry_after is never slept in the handler: deferrable calls (sending /
// editing / deleting messages) are handed to the deferred-retry queue that
// honours retry_after in its own goroutines, everything else fails fast
// with *RateLimitError.
var maxInlineRetryAfter = time.Second

// RateLimitError is returned when Telegram keeps answering 429 after the
// bounded retries (or asks for a wait longer than maxRetryAfter).
// Deferred = true means the request was NOT dropped: it is queued and will
// be re-sent automatically once retry_after has passed (the result, e.g. a
// message id, is then not available to the caller).
type RateLimitError struct {
	Method     string
	RetryAfter time.Duration
	Deferred   bool
}

func (e *RateLimitError) Error() string {
	if e.Deferred {
		return fmt.Sprintf("telegram %s: rate limited, re-send deferred by %s", e.Method, e.RetryAfter)
	}
	return fmt.Sprintf("telegram %s: rate limited (retry after %s)", e.Method, e.RetryAfter)
}

// APIError is a non-429 error answer of the Telegram Bot API (code and
// description as returned by Telegram).
type APIError struct {
	Method      string
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %s", e.Method, e.Description)
}

// IsNotModified reports Telegram's 400 "message is not modified" (the
// content on screen is already up to date).
func IsNotModified(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == http.StatusBadRequest &&
		strings.Contains(ae.Description, "message is not modified")
}

// IsForbidden reports Telegram 403 (bot blocked by the user, kicked from
// the chat, user deactivated).
func IsForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == http.StatusForbidden
}

// IsDeferred reports whether err means "rate limited, the request is queued
// and will be delivered later".
func IsDeferred(err error) bool {
	var rl *RateLimitError
	return errors.As(err, &rl) && rl.Deferred
}

// deferrable lists the methods whose delivery may be postponed: the caller
// only logs their errors and does not depend on an immediate result.
// answerCallbackQuery is excluded (a late answer is useless — the spinner
// stops by itself) and so is setWebhook (it has its own retry loop).
var deferrable = map[string]bool{
	"sendMessage":     true,
	"editMessageText": true,
	"deleteMessage":   true,
	"deleteMessages":  true,
}

// call performs one Bot API request. On HTTP 429 it honours
// parameters.retry_after:
//   - retry_after <= maxInlineRetryAfter: wait and retry in place (at most
//     maxRateLimitRetries times);
//   - longer, up to maxRetryAfter: a deferrable request is queued for a
//     later re-send (RateLimitError{Deferred: true}); other requests fail
//     fast — the handler slot is never blocked for seconds;
//   - longer than maxRetryAfter: fail fast.
//
// While a chat is under a known flood-control ban, new deferrable requests
// to it are queued straight away (no HTTP call that would only get another
// 429), keeping their order behind the already queued ones.
func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	chatID := payloadChatID(payload)
	if c.flood != nil && chatID != 0 && deferrable[method] {
		if wait, queued := c.flood.blocked(chatID); wait > 0 || queued {
			if c.flood.enqueue(c, chatID, method, body, wait) {
				return &RateLimitError{Method: method, RetryAfter: wait, Deferred: true}
			}
			if wait > 0 {
				return &RateLimitError{Method: method, RetryAfter: wait}
			}
		}
	}
	for attempt := 0; ; attempt++ {
		retryAfter, err := c.callOnce(ctx, method, body, out)
		if retryAfter <= 0 {
			return err
		}
		if c.flood != nil && chatID != 0 {
			c.flood.block(chatID, retryAfter)
		}
		if retryAfter > maxRetryAfter {
			return &RateLimitError{Method: method, RetryAfter: retryAfter}
		}
		if retryAfter > maxInlineRetryAfter || attempt >= maxRateLimitRetries {
			if c.flood != nil && chatID != 0 && deferrable[method] && c.flood.enqueue(c, chatID, method, body, retryAfter) {
				return &RateLimitError{Method: method, RetryAfter: retryAfter, Deferred: true}
			}
			return &RateLimitError{Method: method, RetryAfter: retryAfter}
		}
		if c.limiter != nil && limitedMethods[method] {
			// The 429 paused the global limiter (callOnce): the retry waits
			// for it there instead of sleeping here as well.
			continue
		}
		t := time.NewTimer(retryAfter)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// payloadChatID extracts chat_id from a request payload (0 when absent).
func payloadChatID(payload any) int64 {
	m, ok := payload.(map[string]any)
	if !ok {
		return 0
	}
	switch v := m["chat_id"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// safeErr strips the request URL (https://api.telegram.org/bot<TOKEN>/...)
// from transport errors. net/http returns *url.Error whose Error() embeds
// the full URL — i.e. the bot token — and those errors end up in log.Printf
// all over the bot. Only the method name and the underlying cause are kept;
// as defence in depth any remaining occurrence of the token is redacted.
func (c *Client) safeErr(method string, err error) error {
	if err == nil {
		return nil
	}
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if c.token != "" && strings.Contains(err.Error(), c.token) {
		return fmt.Errorf("telegram %s: %s", method, strings.ReplaceAll(err.Error(), c.token, "<redacted>"))
	}
	return fmt.Errorf("telegram %s: %w", method, err)
}

// callOnce sends the request once. retryAfter > 0 means Telegram answered
// 429 and the request may be repeated after that delay.
//
// Message-changing methods first wait for the global limiter (A8); a 429
// pauses the whole limiter for retry_after (capped at maxGlobalPause).
func (c *Client) callOnce(ctx context.Context, method string, body []byte, out any) (retryAfter time.Duration, err error) {
	if err := c.waitLimit(ctx, method); err != nil {
		return 0, err
	}
	url := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	if c.testEnv {
		url = fmt.Sprintf("%s/bot%s/test/%s", c.baseURL, c.token, method)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, c.safeErr(method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, c.safeErr(method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, c.safeErr(method, err)
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return 0, fmt.Errorf("telegram %s: decode: %w", method, err)
	}
	if !ar.OK {
		if resp.StatusCode == http.StatusTooManyRequests || ar.ErrorCode == http.StatusTooManyRequests {
			wait := time.Second
			if ar.Parameters != nil && ar.Parameters.RetryAfter > 0 {
				wait = time.Duration(ar.Parameters.RetryAfter) * time.Second
			}
			if c.limiter != nil {
				c.limiter.pause(min(wait, maxGlobalPause))
			}
			if c.shared != nil {
				c.shared.Pause(min(wait, maxGlobalPause))
			}
			return wait, fmt.Errorf("telegram %s: %s", method, ar.Description)
		}
		return 0, &APIError{Method: method, Code: ar.ErrorCode, Description: ar.Description}
	}
	if out != nil && len(ar.Result) > 0 {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return 0, fmt.Errorf("telegram %s: result: %w", method, err)
		}
	}
	return 0, nil
}

// maxMessageRunes is the Telegram limit for one message text (4096
// characters). AI-generated question payloads could in theory push a
// rendered question past it — such a send would fail with HTTP 400 and the
// test would look frozen, so texts are truncated defensively instead.
const maxMessageRunes = 4096

// maxCallbackAnswerRunes is the Telegram limit for the answerCallbackQuery
// popup text (200 characters) — longer texts are rejected with HTTP 400
// and the button keeps spinning.
const maxCallbackAnswerRunes = 200

// truncateRunes cuts s to at most n runes, appending an ellipsis when cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// SendMessage sends a new message and returns its id. The keyboard may be
// *InlineKeyboardMarkup, *ReplyKeyboardMarkup or *ReplyKeyboardRemove.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, kb any) (int64, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    truncateRunes(text, maxMessageRunes),
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	var msg Message
	if err := c.call(ctx, "sendMessage", payload, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// EditMessageText edits an existing message text and keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string, kb *InlineKeyboardMarkup) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       truncateRunes(text, maxMessageRunes),
	}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	return c.call(ctx, "editMessageText", payload, nil)
}

// DeleteMessage removes a message (best-effort helper — e.g. the transient
// note used to hide the reply keyboard).
func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	}, nil)
}

// DeleteMessages removes several messages of one chat with ONE request
// (Bot API deleteMessages). If the server does not know the method (an old
// local Bot API server) it falls back to one deleteMessage per id.
func (c *Client) DeleteMessages(ctx context.Context, chatID int64, ids []int64) error {
	switch len(ids) {
	case 0:
		return nil
	case 1:
		return c.DeleteMessage(ctx, chatID, ids[0])
	}
	err := c.call(ctx, "deleteMessages", map[string]any{
		"chat_id":     chatID,
		"message_ids": ids,
	}, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "not found") {
		return err
	}
	var errs []error
	for _, id := range ids {
		if derr := c.DeleteMessage(ctx, chatID, id); derr != nil {
			errs = append(errs, derr)
		}
	}
	return errors.Join(errs...)
}

// AnswerCallbackQuery acknowledges a callback (stops the loading spinner).
// The popup text is capped at the Telegram limit of 200 characters.
func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	payload := map[string]any{"callback_query_id": id}
	if text != "" {
		payload["text"] = truncateRunes(text, maxCallbackAnswerRunes)
	}
	return c.call(ctx, "answerCallbackQuery", payload, nil)
}

// AnswerCallbackAlert acknowledges a callback with a MODAL alert (a dialog
// with an OK button) instead of the tiny toast that disappears after a few
// seconds — used for important notices the user must actually read.
func (c *Client) AnswerCallbackAlert(ctx context.Context, id, text string) error {
	payload := map[string]any{
		"callback_query_id": id,
		"text":              truncateRunes(text, maxCallbackAnswerRunes),
		"show_alert":        true,
	}
	return c.call(ctx, "answerCallbackQuery", payload, nil)
}

// SetWebhook registers the webhook URL for the bot. When secretToken is not
// empty Telegram sends it back in the X-Telegram-Bot-Api-Secret-Token
// header of every update, so forged requests can be rejected.
func (c *Client) SetWebhook(ctx context.Context, webhookURL, secretToken string) error {
	payload := map[string]any{
		"url":             webhookURL,
		"allowed_updates": []string{"message", "callback_query", "pre_checkout_query"},
	}
	if secretToken != "" {
		payload["secret_token"] = secretToken
	}
	return c.call(ctx, "setWebhook", payload, nil)
}

// --- Telegram Stars payments ------------------------------------------------

// Invoice is the content of a Stars invoice (currency XTR, no provider
// token). SubscriptionPeriod > 0 makes it a monthly subscription (only
// createInvoiceLink supports it; Telegram requires exactly 2592000).
type Invoice struct {
	Title              string
	Description        string
	Payload            string
	Prices             []LabeledPrice
	SubscriptionPeriod int
}

func (inv Invoice) payload() map[string]any {
	p := map[string]any{
		"title":       truncateRunes(inv.Title, 32),
		"description": truncateRunes(inv.Description, 255),
		"payload":     inv.Payload,
		"currency":    "XTR",
		"prices":      inv.Prices,
	}
	if inv.SubscriptionPeriod > 0 {
		p["subscription_period"] = inv.SubscriptionPeriod
	}
	return p
}

// CreateInvoiceLink creates a payment link for the invoice (Bot API
// createInvoiceLink). The link is not bound to a user: whoever pays is the
// From of the payment updates.
func (c *Client) CreateInvoiceLink(ctx context.Context, inv Invoice) (string, error) {
	var link string
	if err := c.call(ctx, "createInvoiceLink", inv.payload(), &link); err != nil {
		return "", err
	}
	return link, nil
}

// SendInvoice sends an invoice message to the chat (one-off payments).
func (c *Client) SendInvoice(ctx context.Context, chatID int64, inv Invoice, kb *InlineKeyboardMarkup) (int64, error) {
	p := inv.payload()
	delete(p, "subscription_period") // not supported by sendInvoice
	p["chat_id"] = chatID
	if kb != nil {
		p["reply_markup"] = kb
	}
	var msg Message
	if err := c.call(ctx, "sendInvoice", p, &msg); err != nil {
		return 0, err
	}
	return msg.MessageID, nil
}

// AnswerPreCheckoutQuery confirms (ok) or rejects a payment; errMsg is
// shown to the user when rejected.
func (c *Client) AnswerPreCheckoutQuery(ctx context.Context, id string, ok bool, errMsg string) error {
	p := map[string]any{"pre_checkout_query_id": id, "ok": ok}
	if !ok {
		p["error_message"] = truncateRunes(errMsg, 255)
	}
	return c.call(ctx, "answerPreCheckoutQuery", p, nil)
}

// RefundStarPayment refunds a successful Stars payment.
func (c *Client) RefundStarPayment(ctx context.Context, userID int64, chargeID string) error {
	return c.call(ctx, "refundStarPayment", map[string]any{
		"user_id":                    userID,
		"telegram_payment_charge_id": chargeID,
	}, nil)
}

// EditUserStarSubscription cancels (canceled = true) or re-enables the
// auto-renewal of a user's Stars subscription.
func (c *Client) EditUserStarSubscription(ctx context.Context, userID int64, chargeID string, canceled bool) error {
	return c.call(ctx, "editUserStarSubscription", map[string]any{
		"user_id":                    userID,
		"telegram_payment_charge_id": chargeID,
		"is_canceled":                canceled,
	}, nil)
}

// IsAlreadyRefunded reports Telegram's answer for a payment that has
// already been refunded (the refund is then done — not an error).
func IsAlreadyRefunded(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && strings.Contains(strings.ToUpper(ae.Description), "ALREADY_REFUNDED")
}

// Package bot implements a minimal Telegram Bot API client over net/http.
package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"
)

// Client is a thin Telegram Bot API client.
type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a client for the given bot token.
func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		baseURL:    "https://api.telegram.org",
	}
}

// --- Incoming update types -------------------------------------------------

// Update is a Telegram webhook update.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
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
	Description string          `json:"description,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("telegram %s: decode: %w", method, err)
	}
	if !ar.OK {
		return fmt.Errorf("telegram %s: %s", method, ar.Description)
	}
	if out != nil && len(ar.Result) > 0 {
		if err := json.Unmarshal(ar.Result, out); err != nil {
			return fmt.Errorf("telegram %s: result: %w", method, err)
		}
	}
	return nil
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
		"allowed_updates": []string{"message", "callback_query"},
	}
	if secretToken != "" {
		payload["secret_token"] = secretToken
	}
	return c.call(ctx, "setWebhook", payload, nil)
}

package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/metrics"
	"github.com/Bihan293/Juz40tester/internal/repositories"
)

// --- Content validation (pure) ----------------------------------------------------

// Broadcast content limits.
const (
	BroadcastMaxButtonRows   = 8
	BroadcastMaxButtonsInRow = 3
	broadcastMaxButtonText   = 64
)

// ParseBroadcastButtons parses the admin's button input: one row per line,
// «Текст | https://ссылка»; several buttons in one row are separated by
// «;;». Empty lines are ignored.
//
//	Открыть сайт | https://juz40.kz
//	Канал | https://t.me/juz40 ;; Чат | https://t.me/juz40chat
func ParseBroadcastButtons(input string) ([][]repositories.BroadcastButton, error) {
	var rows [][]repositories.BroadcastButton
	for n, line := range strings.Split(strings.ReplaceAll(input, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row []repositories.BroadcastButton
		for _, part := range strings.Split(line, ";;") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			i := strings.LastIndex(part, "|")
			if i <= 0 {
				return nil, fmt.Errorf("строка %d: нужен формат «Текст | ссылка»", n+1)
			}
			text, url := strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
			if text == "" {
				return nil, fmt.Errorf("строка %d: пустой текст кнопки", n+1)
			}
			if utf8.RuneCountInString(text) > broadcastMaxButtonText {
				return nil, fmt.Errorf("строка %d: текст кнопки длиннее %d символов", n+1, broadcastMaxButtonText)
			}
			if !validButtonURL(url) {
				return nil, fmt.Errorf("строка %d: ссылка должна начинаться с https://, http:// или tg://", n+1)
			}
			row = append(row, repositories.BroadcastButton{Text: text, URL: url})
		}
		if len(row) > BroadcastMaxButtonsInRow {
			return nil, fmt.Errorf("строка %d: не больше %d кнопок в ряду", n+1, BroadcastMaxButtonsInRow)
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	if len(rows) > BroadcastMaxButtonRows {
		return nil, fmt.Errorf("не больше %d рядов кнопок", BroadcastMaxButtonRows)
	}
	if len(rows) == 0 {
		return nil, errors.New("не нашёл ни одной кнопки")
	}
	return rows, nil
}

func validButtonURL(u string) bool {
	if strings.ContainsAny(u, " \t\n") || len(u) > 2048 {
		return false
	}
	l := strings.ToLower(u)
	for _, p := range []string{"https://", "http://", "tg://"} {
		if strings.HasPrefix(l, p) && len(u) > len(p) {
			return true
		}
	}
	return false
}

// ValidateBroadcast checks the content of a broadcast against the
// Telegram limits.
func ValidateBroadcast(b *repositories.Broadcast) error {
	n := utf8.RuneCountInString(b.Text)
	switch b.MediaType {
	case "":
		if strings.TrimSpace(b.Text) == "" {
			return errors.New("пустой текст")
		}
		if n > bot.MaxTextRunes {
			return fmt.Errorf("текст длиннее %d символов (сейчас %d)", bot.MaxTextRunes, n)
		}
	case "photo", "video":
		if b.MediaFileID == "" {
			return errors.New("нет файла")
		}
		if n > bot.MaxCaptionRunes {
			return fmt.Errorf("подпись к фото/видео — не длиннее %d символов (сейчас %d). Сократи текст или отправь без медиа", bot.MaxCaptionRunes, n)
		}
	default:
		return fmt.Errorf("неизвестный тип медиа %q", b.MediaType)
	}
	if len(b.Buttons) > BroadcastMaxButtonRows {
		return fmt.Errorf("не больше %d рядов кнопок", BroadcastMaxButtonRows)
	}
	return nil
}

// BroadcastMessage renders a stored broadcast for the Bot API.
func BroadcastMessage(b *repositories.Broadcast) bot.BroadcastMessage {
	m := bot.BroadcastMessage{Text: b.Text, Entities: b.Entities, MediaType: b.MediaType, MediaFileID: b.MediaFileID}
	if len(b.Buttons) > 0 {
		kb := &bot.InlineKeyboardMarkup{}
		for _, row := range b.Buttons {
			var r []bot.InlineKeyboardButton
			for _, x := range row {
				r = append(r, bot.URLBtn(x.Text, x.URL))
			}
			kb.InlineKeyboard = append(kb.InlineKeyboard, r)
		}
		m.Keyboard = kb
	}
	return m
}

// --- Sender (worker side) ---------------------------------------------------------------

// BroadcastAPI sends one broadcast message (bot.Client).
type BroadcastAPI interface {
	SendBroadcast(ctx context.Context, chatID int64, m bot.BroadcastMessage) (int64, error)
}

// BroadcastNotifier reports a finished broadcast to its admin.
type BroadcastNotifier interface {
	BroadcastFinished(ctx context.Context, b *repositories.Broadcast)
}

// BroadcastSettings tunes the sender.
type BroadcastSettings struct {
	// RPS caps the broadcast messages per second of the whole cluster
	// (one broadcast is sent by one worker at a time). The global
	// TG_MAX_RPS limiter of the client applies on top, so interactive
	// traffic always keeps a share.
	RPS         int
	Lease       time.Duration // broadcast lease of a sender (renewed while sending)
	Poll        time.Duration // idle poll for new broadcasts (Wake skips it)
	MaxAttempts int           // retries of a recipient after 5xx / network errors
	Batch       int
}

// BroadcastService sends the queued broadcasts in the background. It runs
// on every worker (ROLE=worker / all); a broadcast is owned by one worker
// at a time (lease in PostgreSQL), progress is durable per recipient, so a
// restart continues where it stopped and nobody gets a message twice.
type BroadcastService struct {
	repo   *repositories.AdminRepository
	api    BroadcastAPI
	notify BroadcastNotifier
	worker string
	set    BroadcastSettings
	wake   chan struct{}
	sleep  func(ctx context.Context, d time.Duration) bool
}

// NewBroadcastService wires the sender. worker identifies this process
// (INSTANCE_ID).
func NewBroadcastService(repo *repositories.AdminRepository, api BroadcastAPI, worker string, set BroadcastSettings) *BroadcastService {
	if set.RPS <= 0 {
		set.RPS = 15
	}
	if set.Lease <= 0 {
		set.Lease = time.Minute
	}
	if set.Poll <= 0 {
		set.Poll = 30 * time.Second
	}
	if set.MaxAttempts <= 0 {
		set.MaxAttempts = 3
	}
	if set.Batch <= 0 {
		set.Batch = 50
	}
	return &BroadcastService{repo: repo, api: api, worker: worker, set: set, wake: make(chan struct{}, 1), sleep: sleepCtx}
}

// WithNotifier wires the «broadcast finished» report.
func (s *BroadcastService) WithNotifier(n BroadcastNotifier) *BroadcastService {
	s.notify = n
	return s
}

// Wake makes the sender look for work at once (non-blocking).
func (s *BroadcastService) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run is the sender loop of a worker.
func (s *BroadcastService) Run(ctx context.Context) {
	t := time.NewTicker(s.set.Poll)
	defer t.Stop()
	lastCleanup := time.Time{}
	for {
		for ctx.Err() == nil {
			worked, err := s.RunOnce(ctx)
			if err != nil {
				log.Printf("broadcast: %v", err)
				break
			}
			if !worked {
				break
			}
		}
		if time.Since(lastCleanup) > 6*time.Hour {
			lastCleanup = time.Now()
			if n, err := s.repo.DeleteStaleDrafts(ctx, 7*24*time.Hour); err != nil {
				log.Printf("broadcast: drafts cleanup: %v", err)
			} else if n > 0 {
				log.Printf("broadcast: %d stale draft(s) deleted", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// RunOnce claims one broadcast and sends it until it is finished,
// cancelled, taken over or ctx ends. worked = a broadcast was processed.
func (s *BroadcastService) RunOnce(ctx context.Context) (bool, error) {
	b, err := s.repo.ClaimBroadcast(ctx, s.worker, s.set.Lease)
	if err != nil || b == nil {
		return false, err
	}
	log.Printf("broadcast %d: sending (worker %s, %d recipient(s), %d msg/s)", b.ID, s.worker, b.Total, s.set.RPS)
	err = s.send(ctx, b)
	if ctx.Err() != nil {
		// Graceful shutdown: give the broadcast back so another worker (or
		// this one after the restart) continues at once.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rerr := s.repo.ReleaseBroadcast(rctx, b.ID, s.worker); rerr != nil {
			log.Printf("broadcast %d: release: %v", b.ID, rerr)
		}
		return false, nil
	}
	return true, err
}

// errStop: the broadcast was cancelled or taken over.
var errStop = errors.New("broadcast stopped")

func (s *BroadcastService) send(ctx context.Context, b *repositories.Broadcast) error {
	msg := BroadcastMessage(b)
	interval := time.Second / time.Duration(s.set.RPS)
	renewed := time.Now()
	var next time.Time
	for ctx.Err() == nil {
		batch, err := s.repo.DueRecipients(ctx, b.ID, s.set.Batch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			fin, err := s.repo.FinishBroadcastIfDone(ctx, b.ID)
			if err != nil {
				return err
			}
			if fin != nil {
				s.finished(ctx, fin)
				return nil
			}
			wait, pending, err := s.repo.NextDueIn(ctx, b.ID)
			if err != nil {
				return err
			}
			if !pending {
				// Cancelled meanwhile (nothing pending, not finishable).
				return nil
			}
			ok, err := s.repo.RenewBroadcastLease(ctx, b.ID, s.worker, s.set.Lease)
			if err != nil || !ok {
				return err
			}
			renewed = time.Now()
			s.sleep(ctx, min(max(wait, 100*time.Millisecond), s.set.Lease/3))
			continue
		}
		for _, rc := range batch {
			if time.Since(renewed) > s.set.Lease/3 {
				ok, err := s.repo.RenewBroadcastLease(ctx, b.ID, s.worker, s.set.Lease)
				if err != nil {
					return err
				}
				if !ok {
					log.Printf("broadcast %d: cancelled or taken over — worker %s stops", b.ID, s.worker)
					return nil
				}
				renewed = time.Now()
			}
			if d := time.Until(next); d > 0 && !s.sleep(ctx, d) {
				return nil
			}
			next = time.Now().Add(interval)
			if err := s.deliver(ctx, b, rc, msg); err != nil {
				if errors.Is(err, errStop) {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

// deliver sends one message and records the result. Only Telegram's own
// refusals (429, 5xx) and errors where no request left the process are
// retried — everything else is final, so a recipient never gets the
// message twice.
func (s *BroadcastService) deliver(ctx context.Context, b *repositories.Broadcast, rc repositories.BroadcastRecipient, msg bot.BroadcastMessage) error {
	ok, err := s.repo.MarkRecipientSending(ctx, b.ID, rc.UserID)
	if err != nil {
		return err
	}
	if !ok {
		return nil // cancelled meanwhile
	}
	_, serr := s.api.SendBroadcast(ctx, rc.TelegramID, msg)
	// The result is stored even when ctx was cancelled during the call.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	status, retryIn, errText := classifyBroadcastError(serr, rc.Attempts+1, s.set.MaxAttempts)
	switch {
	case retryIn > 0:
		metrics.Inc(metrics.BroadcastMessages, "result", "retry")
		var rl *bot.RateLimitError
		if errors.As(serr, &rl) {
			metrics.Inc(metrics.BroadcastMessages, "result", "rate_limited")
			// Everything else waits too: Telegram limits the whole bot.
			defer s.sleep(ctx, min(rl.RetryAfter, 30*time.Second))
		}
		return s.repo.RetryRecipient(wctx, b.ID, rc.UserID, retryIn, errText)
	case status == repositories.RecipientBlocked:
		metrics.Inc(metrics.BroadcastMessages, "result", "blocked")
		if err := s.repo.MarkUserBlocked(wctx, rc.UserID); err != nil {
			log.Printf("broadcast %d: mark user %d blocked: %v", b.ID, rc.UserID, err)
		}
	default:
		metrics.Inc(metrics.BroadcastMessages, "result", status)
	}
	return s.repo.FinishRecipient(wctx, b.ID, rc.UserID, status, errText)
}

// classifyBroadcastError maps a send result to the recipient status:
// sent / blocked / failed, or a retry delay (> 0) for temporary refusals.
func classifyBroadcastError(err error, attempt, maxAttempts int) (status string, retryIn time.Duration, text string) {
	if err == nil {
		return repositories.RecipientSent, 0, ""
	}
	text = err.Error()
	var rl *bot.RateLimitError
	if errors.As(err, &rl) {
		// 429: Telegram did not deliver — always safe to retry.
		return repositories.RecipientPending, max(rl.RetryAfter, time.Second), text
	}
	var ae *bot.APIError
	if errors.As(err, &ae) {
		desc := strings.ToLower(ae.Description)
		switch {
		case ae.Code == http.StatusForbidden,
			strings.Contains(desc, "user is deactivated"),
			strings.Contains(desc, "bot was blocked"):
			return repositories.RecipientBlocked, 0, text
		case ae.Code >= 500 && attempt < maxAttempts:
			return repositories.RecipientPending, time.Duration(attempt) * 30 * time.Second, text
		}
		return repositories.RecipientFailed, 0, text
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Shutdown in the middle of the call: delivery unknown → final.
		return repositories.RecipientFailed, 0, "прервано (доставка неизвестна)"
	}
	if attempt < maxAttempts {
		// Network error: almost always no request reached Telegram.
		return repositories.RecipientPending, time.Duration(attempt) * 30 * time.Second, text
	}
	return repositories.RecipientFailed, 0, text
}

func (s *BroadcastService) finished(ctx context.Context, b *repositories.Broadcast) {
	dur := time.Duration(0)
	if b.StartedAt != nil && b.FinishedAt != nil {
		dur = b.FinishedAt.Sub(*b.StartedAt)
	}
	log.Printf("broadcast %d: done in %s — total %d, sent %d, blocked %d, failed %d", b.ID, dur.Round(time.Second), b.Total, b.Sent, b.Blocked, b.Failed)
	if s.notify != nil {
		s.notify.BroadcastFinished(ctx, b)
	}
}

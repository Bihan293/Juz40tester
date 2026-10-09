package handlers

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/models"
)

// Generation watchers (R-7).
//
// Every user waiting for the same generation subscribes to ONE shared key
// without the user id (`chain:<subject>:<number>`, personal tests
// `job:<id>`). One goroutine per key polls with a light query every
// 15–30 s (jitter) as a fallback; the generation worker of this process
// kicks the key right after CompleteJob/FailJob, so the subscribers get the
// result at once. The goroutine ends on result, timeout or shutdown and
// always removes the key — no leaks.

const (
	// genWatchInterval / genWatchJitter: fallback poll period 15–30 s.
	genWatchInterval = 15 * time.Second
	genWatchJitter   = 15 * time.Second
	// genWatchTimeout caps the wait (a real generation takes ~1–3 minutes,
	// the worker's per-job timeout is 8 minutes).
	genWatchTimeout = 10 * time.Minute
)

type genSubscriber struct{ chatID, msgID int64 }

// genCheck reports the produced test (nil = not yet) and whether the
// generation is still pending/running.
type genCheck func(context.Context) (*models.Test, bool, error)

type genWatch struct {
	subs      []genSubscriber
	retryData string
	kick      chan struct{} // buffered(1): «the job finished, check now»
}

// genWatchers is the in-memory registry key -> watch.
type genWatchers struct {
	mu       sync.Mutex
	m        map[string]*genWatch
	wg       sync.WaitGroup
	stop     chan struct{}
	stopOnce sync.Once
	// poll interval / timeout — overridable in tests.
	interval, jitter, timeout time.Duration
}

func newGenWatchers() *genWatchers {
	return &genWatchers{
		m: map[string]*genWatch{}, stop: make(chan struct{}),
		interval: genWatchInterval, jitter: genWatchJitter, timeout: genWatchTimeout,
	}
}

func chainWatchKey(subjectID int64, number int) string {
	return fmt.Sprintf("chain:%d:%d", subjectID, number)
}

func jobWatchKey(jobID int64) string { return "job:" + strconv.FormatInt(jobID, 10) }

// bankWatchKey: a user waiting for topic_batch jobs of the subject (B4b).
// Per user — the check assembles THIS user's test from the bank.
func bankWatchKey(subjectID, userID int64) string {
	return bankWatchPrefix(subjectID) + strconv.FormatInt(userID, 10)
}

func bankWatchPrefix(subjectID int64) string { return fmt.Sprintf("bank:%d:", subjectID) }

// kickPrefix kicks every key starting with prefix (non-blocking).
func (w *genWatchers) kickPrefix(prefix string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, gw := range w.m {
		if strings.HasPrefix(k, prefix) {
			select {
			case gw.kick <- struct{}{}:
			default:
			}
		}
	}
}

// isSubscribed reports whether the chat already waits on the key.
func (w *genWatchers) isSubscribed(key string, chatID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gw, ok := w.m[key]; ok {
		for _, s := range gw.subs {
			if s.chatID == chatID {
				return true
			}
		}
	}
	return false
}

// kick asks the key's poller to check right now (non-blocking).
func (w *genWatchers) kick(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gw, ok := w.m[key]; ok {
		select {
		case gw.kick <- struct{}{}:
		default:
		}
	}
}

// take removes the key and returns its subscribers — only while gw is
// still the key's watch. A poller that already finished must never remove
// (and orphan) a NEWER watch registered under the same key in the
// meantime: its subscribers would never be told the result.
func (w *genWatchers) take(key string, gw *genWatch) []genSubscriber {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.m[key]; !ok || cur != gw {
		return nil
	}
	delete(w.m, key)
	return gw.subs
}

// subscribeGeneration adds (chatID, msgID) to the key's subscribers and
// starts the single poller when the key is new. Returns true when a new
// poller was started.
func (h *Handler) subscribeGeneration(key string, chatID, msgID int64, check genCheck, retryData string) bool {
	w := h.gen
	w.mu.Lock()
	select {
	case <-w.stop: // shutting down: no new goroutines
		w.mu.Unlock()
		return false
	default:
	}
	if gw, ok := w.m[key]; ok {
		gw.subs = append(gw.subs, genSubscriber{chatID, msgID})
		w.mu.Unlock()
		return false
	}
	gw := &genWatch{subs: []genSubscriber{{chatID, msgID}}, retryData: retryData, kick: make(chan struct{}, 1)}
	w.m[key] = gw
	w.wg.Add(1)
	w.mu.Unlock()
	go h.pollGeneration(key, gw, check)
	return true
}

// pollGeneration is the ONE poller of a key.
func (h *Handler) pollGeneration(key string, gw *genWatch, check genCheck) {
	w := h.gen
	defer w.wg.Done()
	defer w.take(key, gw) // always unregister (result, timeout, shutdown, panic)
	defer func() {
		if r := recover(); r != nil {
			logf("generation watcher %s panicked: %v", key, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout+30*time.Second)
	defer cancel()
	go func() {
		select {
		case <-w.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	deadline := time.Now().Add(w.timeout)
	misses := 0 // consecutive checks with neither a test nor an active job
	timer := time.NewTimer(w.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-gw.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		test, pending, err := check(ctx)
		switch {
		case err != nil:
			logf("generation watcher %s: %v", key, err)
		case test != nil:
			h.finishGeneration(ctx, key, gw, readyText(test), &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
				bot.Row(bot.Btn("▶️ Начать тест", cbOpenTest+strconv.FormatInt(test.ID, 10))),
			}})
			return
		case !pending:
			// No test and no active job: the generation failed for good. Two
			// consecutive misses guard against the short window between the
			// job being marked done and the test becoming visible.
			misses++
			if misses >= 2 {
				h.finishGeneration(ctx, key, gw, "😔 Не получилось сгенерировать тест. Нажми «🔄 Попробовать ещё раз» — я перезапущу генерацию.",
					retryKeyboard("🔄 Попробовать ещё раз", gw.retryData))
				return
			}
		default:
			misses = 0
		}
		if time.Now().After(deadline) {
			h.finishGeneration(ctx, key, gw, "⏳ Генерация идёт дольше обычного. Нажми «🔄 Обновить» чуть позже — если тест уже готов, он сразу откроется.",
				retryKeyboard("🔄 Обновить", gw.retryData))
			return
		}
		next := w.nextDelay()
		if misses > 0 && next > 5*time.Second {
			next = 5 * time.Second // confirm a miss quickly
		}
		timer.Reset(next)
	}
}

func (w *genWatchers) nextDelay() time.Duration {
	d := w.interval
	if w.jitter > 0 {
		d += time.Duration(rand.Int63n(int64(w.jitter)))
	}
	return d
}

func readyText(test *models.Test) string {
	ready := fmt.Sprintf("✅ Готово! «Тест %d» сгенерирован.", test.TestNumber)
	if test.Kind == models.TestKindPersonal {
		ready = "✅ Готово! Персональный тест по твоим слабым темам собран."
	}
	return ready + "\n\nНажми кнопку ниже, чтобы начать 👇"
}

func retryKeyboard(label, retryData string) *bot.InlineKeyboardMarkup {
	return &bot.InlineKeyboardMarkup{InlineKeyboard: [][]bot.InlineKeyboardButton{
		bot.Row(bot.Btn(label, retryData)),
		bot.Row(bot.Btn("⬅️ Главное меню", cbMainMenu)),
	}}
}

// finishGeneration unregisters the key and edits every subscriber's note.
func (h *Handler) finishGeneration(ctx context.Context, key string, gw *genWatch, text string, kb *bot.InlineKeyboardMarkup) {
	for _, s := range h.gen.take(key, gw) {
		h.editNote(ctx, s.chatID, s.msgID, text, kb)
	}
}

// NotifyJobFinished is called by the generation worker of this process
// right after a job was completed or failed: the waiting users get the
// result immediately instead of on the next poll (R-7).
func (h *Handler) NotifyJobFinished(job *models.GenerationJob) {
	if job == nil {
		return
	}
	switch job.Kind {
	case models.TestKindChain:
		h.gen.kick(chainWatchKey(job.SubjectID, job.TestNumber))
	case models.JobKindTopicBatch:
		// B4b: every user of the subject waiting for the bank re-assembles.
		h.gen.kickPrefix(bankWatchPrefix(job.SubjectID))
	default:
		h.gen.kick(jobWatchKey(job.ID))
	}
}

// Close stops all generation watchers (shutdown) and waits for them at most
// d. Safe to call more than once.
func (h *Handler) Close(d time.Duration) {
	w := h.gen
	w.mu.Lock()
	w.stopOnce.Do(func() { close(w.stop) })
	w.mu.Unlock()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		logf("shutdown: generation watchers did not stop within %s", d)
	}
}

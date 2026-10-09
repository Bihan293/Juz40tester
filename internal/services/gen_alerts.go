package services

// Operational alerts for the administrators (ADMIN_IDS) about the AI
// pipeline: a generation job that failed for good, generations that keep
// failing, stuck jobs parked by the reaper, and the DeepSeek daily cap.
//
// Every alert has a KEY and a minimum interval: the same key is sent at
// most once per interval. The interval is claimed cluster-wide in
// PostgreSQL when a claim function is wired (two instances during a deploy
// and restarts never repeat an alert), with an in-memory guard on top (and
// as the fallback when the database is unavailable). Sending never blocks
// the generation worker: alerts go out from a goroutine with a short
// timeout, and a failed send is only logged.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/models"
)

const (
	// alertJobFailedEvery: one alert per failed generation slot (chain
	// test / personal test / bank topic) per this interval.
	alertJobFailedEvery = 6 * time.Hour
	// alertStreakEvery: «generations keep failing» at most this often.
	alertStreakEvery = time.Hour
	// alertStreakAfter: consecutive failed job runs (any job, any attempt)
	// that make a streak worth an alert.
	alertStreakAfter = 3
	// alertStuckEvery: «stuck jobs parked as failed» at most this often.
	alertStuckEvery = time.Hour
	// alertCapEvery: the DeepSeek daily cap alert — once per UTC day (the
	// key carries the date; the interval only guards repeats of that key).
	alertCapEvery = 20 * time.Hour
	// alertSendTimeout bounds the Telegram sends of one alert.
	alertSendTimeout = 15 * time.Second
	// alertErrMax caps the error excerpt in an alert (runes).
	alertErrMax = 400
)

// AdminAlerter sends rate-limited alerts to the administrators.
type AdminAlerter struct {
	admins []int64
	send   func(ctx context.Context, chatID int64, text string) error
	// claim (optional) claims key for every cluster-wide: true = this
	// process may send now.
	claim func(ctx context.Context, key string, every time.Duration) (bool, error)
	now   func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
	wg   sync.WaitGroup
}

// NewAdminAlerter returns nil when there is nobody to alert or no sender.
func NewAdminAlerter(admins []int64, send func(ctx context.Context, chatID int64, text string) error) *AdminAlerter {
	if len(admins) == 0 || send == nil {
		return nil
	}
	return &AdminAlerter{
		admins: append([]int64(nil), admins...), send: send,
		now: time.Now, last: map[string]time.Time{},
	}
}

// WithClaim installs the cluster-wide claim of an alert interval.
func (a *AdminAlerter) WithClaim(claim func(ctx context.Context, key string, every time.Duration) (bool, error)) *AdminAlerter {
	if a != nil {
		a.claim = claim
	}
	return a
}

// allowLocal is the in-memory interval guard; it books the slot.
func (a *AdminAlerter) allowLocal(key string, every time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if t, ok := a.last[key]; ok && now.Sub(t) < every {
		return false
	}
	a.last[key] = now
	// Keep the map small: forget keys whose interval is long over.
	if len(a.last) > 512 {
		for k, t := range a.last {
			if now.Sub(t) > 24*time.Hour {
				delete(a.last, k)
			}
		}
	}
	return true
}

// Alert sends text to every administrator unless key was alerted within
// every. Synchronous; returns whether the alert was sent (to anyone).
func (a *AdminAlerter) Alert(ctx context.Context, key string, every time.Duration, text string) bool {
	if a == nil || !a.allowLocal(key, every) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), alertSendTimeout)
	defer cancel()
	if a.claim != nil {
		ok, err := a.claim(ctx, key, every)
		if err != nil {
			log.Printf("alerts: claim %q: %v (sending anyway — local guard only)", key, err)
		} else if !ok {
			return false // another instance (or a previous run) already sent it
		}
	}
	sent := false
	for _, id := range a.admins {
		if err := a.send(ctx, id, text); err != nil {
			log.Printf("alerts: send %q to admin %d: %v", key, id, err)
			continue
		}
		sent = true
	}
	if sent {
		log.Printf("alerts: %q sent to %d admin(s)", key, len(a.admins))
	}
	return sent
}

// AlertAsync is Alert in a goroutine (never blocks the caller).
func (a *AdminAlerter) AlertAsync(ctx context.Context, key string, every time.Duration, text string) {
	if a == nil {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("alerts: %q panicked: %v", key, r)
			}
		}()
		a.Alert(ctx, key, every, text)
	}()
}

// Wait blocks until the in-flight async alerts are done (tests, shutdown).
func (a *AdminAlerter) Wait() {
	if a != nil {
		a.wg.Wait()
	}
}

// shortErr renders an error for an alert: one line, bounded.
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > alertErrMax {
		s = string(r[:alertErrMax]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// DeepSeek daily cap
// ---------------------------------------------------------------------------

// capAlertBudget wraps the DeepSeek daily budget: the first refused call of
// a UTC day alerts the administrators.
type capAlertBudget struct {
	inner  deepseek.Budget
	alerts *AdminAlerter
	capUSD float64
	now    func() time.Time
}

// WithCapAlert returns b with a «daily cap reached» alert (b itself when
// there is nobody to alert). b must be non-nil.
func WithCapAlert(b deepseek.Budget, alerts *AdminAlerter, capUSD float64) deepseek.Budget {
	if alerts == nil {
		return b
	}
	return &capAlertBudget{inner: b, alerts: alerts, capUSD: capUSD, now: time.Now}
}

func (c *capAlertBudget) Reserve(ctx context.Context, amountUSD float64) error {
	err := c.inner.Reserve(ctx, amountUSD)
	if errors.Is(err, deepseek.ErrBudgetExceeded) {
		day := c.now().UTC().Format("2006-01-02")
		c.alerts.AlertAsync(ctx, "dscap:"+day, alertCapEvery, fmt.Sprintf(
			"⚠️ DeepSeek: дневной лимит $%.2f исчерпан (%s UTC).\n"+
				"Платный запасной провайдер выключен до 00:00 UTC. Генерация продолжает работать только на бесплатном Groq; "+
				"задачи, которым нужен DeepSeek, отложены до завтра (попытки не сгорают).", c.capUSD, day))
	}
	return err
}

func (c *capAlertBudget) Settle(ctx context.Context, reservedUSD, actualUSD float64) {
	c.inner.Settle(ctx, reservedUSD, actualUSD)
}

// ---------------------------------------------------------------------------
// Generation job alerts
// ---------------------------------------------------------------------------

// WithAlerts wires the administrator alerts of the generation worker.
func (g *GeneratorService) WithAlerts(a *AdminAlerter) *GeneratorService {
	g.alerts = a
	return g
}

// genFailStreak counts consecutive failed job runs of this process.
type genFailStreak struct {
	mu   sync.Mutex
	n    int
	last string
}

func (s *genFailStreak) fail(desc string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.last = desc
	return s.n
}

func (s *genFailStreak) ok() {
	s.mu.Lock()
	s.n = 0
	s.mu.Unlock()
}

// jobDesc names a job for an alert.
func jobDesc(job *models.GenerationJob) string {
	switch job.Kind {
	case models.TestKindChain:
		return fmt.Sprintf("цепочка: предмет %d, Тест %d", job.SubjectID, job.TestNumber)
	case models.TestKindPersonal:
		return fmt.Sprintf("слабые темы: предмет %d, пользователь %d", job.SubjectID, job.OwnerUserID)
	case models.JobKindTopicBatch:
		return fmt.Sprintf("банк вопросов: предмет %d, тема %q", job.SubjectID, job.TopicKey)
	}
	return fmt.Sprintf("%s: предмет %d", job.Kind, job.SubjectID)
}

// jobSlotKey identifies what a job produces (a revived/re-queued job for
// the same slot shares the key, so it does not alert again).
func jobSlotKey(job *models.GenerationJob) string {
	switch job.Kind {
	case models.TestKindChain:
		return fmt.Sprintf("chain:%d:%d", job.SubjectID, job.TestNumber)
	case models.TestKindPersonal:
		return fmt.Sprintf("personal:%d:%d", job.SubjectID, job.OwnerUserID)
	case models.JobKindTopicBatch:
		return fmt.Sprintf("topic:%d:%s", job.SubjectID, job.TopicKey)
	}
	return fmt.Sprintf("job:%d", job.ID)
}

// noteJobFailure records a failed job run and alerts the administrators
// when the job is now failed for good (attempts exhausted) or when job runs
// keep failing in a row.
func (g *GeneratorService) noteJobFailure(ctx context.Context, job *models.GenerationJob, runErr error) {
	n := g.failStreak.fail(jobDesc(job))
	if g.alerts == nil {
		return
	}
	// Final failures of chain / personal tests (a student is left without
	// the test). Bank topic batches are background fillers — a failing
	// series of them shows up in the streak alert instead of one message
	// per topic.
	if job.Attempts >= maxJobAttempts && job.Kind != models.JobKindTopicBatch {
		g.alerts.AlertAsync(ctx, "genfail:"+jobSlotKey(job), alertJobFailedEvery, fmt.Sprintf(
			"❌ Генерация не удалась окончательно (%s), попыток: %d.\nОшибка: %s",
			jobDesc(job), job.Attempts, shortErr(runErr)))
	}
	if n >= alertStreakAfter {
		g.alerts.AlertAsync(ctx, "genfail-streak", alertStreakEvery, fmt.Sprintf(
			"⚠️ Генерация тестов падает %d раз(а) подряд. Последняя: %s.\nОшибка: %s\n"+
				"Проверь логи (ai[...], groq:, deepseek:) и квоты Groq/DeepSeek.",
			n, jobDesc(job), shortErr(runErr)))
	}
}

// noteJobSuccess ends a failure streak.
func (g *GeneratorService) noteJobSuccess() { g.failStreak.ok() }

// noteStuckParked alerts about running jobs the reaper parked as failed
// (the worker died on them maxJobAttempts times — deploy loop, OOM, panic).
func (g *GeneratorService) noteStuckParked(ctx context.Context, n int64) {
	if g.alerts == nil || n <= 0 {
		return
	}
	g.alerts.AlertAsync(ctx, "genstuck", alertStuckEvery, fmt.Sprintf(
		"❌ %d задач(и) генерации зависли и сняты после %d попыток (процесс падал/перезапускался во время генерации). "+
			"Проверь логи и память сервера.", n, maxJobAttempts))
}

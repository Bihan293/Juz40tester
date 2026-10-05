// JUZ40 Tester — Telegram bot for ENT preparation.
//
// Entry point: loads config, connects to PostgreSQL, runs migrations,
// seeds the starter subject, registers the webhook and serves HTTP:
//
//	GET  /health
//	GET  /ping   (hidden keep-alive endpoint, not referenced in the bot UI)
//	POST /telegram/webhook
//
// A background pinger periodically hits WEBHOOK_URL/ping so the service
// does not fall asleep on free hosting tiers (Render).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Bihan293/Juz40tester/internal/bot"
	"github.com/Bihan293/Juz40tester/internal/config"
	"github.com/Bihan293/Juz40tester/internal/database"
	"github.com/Bihan293/Juz40tester/internal/deepseek"
	"github.com/Bihan293/Juz40tester/internal/groq"
	"github.com/Bihan293/Juz40tester/internal/handlers"
	"github.com/Bihan293/Juz40tester/internal/ratelimit"
	"github.com/Bihan293/Juz40tester/internal/repositories"
	"github.com/Bihan293/Juz40tester/internal/services"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	// Bounded pool (audit #20): the pgx default is max(4, NumCPU); together
	// with the bounded update concurrency below this keeps the instance
	// inside the Neon connection cap.
	pool, err := database.ConnectPoolOpts(ctx, cfg.DatabaseURL, database.PoolOptions{
		MaxConns:        int32(cfg.DBMaxConns),
		MinConns:        int32(cfg.DBMinConns),
		MaxConnLifetime: cfg.DBConnMaxLifetime,
		MaxConnIdleTime: cfg.DBConnMaxIdleTime,
	})
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	// Migrations are serialised by a SESSION-level advisory lock, which is
	// unreliable behind a transaction-mode pooler (audit #21): they run over
	// a separate DIRECT connection (MIGRATION_DATABASE_URL, or DATABASE_URL
	// with the Neon "-pooler" host rewritten to the direct endpoint).
	migURL := cfg.MigrationDatabaseURL
	if migURL == "" {
		migURL = database.DirectURL(cfg.DatabaseURL)
	}
	if err := database.MigrateURL(ctx, migURL); err != nil {
		log.Fatalf("migrations: %v", err)
	}
	log.Println("migrations applied")

	// Wire dependencies.
	userRepo := repositories.NewUserRepository(pool)
	subjectRepo := repositories.NewSubjectRepository(pool)
	repositories.SetChainCacheTTL(cfg.ChainCacheTTL)
	attemptRepo := repositories.NewAttemptRepository(pool)
	stateRepo := repositories.NewStateRepository(pool)
	genRepo := repositories.NewGenerationRepository(pool).WithLockURL(migURL)
	translationRepo := repositories.NewTranslationRepository(pool)

	// DeepSeek AI test generation. Optional: without DEEPSEEK_API_KEY the bot
	// still works with the seeded tests, AI generation is simply disabled.
	// R-9: global daily DeepSeek spending cap, kept in PostgreSQL
	// (ai_spend_daily) and fed by the client's per-call cost estimate.
	budget := services.NewDailyBudget(repositories.NewSpendRepository(pool), cfg.DeepSeekDailyCapUSD)
	var ds *deepseek.Client
	if cfg.DeepSeekAPIKey != "" {
		ds = deepseek.New(cfg.DeepSeekAPIKey, cfg.DeepSeekModel, cfg.DeepSeekReasonerModel, cfg.DeepSeekBaseURL)
		if budget != nil {
			ds.WithBudget(budget, cfg.IsOffPeak)
			log.Printf("deepseek: daily spending cap $%.2f (UTC day)", cfg.DeepSeekDailyCapUSD)
		} else {
			log.Println("deepseek: DEEPSEEK_DAILY_CAP_USD=0 — NO daily spending cap")
		}
		offPeak := "official DeepSeek schedule (peak 01-04 & 06-10 UTC, Mon-Fri)"
		if cfg.OffPeakCustom {
			offPeak = fmt.Sprintf("custom window %02d:00-%02d:00 server-local", cfg.OffPeakStartHour, cfg.OffPeakEndHour)
		}
		log.Printf("deepseek: configured (thinking=%s, fallback=%s, off-peak=%s)",
			ds.ReasonerModel(), ds.Model(), offPeak)
	} else {
		log.Println("deepseek: DEEPSEEK_API_KEY not set — no paid fallback (Groq only, if configured)")
	}
	// Groq free tier: primary provider for translation (Qwen 3.8 27B) and
	// generation (GPT-OSS 120B). Every request is kept inside the published
	// free-tier quota by the client's local limiter (RPM/RPD/TPM/TPD); when
	// the quota runs out the work falls through to paid DeepSeek, so the bot
	// never breaks. Optional: without GROQ_API_KEY nothing changes.
	var gq *groq.Client
	if cfg.GroqAPIKey != "" {
		gq = groq.New(cfg.GroqAPIKey, cfg.GroqBaseURL)
		for _, m := range []string{groq.ModelQwen27B, groq.ModelGPTOSS120B} {
			l := groq.LimitsFor(m)
			log.Printf("groq: %s enabled (limits: %d RPM, %d RPD, %d TPM, %d TPD, max request %d tok)",
				m, l.RPM, l.RPD, l.TPM, l.TPD, l.MaxRequestTokens())
		}
	} else {
		log.Println("groq: GROQ_API_KEY not set — all AI work goes to DeepSeek (paid)")
	}
	genSvc := services.NewGeneratorService(ds, cfg, genRepo, subjectRepo, stateRepo).WithGroq(gq).WithBudget(budget)
	// Kazakh test translations: reuse the same DeepSeek client (flash, low
	// effort). A question is translated once, cached in the DB and shared by
	// every user — no per-user API calls. The generator gets the translator
	// too: when a personal weak-topics test is CLONED for another user with
	// the same weakness profile (fresh question rows, new ids), the cached
	// Kazakh translations are carried over to the clone — otherwise every
	// Kazakh-speaking user of a clone would pay for translating the very
	// same text again.
	translatorSvc := services.NewTranslatorService(ds, translationRepo).WithGroq(gq)
	genSvc.WithTranslator(translatorSvc)
	quiz := services.NewQuizService(subjectRepo, attemptRepo, stateRepo, genRepo, genSvc, userRepo).
		WithTranslator(translatorSvc)
	tg := bot.NewClient(cfg.BotToken).WithMaxRPS(cfg.TGMaxRPS)
	h := handlers.New(tg, userRepo, quiz).WithActionLimiter(ratelimit.New(cfg.UserActionInterval))
	// R-7: the worker delivers a finished generation to the waiting users
	// at once (shared watcher per key) instead of waiting for their poll.
	genSvc.WithJobFinishedHook(h.NotifyJobFinished)

	// Background worker: processes the AI test-generation queue (thinking
	// model with adaptive effort, off-peak deferral, cost logging). No-op
	// without an API key.
	// On shutdown workerCtx is cancelled: a running job is handed back to
	// the queue (pending, attempts unchanged) and the background loops are
	// awaited (bgWG) before the pool is closed.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	var bgWG sync.WaitGroup
	bgWG.Add(5)
	// R-5b: one dedicated DIRECT connection (same URL as the migrations;
	// LISTEN does not work behind a transaction-mode pooler) listens on the
	// queue channels and wakes the workers the moment a job is enqueued by
	// any instance. Workers fall back to a 3-minute poll.
	go func() {
		defer bgWG.Done()
		database.Listen(workerCtx, migURL, []string{repositories.ChannelGenJobs, repositories.ChannelTrJobs}, func(ch string) {
			switch ch {
			case repositories.ChannelGenJobs:
				genSvc.Wake()
			case repositories.ChannelTrJobs:
				translatorSvc.Wake()
			}
		})
	}()
	go func() { defer bgWG.Done(); genSvc.RunWorker(workerCtx) }()
	// R-4: background Kazakh translation queue (translation_jobs). Update
	// handlers only enqueue a job and answer «⏳ Перевод готовится…»; the
	// worker translates and notifies the waiting users. No-op when no AI
	// provider is configured.
	go func() { defer bgWG.Done(); translatorSvc.RunWorker(workerCtx) }()
	// Stale-attempt reaper: attempts left unfinished for staleAttemptAge
	// are closed as abandoned (they would otherwise keep their questions
	// "busy" for the quality sweep forever).
	go func() { defer bgWG.Done(); reapStaleAttempts(workerCtx, attemptRepo) }()
	// R-8a: daily batched cleanup of old attempt rows and finished jobs.
	cleanupRepo := repositories.NewCleanupRepository(pool)
	go func() {
		defer bgWG.Done()
		runCleanup(workerCtx, cleanupRepo, cleanupSettings{
			attemptAge:         time.Duration(cfg.CleanupAttemptDays) * 24 * time.Hour,
			jobAge:             time.Duration(cfg.CleanupJobDays) * 24 * time.Hour,
			emptyAge:           emptyAbandonedAge,
			templateAge:        time.Duration(cfg.TemplateTTLDays) * 24 * time.Hour,
			trJobAge:           time.Duration(cfg.TranslationJobTTLDays) * 24 * time.Hour,
			finishedAttemptAge: time.Duration(cfg.AttemptTTLDays) * 24 * time.Hour,
			firstDelay:         10 * time.Minute,
			interval:           24 * time.Hour,
			pause:              time.Second,
		})
	}()

	// Bootstrap the AI chain: queue the generation of Тест 1 right away for
	// EVERY subject whose chain is still empty — otherwise a fresh database
	// (or a subject left without tests by an earlier broken generator) shows
	// an empty grid and ⏳ forever. Subjects are managed in the database
	// (INSERT INTO subjects ...); there is no code-level seed anymore. No-op
	// when generation is disabled.
	warnIfNoSubjects(ctx, subjectRepo)
	if genSvc.Enabled() {
		subjects, serr := subjectRepo.List(ctx)
		if serr != nil {
			log.Printf("chain bootstrap: %v", serr)
		}
		for _, subj := range subjects {
			chain, cerr := subjectRepo.ListChainTests(ctx, subj.ID)
			if cerr != nil {
				log.Printf("chain bootstrap (%s): %v", subj.Name, cerr)
				continue
			}
			if len(chain) > 0 {
				continue // this subject already has tests
			}
			// Bootstrap jobs must run NOW, not be deferred to off-peak —
			// otherwise a fresh deploy would have no playable tests at all.
			if _, cerr := genRepo.EnqueueChainJobNow(ctx, subj.ID, 1); cerr != nil {
				log.Printf("chain bootstrap (%s): %v", subj.Name, cerr)
			} else {
				log.Printf("chain bootstrap: queued generation of Тест 1 for subject %d (%s)", subj.ID, subj.Name)
			}
		}
	}

	webhookEndpoint := cfg.WebhookURL + "/telegram/webhook"
	if cfg.WebhookSecret == "" {
		// Only reachable with APP_ENV=development (config.Load refuses a
		// production start without the secret).
		log.Println("webhook: WEBHOOK_SECRET not set — incoming updates are NOT authenticated (development mode)")
	}

	mux := http.NewServeMux()
	// R-5c: the DB ping result is cached for HEALTH_CACHE_SEC (default 45s).
	mux.Handle("GET /health", newHealthCache(pool.Ping, cfg.HealthCacheTTL))
	// Hidden keep-alive endpoint: used only by external uptime monitors and
	// by the internal self-pinger below. Deliberately absent from the bot UI.
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"pong"}`))
	})

	// Self-ping: free hosting tiers (Render) suspend an idle service, which
	// also freezes the background generation worker. A lightweight loop pings
	// our own /ping endpoint to keep the instance awake. Silent on purpose.
	go selfPing(workerCtx, cfg.WebhookURL+"/ping")

	// Updates: bounded concurrency + tracked for graceful shutdown.
	// R-3: a fixed pool of maxConcurrentUpdates workers + a bounded queue of
	// defaultUpdateQueueSize; overflow is answered with 503 (Telegram
	// re-delivers later).
	updates := newUpdateDispatcher(cfg.WebhookSecret, updateWorkers(cfg.DBMaxConns), defaultUpdateQueueSize, cfg.UpdateTimeout, h.HandleUpdate)
	mux.Handle("POST /telegram/webhook", updates)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	// Register the webhook AFTER the server is up, retrying temporary
	// Telegram/network failures instead of log.Fatalf (audit #22: a short
	// Telegram outage used to crash-loop the service).
	go func() {
		err := setWebhookWithRetry(workerCtx, func(c context.Context) error {
			return tg.SetWebhook(c, webhookEndpoint, cfg.WebhookSecret)
		}, 2*time.Second, time.Minute)
		if err == nil {
			log.Printf("webhook set: %s", webhookEndpoint)
		}
	}()

	// One-off: build the per-topic statistics (source of weak topics) from
	// the answer history stored before they existed. Runs in the background
	// after the HTTP server is up so a large history never delays startup /
	// the Render health check (audit #24). No-op once done.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		if err := genRepo.BackfillTopicStats(workerCtx); err != nil {
			log.Printf("topic stats backfill: %v", err)
		}
		// B1: seed the topic catalog from existing questions and map
		// questions.topic_key (one-off, no-op once done).
		if err := genRepo.BackfillTopicCatalog(workerCtx); err != nil {
			log.Printf("topic catalog backfill: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// 1. Stop the worker first: a running generation is interrupted and
	//    handed back to 'pending' (attempts unchanged) right away, so the
	//    new instance can pick it up without waiting for the stuck-job reaper.
	stopWorker()
	// 2. Stop accepting HTTP requests (in-flight webhook calls finish).
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: http: %v", err)
	}
	// 3. Wait for acknowledged updates that are still being processed.
	if err := updates.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: some updates did not finish in time: %v", err)
	}
	// 3a. Stop the generation watchers (their notes keep the ⏳ text; the
	//     user taps the ⏳ button again after the restart).
	h.Close(2 * time.Second)
	// 3b. Flush Telegram requests deferred by a 429 (best effort).
	if err := tg.Close(shutdownCtx); err != nil {
		log.Printf("shutdown: %d deferred Telegram request(s) dropped: %v", tg.PendingDeferred(), err)
	}
	// 4. Wait for the background loops before the DB pool is closed.
	waitTimeout(&bgWG, 5*time.Second)
	log.Println("shutdown complete")
}

// maxConcurrentUpdates bounds simultaneously processed Telegram updates.
const maxConcurrentUpdates = 32

// dbConnsReserved is the part of the pgx pool kept for the generation /
// translation workers, reapers and cleanup, so update handlers can never
// take every pooled connection.
const dbConnsReserved = 8

// updateWorkers caps maxConcurrentUpdates at DB_MAX_CONNS - 8 (at least 1):
// more concurrent updates than free pool connections only queue on the
// pool and starve the background workers.
func updateWorkers(dbMaxConns int) int {
	n := maxConcurrentUpdates
	if limit := dbMaxConns - dbConnsReserved; n > limit {
		n = max(limit, 1)
		log.Printf("WARNING: concurrent updates capped at %d (DB_MAX_CONNS=%d - %d reserved), was %d",
			n, dbMaxConns, dbConnsReserved, maxConcurrentUpdates)
	}
	return n
}

// shutdownTimeout bounds the graceful wait for HTTP + in-flight updates
// (Render sends SIGKILL ~30 s after SIGTERM).
const shutdownTimeout = 20 * time.Second

// waitTimeout waits for wg at most d.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		log.Printf("shutdown: background tasks did not stop within %s", d)
	}
}

// staleAttemptAge: an in-progress attempt untouched this long is abandoned.
const staleAttemptAge = 30 * 24 * time.Hour

// reapStaleAttempts closes long-forgotten in-progress attempts once at
// startup and then every 6 hours.
func reapStaleAttempts(ctx context.Context, attempts *repositories.AttemptRepository) {
	run := func() {
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		n, err := attempts.AbandonStaleAttempts(rctx, staleAttemptAge)
		if err != nil {
			log.Printf("stale attempts reaper: %v", err)
			return
		}
		if n > 0 {
			log.Printf("stale attempts reaper: %d attempt(s) marked abandoned", n)
		}
	}
	run()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// selfPing hits the given URL every 5 minutes so the hosting platform keeps
// the instance alive (Render free tier spins services down after ~15 minutes
// of inactivity). Failures are ignored: this is best-effort keep-alive.
func selfPing(ctx context.Context, url string) {
	if url == "" || url == "/ping" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// warnIfNoSubjects logs a clear hint on a fresh database: subjects are not
// seeded by code (migration 000002 removed the old seed) and must be added
// with SQL, otherwise «📚 Предметы» stays empty.
func warnIfNoSubjects(ctx context.Context, subjects *repositories.SubjectRepository) {
	list, err := subjects.List(ctx)
	if err != nil {
		log.Printf("subjects check: %v", err)
		return
	}
	if len(list) == 0 {
		log.Println("subjects: the database has NO subjects — there is no built-in seed. " +
			"Add them with SQL, e.g. INSERT INTO subjects (name, position) VALUES ('Биология', 1); see README «База данных»")
	}
}

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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bihan293/Juz40-test2/internal/bot"
	"github.com/Bihan293/Juz40-test2/internal/config"
	"github.com/Bihan293/Juz40-test2/internal/database"
	"github.com/Bihan293/Juz40-test2/internal/deepseek"
	"github.com/Bihan293/Juz40-test2/internal/groq"
	"github.com/Bihan293/Juz40-test2/internal/handlers"
	"github.com/Bihan293/Juz40-test2/internal/repositories"
	"github.com/Bihan293/Juz40-test2/internal/services"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx := context.Background()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrations: %v", err)
	}
	log.Println("migrations applied")

	// Wire dependencies.
	userRepo := repositories.NewUserRepository(pool)
	subjectRepo := repositories.NewSubjectRepository(pool)
	attemptRepo := repositories.NewAttemptRepository(pool)
	stateRepo := repositories.NewStateRepository(pool)
	genRepo := repositories.NewGenerationRepository(pool)
	translationRepo := repositories.NewTranslationRepository(pool)

	// DeepSeek AI test generation. Optional: without DEEPSEEK_API_KEY the bot
	// still works with the seeded tests, AI generation is simply disabled.
	var ds *deepseek.Client
	if cfg.DeepSeekAPIKey != "" {
		ds = deepseek.New(cfg.DeepSeekAPIKey, cfg.DeepSeekModel, cfg.DeepSeekReasonerModel, cfg.DeepSeekBaseURL)
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
	genSvc := services.NewGeneratorService(ds, cfg, genRepo, subjectRepo, stateRepo).WithGroq(gq)
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
	tg := bot.NewClient(cfg.BotToken)
	h := handlers.New(tg, userRepo, quiz)

	// Background worker: processes the AI test-generation queue (thinking
	// model with adaptive effort, off-peak deferral, cost logging). No-op
	// without an API key.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go genSvc.RunWorker(workerCtx)

	// Bootstrap the AI chain: queue the generation of Тест 1 right away for
	// EVERY subject whose chain is still empty — otherwise a fresh database
	// (or a subject left without tests by an earlier broken generator) shows
	// an empty grid and ⏳ forever. Subjects are managed in the database
	// (INSERT INTO subjects ...); there is no code-level seed anymore. No-op
	// when generation is disabled.
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

	// Register webhook (WEBHOOK_URL is the public base URL of this service).
	webhookEndpoint := cfg.WebhookURL + "/telegram/webhook"
	if err := tg.SetWebhook(ctx, webhookEndpoint); err != nil {
		log.Fatalf("set webhook: %v", err)
	}
	log.Printf("webhook set: %s", webhookEndpoint)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, `{"status":"unhealthy"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Hidden keep-alive endpoint: used only by external uptime monitors and
	// by the internal self-pinger below. Deliberately absent from the bot UI.
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"pong"}`))
	})

	// Self-ping: free hosting tiers (Render) suspend an idle service, which
	// also freezes the background generation worker. A lightweight loop pings
	// our own /ping endpoint to keep the instance awake. Silent on purpose.
	go selfPing(context.Background(), cfg.WebhookURL+"/ping")
	mux.HandleFunc("POST /telegram/webhook", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var upd bot.Update
		if err := json.Unmarshal(body, &upd); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Respond to Telegram immediately, process asynchronously.
		w.WriteHeader(http.StatusOK)
		// Detach from the request context so processing survives the response.
		go h.HandleUpdate(context.Background(), &upd)
	})

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

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
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

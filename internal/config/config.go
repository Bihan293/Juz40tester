// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the bot.
type Config struct {
	BotToken    string
	DatabaseURL string
	WebhookURL  string
	Port        string
	// WebhookSecret (WEBHOOK_SECRET): Telegram echoes it in the
	// X-Telegram-Bot-Api-Secret-Token header; requests without it are
	// rejected. Without it anyone who knows the URL could forge updates, so
	// it is REQUIRED in production (see Production / Load).
	WebhookSecret string

	// Production is true unless APP_ENV is one of development/dev/local/test.
	// In production WEBHOOK_SECRET is mandatory.
	Production bool

	// MigrationDatabaseURL (MIGRATION_DATABASE_URL, optional): a DIRECT
	// (non-pooler) connection string used only for migrations. Migrations are
	// serialised by a SESSION-level pg_advisory_lock; behind a transaction-
	// mode pooler (Neon "-pooler" host / PgBouncer) the lock and the unlock
	// may land on different server connections. Defaults to DATABASE_URL with
	// a Neon "-pooler" host rewritten to the direct host.
	MigrationDatabaseURL string

	// DBMaxConns (DB_MAX_CONNS, default 20): upper bound of the pgx pool.
	// The LISTEN listener, the quality-sweep advisory lock and the migration
	// connection are DEDICATED connections outside the pool (+3 at most).
	DBMaxConns int
	// UpdateTimeout (UPDATE_TIMEOUT_SEC, default 60): upper bound of the
	// processing of one Telegram update. Long work (generation, translation)
	// runs in the background, so a handler never needs more.
	UpdateTimeout time.Duration
	// DBMinConns (DB_MIN_CONNS, default 5): connections kept open when idle
	// (pgxpool has no "max idle": idle conns above this are closed after
	// DBConnMaxIdleTime).
	DBMinConns int
	// DBConnMaxLifetime (DB_CONN_MAX_LIFETIME_SEC, default 30 min).
	DBConnMaxLifetime time.Duration
	// DBConnMaxIdleTime (DB_CONN_MAX_IDLE_SEC, default 5 min).
	DBConnMaxIdleTime time.Duration
	// ChainCacheTTL (CHAIN_CACHE_TTL_SEC, default 45, 0 = off): TTL of the
	// in-memory chain-structure cache (R-10a).
	ChainCacheTTL time.Duration

	// GenWorkers (GEN_WORKERS, default 4, max 16): number of concurrent
	// generation-queue workers. Each claims jobs with FOR UPDATE SKIP LOCKED,
	// so a job never runs twice. Kept small on purpose: the Groq free tier
	// serves ~1 test per minute per model, extra workers mostly spill over
	// to the paid DeepSeek fallback.
	GenWorkers int
	// GenDeepSeekConcurrency (GEN_DEEPSEEK_CONCURRENCY, default 2): max
	// number of paid DeepSeek generation calls in flight at once across all
	// workers — bounds the cost burst when Groq is saturated.
	GenDeepSeekConcurrency int

	// DeepSeek API settings for AI test generation.
	DeepSeekAPIKey        string
	DeepSeekModel         string // non-thinking fallback model (DEEPSEEK_MODEL), default deepseek-v4-pro
	DeepSeekReasonerModel string // primary thinking model (DEEPSEEK_REASONER_MODEL), default deepseek-flash
	DeepSeekBaseURL       string // overridable via DEEPSEEK_BASE_URL

	// Groq API (free tier) — primary provider for Kazakh translation
	// (qwen/qwen3.8-27b) and test generation (openai/gpt-oss-120b).
	// DeepSeek becomes the paid fallback. Optional: without GROQ_API_KEY the
	// bot behaves exactly as before (DeepSeek only). Quota: docs/GROQ_LIMITS.md.
	GroqAPIKey  string
	GroqBaseURL string // overridable via GROQ_BASE_URL

	// Generation scheduling: generation of LOCKED chain tests is deferred to
	// off-peak hours (half price on DeepSeek). Tests the user can already
	// open, and personal weak-topics tests, are URGENT and ignore this.
	//
	// The official DeepSeek pricing schedule (2026): PEAK = 01:00–04:00 and
	// 06:00–10:00 UTC, Monday–Friday; everything else (nights, evenings,
	// weekends, Chinese public holidays) is OFF-PEAK at half price. This is
	// the default. GEN_OFFPEAK_START_HOUR / GEN_OFFPEAK_END_HOUR (server-local
	// hours) override it with a custom daily window — used only when the API
	// is proxied through a provider with a different discount schedule.
	// R-9 abuse limits / spending cap.
	//
	// UserActionInterval (USER_ACTION_INTERVAL_MS, default 300ms, 0 = off):
	// minimum gap between two actions (taps / messages) of one user; faster
	// taps are acknowledged and ignored. In-memory, per instance.
	UserActionInterval time.Duration
	// PersonalGenPerUserDay (PERSONAL_GEN_PER_USER_DAY, default 3, 0 = no
	// limit): how many NEW personal weak-topics generations one user may
	// request per day (clones of an existing test are free and not counted).
	PersonalGenPerUserDay int
	// DeepSeekDailyCapUSD (DEEPSEEK_DAILY_CAP_USD, default 2.0, 0 = no cap):
	// global daily spending cap of paid DeepSeek calls (UTC day). Enforced
	// in PostgreSQL (ai_spend_daily), valid across instances.
	DeepSeekDailyCapUSD float64

	// R-5c idle-load knobs (fewer DB queries so Neon can scale to zero).
	// ReaperInterval (REAPER_INTERVAL_SEC, default 300): how often stuck
	// 'running' generation jobs are returned to the queue.
	// HealthCacheTTL (HEALTH_CACHE_SEC, default 45, 0 = no cache): how long
	// /health reuses the last database ping result.
	ReaperInterval time.Duration
	HealthCacheTTL time.Duration

	// R-8a daily cleanup. CleanupAttemptDays (CLEANUP_ATTEMPT_DAYS, default
	// 45): per-question rows of finished attempts older than this are
	// deleted. CleanupJobDays (CLEANUP_JOB_DAYS, default 14): done/failed
	// generation jobs older than this are deleted.
	CleanupAttemptDays int
	CleanupJobDays     int
	// A3: TemplateTTLDays (TEMPLATE_TTL_DAYS, default 60): ownerless hidden
	// personal-test templates older than this are deleted.
	// TranslationJobTTLDays (TRANSLATION_JOB_TTL_DAYS, default 1): done
	// translation jobs older than this are deleted.
	TemplateTTLDays       int
	TranslationJobTTLDays int

	OffPeakStartHour int  // custom window start (inclusive); -1 = official schedule
	OffPeakEndHour   int  // custom window end (exclusive)
	OffPeakCustom    bool // true when a custom window is configured
}

// Load reads configuration from environment variables and validates that
// all required variables are set. No secrets are hardcoded in the codebase.
func Load() (*Config, error) {
	cfg := &Config{
		BotToken:               os.Getenv("BOT_TOKEN"),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		WebhookURL:             strings.TrimRight(os.Getenv("WEBHOOK_URL"), "/"),
		Port:                   os.Getenv("PORT"),
		WebhookSecret:          strings.TrimSpace(os.Getenv("WEBHOOK_SECRET")),
		DeepSeekAPIKey:         os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekModel:          os.Getenv("DEEPSEEK_MODEL"),
		DeepSeekReasonerModel:  os.Getenv("DEEPSEEK_REASONER_MODEL"),
		DeepSeekBaseURL:        os.Getenv("DEEPSEEK_BASE_URL"),
		GroqAPIKey:             strings.TrimSpace(os.Getenv("GROQ_API_KEY")),
		GroqBaseURL:            strings.TrimSpace(os.Getenv("GROQ_BASE_URL")),
		OffPeakStartHour:       -1,
		OffPeakEndHour:         -1,
		MigrationDatabaseURL:   strings.TrimSpace(os.Getenv("MIGRATION_DATABASE_URL")),
		DBMaxConns:             defaultDBMaxConns,
		UpdateTimeout:          DefaultUpdateTimeout,
		DBMinConns:             DefaultDBMinConns,
		DBConnMaxLifetime:      DefaultDBConnMaxLifetime,
		DBConnMaxIdleTime:      DefaultDBConnMaxIdleTime,
		GenWorkers:             DefaultGenWorkers,
		GenDeepSeekConcurrency: DefaultGenDeepSeekConcurrency,
		UserActionInterval:     DefaultUserActionInterval,
		PersonalGenPerUserDay:  DefaultPersonalGenPerUserDay,
		DeepSeekDailyCapUSD:    DefaultDeepSeekDailyCapUSD,
		ReaperInterval:         DefaultReaperInterval,
		HealthCacheTTL:         DefaultHealthCacheTTL,
		CleanupAttemptDays:     DefaultCleanupAttemptDays,
		TemplateTTLDays:        DefaultTemplateTTLDays,
		TranslationJobTTLDays:  DefaultTranslationJobTTLDays,
		CleanupJobDays:         DefaultCleanupJobDays,
		ChainCacheTTL:          DefaultChainCacheTTL,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV"))) {
	case "development", "dev", "local", "test":
		cfg.Production = false
	default:
		cfg.Production = true
	}
	if n, ok := envIntOpt("DB_MAX_CONNS"); ok && n > 0 {
		cfg.DBMaxConns = n
	}
	if n, ok := envIntOpt("UPDATE_TIMEOUT_SEC"); ok && n > 0 {
		cfg.UpdateTimeout = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("DB_MIN_CONNS"); ok && n >= 0 {
		cfg.DBMinConns = n
	}
	cfg.DBMinConns = min(cfg.DBMinConns, cfg.DBMaxConns)
	if n, ok := envIntOpt("DB_CONN_MAX_LIFETIME_SEC"); ok && n > 0 {
		cfg.DBConnMaxLifetime = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("DB_CONN_MAX_IDLE_SEC"); ok && n > 0 {
		cfg.DBConnMaxIdleTime = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("CHAIN_CACHE_TTL_SEC"); ok && n >= 0 {
		cfg.ChainCacheTTL = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("GEN_WORKERS"); ok && n > 0 {
		cfg.GenWorkers = min(n, MaxGenWorkers)
	}
	if n, ok := envIntOpt("GEN_DEEPSEEK_CONCURRENCY"); ok && n > 0 {
		cfg.GenDeepSeekConcurrency = n
	}
	if n, ok := envIntOpt("USER_ACTION_INTERVAL_MS"); ok && n >= 0 {
		cfg.UserActionInterval = time.Duration(n) * time.Millisecond
	}
	if n, ok := envIntOpt("REAPER_INTERVAL_SEC"); ok && n > 0 {
		cfg.ReaperInterval = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("HEALTH_CACHE_SEC"); ok && n >= 0 {
		cfg.HealthCacheTTL = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("CLEANUP_ATTEMPT_DAYS"); ok && n > 0 {
		cfg.CleanupAttemptDays = n
	}
	if n, ok := envIntOpt("TEMPLATE_TTL_DAYS"); ok && n > 0 {
		cfg.TemplateTTLDays = n
	}
	if n, ok := envIntOpt("TRANSLATION_JOB_TTL_DAYS"); ok && n > 0 {
		cfg.TranslationJobTTLDays = n
	}
	if n, ok := envIntOpt("CLEANUP_JOB_DAYS"); ok && n > 0 {
		cfg.CleanupJobDays = n
	}
	if n, ok := envIntOpt("PERSONAL_GEN_PER_USER_DAY"); ok && n >= 0 {
		cfg.PersonalGenPerUserDay = n
	}
	if v := strings.TrimSpace(os.Getenv("DEEPSEEK_DAILY_CAP_USD")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			cfg.DeepSeekDailyCapUSD = f
		}
	}
	if start, ok := envIntOpt("GEN_OFFPEAK_START_HOUR"); ok {
		if end, ok2 := envIntOpt("GEN_OFFPEAK_END_HOUR"); ok2 {
			cfg.OffPeakStartHour = start
			cfg.OffPeakEndHour = end
			cfg.OffPeakCustom = true
		}
	}
	if cfg.Port == "" {
		cfg.Port = "8080"
	}
	if cfg.DeepSeekModel == "" {
		cfg.DeepSeekModel = "deepseek-v4-pro"
	}
	if cfg.DeepSeekReasonerModel == "" {
		cfg.DeepSeekReasonerModel = "deepseek-flash"
	}
	if cfg.DeepSeekBaseURL == "" {
		cfg.DeepSeekBaseURL = "https://api.deepseek.com"
	}
	if cfg.GroqBaseURL == "" {
		cfg.GroqBaseURL = "https://api.groq.com/openai/v1"
	}

	var missing []string
	if cfg.BotToken == "" {
		missing = append(missing, "BOT_TOKEN")
	}
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if cfg.WebhookURL == "" {
		missing = append(missing, "WEBHOOK_URL")
	}
	// A production webhook without a secret accepts forged updates from
	// anyone who knows the URL — refuse to start instead of only warning.
	if cfg.Production && cfg.WebhookSecret == "" {
		missing = append(missing, "WEBHOOK_SECRET (required in production; set APP_ENV=development to run without it)")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %v", missing)
	}
	return cfg, nil
}

// defaultDBMaxConns bounds the pgx pool (the pgx default is max(4, NumCPU),
// too small for concurrent updates + worker + reapers on a 1-CPU instance,
// and unbounded concurrency would exhaust a Neon free-tier connection cap).
const defaultDBMaxConns = 20

// DefaultUpdateTimeout bounds the processing of one Telegram update.
const DefaultUpdateTimeout = 60 * time.Second

const (
	// DefaultGenWorkers is the default size of the generation worker pool.
	DefaultGenWorkers = 4
	// MaxGenWorkers caps GEN_WORKERS: more workers than this only fight
	// over the Groq quota and the DB pool and multiply DeepSeek spend.
	MaxGenWorkers = 16
	// DefaultGenDeepSeekConcurrency bounds parallel paid generations.
	DefaultGenDeepSeekConcurrency = 2

	// DefaultUserActionInterval: at most one action per user per 300ms.
	DefaultUserActionInterval = 300 * time.Millisecond
	// DefaultPersonalGenPerUserDay: new personal generations per user/day.
	DefaultPersonalGenPerUserDay = 3
	// DefaultDeepSeekDailyCapUSD: global daily DeepSeek spend cap (USD).
	DefaultDeepSeekDailyCapUSD = 2.0

	// DefaultReaperInterval: stuck generation jobs are reaped every 5 min.
	DefaultReaperInterval = 5 * time.Minute
	// DefaultHealthCacheTTL: /health pings the database at most every 45s.
	DefaultHealthCacheTTL = 45 * time.Second
	// R-8a cleanup retention defaults (days).
	DefaultCleanupAttemptDays = 45
	DefaultCleanupJobDays     = 14
	// A3 retention defaults (days).
	DefaultTemplateTTLDays       = 60
	DefaultTranslationJobTTLDays = 1
	// R-10a pool defaults.
	DefaultDBMinConns        = 5
	DefaultDBConnMaxLifetime = 30 * time.Minute
	DefaultDBConnMaxIdleTime = 5 * time.Minute
	// DefaultChainCacheTTL: in-memory cache of a subject's chain structure.
	DefaultChainCacheTTL = 45 * time.Second
)

// envIntOpt reads an integer env var; ok is false when it is unset or
// unparsable (callers need to distinguish "unset" from an explicit value).
func envIntOpt(key string) (n int, ok bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// IsOffPeak reports whether the given moment falls into the off-peak
// (half-price) billing window.
//
// With a custom window configured it is a simple hour-of-day check in server
// local time (wrap-around supported). Otherwise the official DeepSeek
// schedule applies, evaluated in UTC: peak is 01:00–04:00 and 06:00–10:00 on
// weekdays; nights, evenings and weekends are off-peak. (Chinese public
// holidays — also off-peak per the docs — are approximated as normal days:
// worst case a job runs an hour later than the cheapest possible moment.)
func (c *Config) IsOffPeak(now time.Time) bool {
	if c.OffPeakCustom {
		hour := now.Hour()
		start, end := c.OffPeakStartHour, c.OffPeakEndHour
		if start == end {
			return true // window covers the whole day
		}
		if start < end {
			return hour >= start && hour < end
		}
		return hour >= start || hour < end
	}
	utc := now.UTC()
	switch utc.Weekday() {
	case time.Saturday, time.Sunday:
		return true // weekends are fully off-peak
	}
	h := utc.Hour()
	// Peak windows: [01:00, 04:00) and [06:00, 10:00) UTC on weekdays.
	if (h >= 1 && h < 4) || (h >= 6 && h < 10) {
		return false
	}
	return true
}

// NextOffPeakStart returns the start of the next off-peak window after now
// (or now itself when already off-peak). Used to defer paid generations to
// the cheaper billing window. The hourly scan is exact for both the
// official weekday schedule and custom wrap-around windows.
func (c *Config) NextOffPeakStart(now time.Time) time.Time {
	if c.IsOffPeak(now) {
		return now
	}
	t := now.Truncate(time.Hour).Add(time.Hour)
	for i := 0; i < 24*8; i++ { // a full week of hours always contains off-peak
		if c.IsOffPeak(t) {
			return t
		}
		t = t.Add(time.Hour)
	}
	return now // unreachable in practice — never block generation forever
}

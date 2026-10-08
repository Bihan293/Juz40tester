// Package config loads application configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	// Embedded tz database: QUOTA_TZ (Asia/Almaty) must resolve even in a
	// minimal container without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/Bihan293/Juz40tester/internal/billing"
)

// Config holds all runtime configuration for the bot.
type Config struct {
	// Subscriptions: Telegram Stars plans, daily quota, paid weak tests.
	Subscriptions Subscriptions
	// TelegramAPIURL (TELEGRAM_API_URL, default https://api.telegram.org):
	// another Bot API server (a local one). TelegramTestEnv
	// (TELEGRAM_TEST_ENV=1): use the Telegram TEST environment (test
	// accounts and test Stars — for checking payments).
	TelegramAPIURL  string
	TelegramTestEnv bool

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
	// TGMaxRPS (TG_MAX_RPS, default 25): global rate of outgoing Telegram
	// calls that create / change messages (answerCallbackQuery is not
	// limited). Calls wait for their slot instead of hitting 429.
	TGMaxRPS int
	// BroadcastRPS (BROADCAST_RPS, default 2/3 of TG_MAX_RPS, at least 1,
	// never above TG_MAX_RPS): messages per second of an admin broadcast
	// for the whole cluster — the rest of TG_MAX_RPS stays for the
	// interactive traffic.
	BroadcastRPS int
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
	// AttemptTTLDays (ATTEMPT_TTL_DAYS, default 180): finished attempts older
	// than this are deleted, except the latest and the best per (user, test).
	AttemptTTLDays int

	// GenStrategy (GEN_STRATEGY): "batch" (default — the test is assembled
	// from batches of GenBatchSize questions), "full" (one call writes all
	// 20 questions) or "ab" (A/B test: GenABBatchPercent % of jobs batch).
	GenStrategy string
	// GenABBatchPercent (GEN_AB_BATCH_PERCENT, default 50): share of jobs
	// on the "batch" arm when GenStrategy = "ab".
	GenABBatchPercent int
	// GenBatchSize (GEN_BATCH_SIZE, default 5, 2..10): questions per batch call.
	GenBatchSize int
	// GenBatchParallel (GEN_BATCH_PARALLEL, default 2, 1..4): batch calls of
	// one job in flight at once (they alternate between the two free Groq
	// models, which have independent quotas).
	GenBatchParallel int
	// GenMaxActivePersonal (GEN_MAX_ACTIVE_PERSONAL, default 300, 0 = no
	// limit): backpressure — when this many personal AI generations are
	// already queued/running, new ones are refused with a friendly message
	// instead of growing the queue without bound (the free AI quota is the
	// real bottleneck with thousands of users).
	GenMaxActivePersonal int
	// GenTemplateReuse (GEN_TEMPLATE_REUSE, default true): reuse generated
	// tests via the weak-topics fingerprint (clone instead of regeneration).
	GenTemplateReuse bool
	// GenPregenAhead (GEN_PREGEN_AHEAD, default 0 = off): when a user
	// unlocks chain test N, also queue test N+1 as a NON-urgent job that
	// runs in the off-peak window (opt-in: the next test is then written
	// with fewer knowledge marks of test N).
	GenPregenAhead int
	// MetricsToken (METRICS_TOKEN): when set, GET /metrics requires
	// «Authorization: Bearer <token>» (or ?token=).
	MetricsToken string

	// --- Cluster mode -----------------------------------------------------

	// Role (ROLE, default "all"): which part of the bot this process runs.
	//   web    — accepts the Telegram webhook, validates it, puts the update
	//            into the durable queue and answers 200 at once; serves
	//            /health, /ping, /metrics. No update handling, no workers.
	//   worker — no webhook endpoint: takes updates from the queue and
	//            handles them, runs generation / translation workers,
	//            reapers, quality sweep, cleanup (/health, /ping, /metrics
	//            are served too, for the platform's health check).
	//   all    — everything in one process (the historical behaviour).
	Role string
	// QueueBackend (QUEUE_BACKEND): where accepted updates wait for a
	// worker. "memory" (in-process bounded channel — only possible with
	// ROLE=all, the default there), "postgres" (durable table, the default
	// for ROLE=web/worker) or "redis" (needs REDIS_URL).
	QueueBackend string
	// RateLimitBackend (RATELIMIT_BACKEND): how TG_MAX_RPS and the per-user
	// action throttle hold across instances. "memory" (per process; the
	// default with ROLE=all), "postgres" (TG_MAX_RPS is split evenly
	// between the live instances registered in cluster_instances; the
	// default with ROLE=web/worker; the per-user throttle stays per process)
	// or "redis" (one shared token bucket + shared throttle; needs REDIS_URL).
	RateLimitBackend string
	// CacheBackend (CACHE_BACKEND, default "memory"): shared cache for
	// cross-instance data (leaderboard). "redis" needs REDIS_URL.
	CacheBackend string
	// RedisURL (REDIS_URL, optional): redis://[:password@]host:port/db.
	// Empty = Redis is never used; everything runs on PostgreSQL / memory.
	// Even when it is set, Redis is used ONLY by the backends switched to
	// "redis" explicitly.
	RedisURL string
	// RedisPrefix (REDIS_PREFIX, default "juz40"): key prefix (a hash tag,
	// so the multi-key scripts also work on Redis Cluster).
	RedisPrefix string
	// InstanceID (INSTANCE_ID, default hostname-pid): name of this process
	// in cluster_instances, queue locks and logs.
	InstanceID string
	// UpdateWorkers (UPDATE_WORKERS, default derived from DB_MAX_CONNS):
	// updates handled at once by this process.
	UpdateWorkers int
	// UpdateQueueMax (UPDATE_QUEUE_MAX, default 20000, 0 = no limit):
	// backpressure of the durable queue — when this many updates wait, the
	// webhook answers 503 and Telegram re-delivers later.
	UpdateQueueMax int
	// UpdateMaxAttempts (UPDATE_MAX_ATTEMPTS, default 3): a crashing update
	// is retried with backoff and then moved to the dead letters.
	UpdateMaxAttempts int
	// UpdateLease (UPDATE_LEASE_SEC, default 45): a claimed update whose
	// worker stopped heart-beating for this long is returned to the queue.
	UpdateLease time.Duration
	// UpdateDoneTTL (UPDATE_DONE_TTL_HOURS, default 48): handled updates are
	// kept this long (idempotency window: Telegram re-delivers for ≤ 24 h).
	UpdateDoneTTL time.Duration
	// UpdateDeadTTL (UPDATE_DEAD_TTL_DAYS, default 14): dead letters are
	// kept this long for inspection.
	UpdateDeadTTL time.Duration

	OffPeakStartHour int  // custom window start (inclusive); -1 = official schedule
	OffPeakEndHour   int  // custom window end (exclusive)
	OffPeakCustom    bool // true when a custom window is configured
}

// Subscriptions holds the Telegram Stars monetisation settings
// (docs/SUBSCRIPTIONS.md). Every price / limit comes from the environment
// with the owner's defaults — nothing is hard-coded in the handlers.
type Subscriptions struct {
	// Enabled (SUBSCRIPTIONS_ENABLED, default 1): the daily quota, the
	// «⭐ Подписка» screen and the paid weak-topics tests. 0 = the bot
	// behaves exactly as before (no limits, free weak-topics tests).
	Enabled bool
	// Plans: PLAN_FREE_DAILY (1), PLAN_PLUS_PRICE (10) / PLAN_PLUS_DAILY (4),
	// PLAN_PRO_PRICE (40) / PLAN_PRO_DAILY (10), PLAN_PREMIUM_PRICE (50) /
	// PLAN_PREMIUM_DAILY (20). A paid plan with price 0 is switched off.
	Plans []billing.Plan
	// QuotaTZ (QUOTA_TZ, default Asia/Almaty): the daily limit resets at
	// local midnight of this zone.
	QuotaTZ string
	// WeakTestPrice (WEAK_TEST_PRICE_STARS, default 10, 0 = free as before):
	// price of ONE generated weak-topics test.
	WeakTestPrice int
	// Grace (SUB_GRACE_MIN, default 60): a paid plan stays in force this
	// long after its expiry date (renewal charges may arrive a bit late).
	Grace time.Duration
	// AdminIDs (ADMIN_IDS, comma-separated Telegram user ids): may use
	// /grant, /revoke, /subinfo, /refund.
	AdminIDs []int64
	// WeakOrderTimeout (WEAK_ORDER_TIMEOUT_MIN, default 45): a paid order
	// without a test after this long is refunded automatically.
	WeakOrderTimeout time.Duration
	// WeakOrderMaxGen (WEAK_ORDER_MAX_GEN, default 3): generation attempts
	// of one paid order before it is refunded.
	WeakOrderMaxGen int
	// ReconcileInterval (BILLING_RECONCILE_SEC, default 60): how often the
	// worker re-checks paid orders and pending refunds (payments and
	// finished generations also wake it at once).
	ReconcileInterval time.Duration
}

// Catalog builds the validated plan catalog.
func (s Subscriptions) Catalog() (*billing.Catalog, error) { return billing.NewCatalog(s.Plans) }

// IsAdmin reports whether the Telegram user is an administrator.
func (s Subscriptions) IsAdmin(tgUserID int64) bool {
	for _, id := range s.AdminIDs {
		if id == tgUserID {
			return true
		}
	}
	return false
}

// loadSubscriptions reads the SUBSCRIPTIONS_* / PLAN_* settings.
func loadSubscriptions() (Subscriptions, error) {
	s := Subscriptions{
		Enabled:           true,
		QuotaTZ:           "Asia/Almaty",
		WeakTestPrice:     10,
		Grace:             time.Hour,
		WeakOrderTimeout:  45 * time.Minute,
		WeakOrderMaxGen:   3,
		ReconcileInterval: time.Minute,
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SUBSCRIPTIONS_ENABLED"))); v != "" {
		s.Enabled = !(v == "0" || v == "false" || v == "no" || v == "off")
	}
	if v := strings.TrimSpace(os.Getenv("QUOTA_TZ")); v != "" {
		if _, err := time.LoadLocation(v); err != nil {
			return s, fmt.Errorf("QUOTA_TZ: unknown time zone %q", v)
		}
		s.QuotaTZ = v
	}
	if n, ok := envIntOpt("WEAK_TEST_PRICE_STARS"); ok && n >= 0 && n <= 10000 {
		s.WeakTestPrice = n
	}
	if n, ok := envIntOpt("SUB_GRACE_MIN"); ok && n >= 0 {
		s.Grace = time.Duration(n) * time.Minute
	}
	if n, ok := envIntOpt("WEAK_ORDER_TIMEOUT_MIN"); ok && n >= 5 {
		s.WeakOrderTimeout = time.Duration(n) * time.Minute
	}
	if n, ok := envIntOpt("WEAK_ORDER_MAX_GEN"); ok && n >= 1 {
		s.WeakOrderMaxGen = n
	}
	if n, ok := envIntOpt("BILLING_RECONCILE_SEC"); ok && n >= 5 {
		s.ReconcileInterval = time.Duration(n) * time.Second
	}
	for _, f := range strings.FieldsFunc(os.Getenv("ADMIN_IDS"), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		id, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil || id <= 0 {
			return s, fmt.Errorf("ADMIN_IDS: bad Telegram id %q", f)
		}
		s.AdminIDs = append(s.AdminIDs, id)
	}
	for _, p := range billing.DefaultPlans() {
		up := strings.ToUpper(p.Code)
		if n, ok := envIntOpt("PLAN_" + up + "_DAILY"); ok {
			p.DailyLimit = n
		}
		if !p.IsFree() {
			if n, ok := envIntOpt("PLAN_" + up + "_PRICE"); ok {
				if n == 0 {
					continue // plan switched off
				}
				p.PriceStars = n
			}
		}
		s.Plans = append(s.Plans, p)
	}
	if _, err := s.Catalog(); err != nil {
		return s, err
	}
	return s, nil
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
		TGMaxRPS:               DefaultTGMaxRPS,
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
		AttemptTTLDays:         DefaultAttemptTTLDays,
		CleanupJobDays:         DefaultCleanupJobDays,
		ChainCacheTTL:          DefaultChainCacheTTL,
		GenStrategy:            GenStrategyBatch,
		GenABBatchPercent:      DefaultGenABBatchPercent,
		GenBatchSize:           DefaultGenBatchSize,
		GenBatchParallel:       DefaultGenBatchParallel,
		GenMaxActivePersonal:   DefaultGenMaxActivePersonal,
		GenTemplateReuse:       true,
		MetricsToken:           strings.TrimSpace(os.Getenv("METRICS_TOKEN")),
		RedisURL:               strings.TrimSpace(os.Getenv("REDIS_URL")),
		RedisPrefix:            DefaultRedisPrefix,
		InstanceID:             strings.TrimSpace(os.Getenv("INSTANCE_ID")),
		UpdateQueueMax:         DefaultUpdateQueueMax,
		UpdateMaxAttempts:      DefaultUpdateMaxAttempts,
		UpdateLease:            DefaultUpdateLease,
		UpdateDoneTTL:          DefaultUpdateDoneTTL,
		UpdateDeadTTL:          DefaultUpdateDeadTTL,
	}
	if err := cfg.loadCluster(); err != nil {
		return nil, err
	}
	subs, err := loadSubscriptions()
	if err != nil {
		return nil, err
	}
	cfg.Subscriptions = subs
	cfg.TelegramAPIURL = strings.TrimRight(strings.TrimSpace(os.Getenv("TELEGRAM_API_URL")), "/")
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TELEGRAM_TEST_ENV"))); v == "1" || v == "true" || v == "yes" {
		cfg.TelegramTestEnv = true
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
	if n, ok := envIntOpt("TG_MAX_RPS"); ok && n > 0 {
		cfg.TGMaxRPS = n
	}
	cfg.BroadcastRPS = DefaultBroadcastRPS(cfg.TGMaxRPS)
	if n, ok := envIntOpt("BROADCAST_RPS"); ok && n > 0 {
		cfg.BroadcastRPS = min(n, cfg.TGMaxRPS)
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
	// Audit names: DB_CONN_MAX_LIFETIME / DB_CONN_MAX_IDLE_TIME accept a Go
	// duration ("30m", "90s") or plain seconds; they win over the *_SEC forms.
	if d, ok := envDurationOpt("DB_CONN_MAX_LIFETIME"); ok && d > 0 {
		cfg.DBConnMaxLifetime = d
	}
	if d, ok := envDurationOpt("DB_CONN_MAX_IDLE_TIME"); ok && d > 0 {
		cfg.DBConnMaxIdleTime = d
	}
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("GEN_STRATEGY"))); v {
	case GenStrategyBatch, GenStrategyFull, GenStrategyAB:
		cfg.GenStrategy = v
	case "":
	default:
		return nil, fmt.Errorf("GEN_STRATEGY must be batch, full or ab (got %q)", v)
	}
	if n, ok := envIntOpt("GEN_AB_BATCH_PERCENT"); ok && n >= 0 && n <= 100 {
		cfg.GenABBatchPercent = n
	}
	if n, ok := envIntOpt("GEN_BATCH_SIZE"); ok && n >= 2 && n <= 10 {
		cfg.GenBatchSize = n
	}
	if n, ok := envIntOpt("GEN_BATCH_PARALLEL"); ok && n >= 1 && n <= 4 {
		cfg.GenBatchParallel = n
	}
	if n, ok := envIntOpt("GEN_MAX_ACTIVE_PERSONAL"); ok && n >= 0 {
		cfg.GenMaxActivePersonal = n
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("GEN_TEMPLATE_REUSE"))); v != "" {
		cfg.GenTemplateReuse = !(v == "0" || v == "false" || v == "no" || v == "off")
	}
	if n, ok := envIntOpt("GEN_PREGEN_AHEAD"); ok && n >= 0 {
		cfg.GenPregenAhead = min(n, 1)
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
	if n, ok := envIntOpt("ATTEMPT_TTL_DAYS"); ok && n > 0 {
		cfg.AttemptTTLDays = n
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
	// A worker never receives the webhook: it needs neither the public URL
	// nor the secret.
	if cfg.WebhookURL == "" && cfg.ServesWebhook() {
		missing = append(missing, "WEBHOOK_URL")
	}
	// A production webhook without a secret accepts forged updates from
	// anyone who knows the URL — refuse to start instead of only warning.
	if cfg.Production && cfg.WebhookSecret == "" && cfg.ServesWebhook() {
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

// DefaultBroadcastRPS is the default BROADCAST_RPS: two thirds of
// TG_MAX_RPS (at least 1).
func DefaultBroadcastRPS(tgMaxRPS int) int {
	return max(1, tgMaxRPS*2/3)
}

// DefaultTGMaxRPS is the default TG_MAX_RPS.
const DefaultTGMaxRPS = 25

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
	DefaultAttemptTTLDays        = 180
	// R-10a pool defaults.
	DefaultDBMinConns        = 5
	DefaultDBConnMaxLifetime = 30 * time.Minute
	DefaultDBConnMaxIdleTime = 5 * time.Minute
	// DefaultChainCacheTTL: in-memory cache of a subject's chain structure.
	DefaultChainCacheTTL = 45 * time.Second
)

// Generation strategies (GEN_STRATEGY).
const (
	GenStrategyBatch = "batch"
	GenStrategyFull  = "full"
	GenStrategyAB    = "ab"
)

// Generation tuning defaults.
const (
	DefaultGenABBatchPercent    = 50
	DefaultGenBatchSize         = 5
	DefaultGenBatchParallel     = 2
	DefaultGenMaxActivePersonal = 300
)

// DBConnsReserved is the minimum part of the pgx pool kept for background
// work (generation/translation workers, reapers, cleanup) — update handlers
// get at most DB_MAX_CONNS - reserve connections.
const DBConnsReserved = 8

// TranslationWorkers is the number of background translation workers.
const TranslationWorkers = 2

// BackgroundConns estimates the pool connections the background work can
// hold at once: one per generation worker, one per translation worker, plus
// 2 for the reapers / cleanup / backfill (short queries that rarely overlap).
// The quality-sweep lock and the LISTEN connection are DEDICATED direct
// connections outside the pool. With the defaults (4 + 2 + 2) this equals
// the historical reserve of 8; GEN_WORKERS=16 reserves 20.
func (c *Config) BackgroundConns() int {
	gw := c.GenWorkers
	if gw <= 0 {
		gw = DefaultGenWorkers
	}
	return max(DBConnsReserved, gw+TranslationWorkers+2)
}

// envDurationOpt reads a duration env var: a Go duration ("30m") or plain
// seconds ("1800").
func envDurationOpt(key string) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, true
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false
	}
	return d, true
}

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

// Process roles (ROLE).
const (
	RoleAll    = "all"
	RoleWeb    = "web"
	RoleWorker = "worker"
)

// Backends (QUEUE_BACKEND, RATELIMIT_BACKEND, CACHE_BACKEND).
const (
	BackendMemory   = "memory"
	BackendPostgres = "postgres"
	BackendRedis    = "redis"
)

// Cluster defaults.
const (
	DefaultRedisPrefix       = "juz40"
	DefaultUpdateQueueMax    = 20000
	DefaultUpdateMaxAttempts = 3
	DefaultUpdateLease       = 45 * time.Second
	DefaultUpdateDoneTTL     = 48 * time.Hour
	DefaultUpdateDeadTTL     = 14 * 24 * time.Hour
)

// loadCluster reads and validates the cluster settings (ROLE, backends,
// Redis, durable update queue).
func (c *Config) loadCluster() error {
	c.Role = strings.ToLower(strings.TrimSpace(os.Getenv("ROLE")))
	switch c.Role {
	case "":
		c.Role = RoleAll
	case RoleAll, RoleWeb, RoleWorker:
	default:
		return fmt.Errorf("ROLE must be all, web or worker (got %q)", c.Role)
	}
	split := c.Role != RoleAll

	pick := func(env string, allowed []string, def string) (string, error) {
		v := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
		if v == "" {
			return def, nil
		}
		for _, a := range allowed {
			if v == a {
				return v, nil
			}
		}
		return "", fmt.Errorf("%s must be one of %v (got %q)", env, allowed, v)
	}
	var err error
	defQueue, defLimit := BackendMemory, BackendMemory
	if split {
		defQueue, defLimit = BackendPostgres, BackendPostgres
	}
	if c.QueueBackend, err = pick("QUEUE_BACKEND", []string{BackendMemory, BackendPostgres, BackendRedis}, defQueue); err != nil {
		return err
	}
	if c.RateLimitBackend, err = pick("RATELIMIT_BACKEND", []string{BackendMemory, BackendPostgres, BackendRedis}, defLimit); err != nil {
		return err
	}
	if c.CacheBackend, err = pick("CACHE_BACKEND", []string{BackendMemory, BackendRedis}, BackendMemory); err != nil {
		return err
	}
	if split && c.QueueBackend == BackendMemory {
		return fmt.Errorf("ROLE=%s needs a shared update queue: QUEUE_BACKEND=postgres or redis (memory works only with ROLE=all)", c.Role)
	}
	if c.RedisURL == "" {
		for env, v := range map[string]string{"QUEUE_BACKEND": c.QueueBackend, "RATELIMIT_BACKEND": c.RateLimitBackend, "CACHE_BACKEND": c.CacheBackend} {
			if v == BackendRedis {
				return fmt.Errorf("%s=redis needs REDIS_URL", env)
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("REDIS_PREFIX")); v != "" {
		c.RedisPrefix = v
	}
	if c.InstanceID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "juz40"
		}
		c.InstanceID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if n, ok := envIntOpt("UPDATE_WORKERS"); ok && n > 0 {
		c.UpdateWorkers = n
	}
	if n, ok := envIntOpt("UPDATE_QUEUE_MAX"); ok && n >= 0 {
		c.UpdateQueueMax = n
	}
	if n, ok := envIntOpt("UPDATE_MAX_ATTEMPTS"); ok && n > 0 {
		c.UpdateMaxAttempts = min(n, 20)
	}
	if n, ok := envIntOpt("UPDATE_LEASE_SEC"); ok && n >= 10 {
		c.UpdateLease = time.Duration(n) * time.Second
	}
	if n, ok := envIntOpt("UPDATE_DONE_TTL_HOURS"); ok && n >= 25 {
		c.UpdateDoneTTL = time.Duration(n) * time.Hour
	}
	if n, ok := envIntOpt("UPDATE_DEAD_TTL_DAYS"); ok && n > 0 {
		c.UpdateDeadTTL = time.Duration(n) * 24 * time.Hour
	}
	return nil
}

// ServesWebhook reports whether this process accepts the Telegram webhook.
func (c *Config) ServesWebhook() bool { return c.Role != RoleWorker }

// HandlesUpdates reports whether this process handles updates and runs the
// background workers (generation, translation, reapers, cleanup).
func (c *Config) HandlesUpdates() bool { return c.Role != RoleWeb }

// DurableQueue reports whether updates go through the shared queue
// (postgres / redis) instead of the in-process channel.
func (c *Config) DurableQueue() bool { return c.QueueBackend != BackendMemory }

// Clustered reports whether more than one process may share the work
// (split roles or a shared queue): cross-instance events are then
// broadcast (generation / translation finished).
func (c *Config) Clustered() bool { return c.Role != RoleAll || c.DurableQueue() }

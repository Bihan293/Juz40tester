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

	// DeepSeek API settings for AI test generation.
	DeepSeekAPIKey        string
	DeepSeekModel         string // non-thinking fallback model (DEEPSEEK_MODEL), default deepseek-v4-pro
	DeepSeekReasonerModel string // primary thinking model (DEEPSEEK_REASONER_MODEL), default deepseek-flash
	DeepSeekBaseURL       string // overridable via DEEPSEEK_BASE_URL

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
	OffPeakStartHour int  // custom window start (inclusive); -1 = official schedule
	OffPeakEndHour   int  // custom window end (exclusive)
	OffPeakCustom    bool // true when a custom window is configured
}

// Load reads configuration from environment variables and validates that
// all required variables are set. No secrets are hardcoded in the codebase.
func Load() (*Config, error) {
	cfg := &Config{
		BotToken:              os.Getenv("BOT_TOKEN"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		WebhookURL:            strings.TrimRight(os.Getenv("WEBHOOK_URL"), "/"),
		Port:                  os.Getenv("PORT"),
		DeepSeekAPIKey:        os.Getenv("DEEPSEEK_API_KEY"),
		DeepSeekModel:         os.Getenv("DEEPSEEK_MODEL"),
		DeepSeekReasonerModel: os.Getenv("DEEPSEEK_REASONER_MODEL"),
		DeepSeekBaseURL:       os.Getenv("DEEPSEEK_BASE_URL"),
		OffPeakStartHour:      -1,
		OffPeakEndHour:        -1,
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
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %v", missing)
	}
	return cfg, nil
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

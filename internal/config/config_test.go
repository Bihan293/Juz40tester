package config

import (
	"testing"
	"time"
)

// at builds a UTC timestamp (day is a Monday-based offset: 2026-09-28 is a
// Monday, 2026-10-03 is a Saturday, 2026-10-04 a Sunday).
func at(month time.Month, day, hour int) time.Time {
	return time.Date(2026, month, day, hour, 0, 0, 0, time.UTC)
}

// TestOfficialOffPeakSchedule pins the DeepSeek billing schedule: peak is
// 01:00–04:00 and 06:00–10:00 UTC on WEEKDAYS only; nights, evenings and
// weekends are off-peak (half price). The old code deferred generation to
// 01:00–06:00 server-local — deep inside the NEW peak window, i.e. it paid
// double exactly where it tried to save. This test is the regression alarm.
func TestOfficialOffPeakSchedule(t *testing.T) {
	cfg := &Config{} // no custom window -> official schedule
	cases := []struct {
		when    time.Time
		offPeak bool
	}{
		{at(time.September, 28, 0), true},  // Mon 00:00 UTC — off-peak
		{at(time.September, 28, 1), false}, // Mon 01:00 — PEAK starts
		{at(time.September, 28, 3), false}, // Mon 03:00 — PEAK
		{at(time.September, 28, 4), true},  // Mon 04:00 — off-peak again
		{at(time.September, 28, 6), false}, // Mon 06:00 — second PEAK window
		{at(time.September, 28, 9), false}, // Mon 09:00 — PEAK
		{at(time.September, 28, 10), true}, // Mon 10:00 — off-peak
		{at(time.September, 28, 16), true}, // Mon 16:00 — off-peak
		{at(time.October, 3, 2), true},     // Sat 02:00 — weekend: off-peak all day
		{at(time.October, 3, 7), true},     // Sat 07:00 — weekend: off-peak all day
		{at(time.October, 4, 8), true},     // Sun 08:00 — weekend: off-peak all day
	}
	for _, c := range cases {
		if got := cfg.IsOffPeak(c.when); got != c.offPeak {
			t.Fatalf("IsOffPeak(%v) = %v, want %v", c.when, got, c.offPeak)
		}
	}
}

// TestNextOffPeakStart ensures deferred (non-urgent) jobs are scheduled
// into the next CHEAP window, never into a peak one.
func TestNextOffPeakStart(t *testing.T) {
	cfg := &Config{}

	// Already off-peak -> unchanged.
	now := at(time.September, 28, 12) // Mon 12:00 UTC — off-peak
	if got := cfg.NextOffPeakStart(now); !got.Equal(now) {
		t.Fatalf("off-peak now must not be deferred, got %v", got)
	}

	// Mon 02:00 (peak) -> 04:00 same day.
	if got := cfg.NextOffPeakStart(at(time.September, 28, 2)); got.Hour() != 4 || got.Day() != 28 {
		t.Fatalf("peak 02:00 must defer to 04:00, got %v", got)
	}

	// Fri 07:00 (peak) -> 10:00 same day.
	if got := cfg.NextOffPeakStart(at(time.October, 2, 7)); got.Hour() != 10 || got.Day() != 2 {
		t.Fatalf("peak 07:00 must defer to 10:00, got %v", got)
	}

	// Custom window keeps working (wrap-around 22:00–06:00 local).
	custom := &Config{OffPeakStartHour: 22, OffPeakEndHour: 6, OffPeakCustom: true}
	loc := time.FixedZone("T", 3*3600)
	local := time.Date(2026, time.September, 28, 12, 0, 0, 0, loc)
	if custom.IsOffPeak(local) {
		t.Fatal("12:00 local must be outside the custom 22-06 window")
	}
	if got := custom.NextOffPeakStart(local); got.Hour() != 22 {
		t.Fatalf("custom window must defer to 22:00 local, got %v", got)
	}
}

// TestWebhookURLTrimsTrailingSlash pins the webhook fix: a WEBHOOK_URL with
// a trailing slash produced "https://host//telegram/webhook" — Telegram
// rejects such a URL and the whole bot stays silent after deploy.
func TestWebhookURLTrimsTrailingSlash(t *testing.T) {
	t.Setenv("BOT_TOKEN", "x")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WEBHOOK_URL", "https://example.onrender.com/")
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookURL != "https://example.onrender.com" {
		t.Fatalf("trailing slash must be trimmed, got %q", cfg.WebhookURL)
	}
}

// TestWebhookSecretRequiredInProduction (audit #28): a production webhook
// without WEBHOOK_SECRET must refuse to start; development may run without.
func TestWebhookSecretRequiredInProduction(t *testing.T) {
	t.Setenv("BOT_TOKEN", "x")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WEBHOOK_URL", "https://example.onrender.com")
	t.Setenv("WEBHOOK_SECRET", "")

	t.Setenv("APP_ENV", "")
	if _, err := Load(); err == nil {
		t.Fatal("production (default) without WEBHOOK_SECRET must fail")
	}
	t.Setenv("APP_ENV", "production")
	if _, err := Load(); err == nil {
		t.Fatal("APP_ENV=production without WEBHOOK_SECRET must fail")
	}
	t.Setenv("APP_ENV", "development")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("development must start without a secret: %v", err)
	}
	if cfg.Production {
		t.Fatal("APP_ENV=development must not be production")
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	if _, err := Load(); err != nil {
		t.Fatalf("production with a secret must start: %v", err)
	}
}

// TestDBMaxConns (audit #20): the pool size is bounded by default and
// overridable.
func TestDBMaxConns(t *testing.T) {
	t.Setenv("BOT_TOKEN", "x")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WEBHOOK_URL", "https://example.onrender.com")
	t.Setenv("WEBHOOK_SECRET", "s")
	t.Setenv("DB_MAX_CONNS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBMaxConns != defaultDBMaxConns {
		t.Fatalf("default DBMaxConns = %d", cfg.DBMaxConns)
	}
	t.Setenv("DB_MAX_CONNS", "7")
	cfg, _ = Load()
	if cfg.DBMaxConns != 7 {
		t.Fatalf("DB_MAX_CONNS override = %d", cfg.DBMaxConns)
	}
}

func TestGenWorkersConfig(t *testing.T) {
	t.Setenv("BOT_TOKEN", "x")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("WEBHOOK_URL", "https://example.onrender.com")
	t.Setenv("WEBHOOK_SECRET", "s3cret")
	for _, c := range []struct {
		env    string
		want   int
		dsEnv  string
		wantDS int
	}{
		{"", DefaultGenWorkers, "", DefaultGenDeepSeekConcurrency},
		{"6", 6, "3", 3},
		{"500", MaxGenWorkers, "0", DefaultGenDeepSeekConcurrency},
		{"junk", DefaultGenWorkers, "-1", DefaultGenDeepSeekConcurrency},
	} {
		t.Setenv("GEN_WORKERS", c.env)
		t.Setenv("GEN_DEEPSEEK_CONCURRENCY", c.dsEnv)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GenWorkers != c.want || cfg.GenDeepSeekConcurrency != c.wantDS {
			t.Fatalf("GEN_WORKERS=%q GEN_DEEPSEEK_CONCURRENCY=%q -> %d/%d, want %d/%d",
				c.env, c.dsEnv, cfg.GenWorkers, cfg.GenDeepSeekConcurrency, c.want, c.wantDS)
		}
	}
}

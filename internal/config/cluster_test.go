package config

import (
	"strings"
	"testing"
	"time"
)

func setBase(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"BOT_TOKEN": "t", "DATABASE_URL": "postgres://x", "WEBHOOK_URL": "https://x", "WEBHOOK_SECRET": "s",
		"ROLE": "", "QUEUE_BACKEND": "", "RATELIMIT_BACKEND": "", "CACHE_BACKEND": "", "REDIS_URL": "",
		"UPDATE_QUEUE_MAX": "", "UPDATE_MAX_ATTEMPTS": "", "UPDATE_LEASE_SEC": "", "UPDATE_WORKERS": "",
		"INSTANCE_ID": "", "REDIS_PREFIX": "", "UPDATE_DONE_TTL_HOURS": "", "UPDATE_DEAD_TTL_DAYS": "",
	} {
		t.Setenv(k, v)
	}
}

// Defaults: ROLE=all keeps the historical in-process behaviour.
func TestClusterDefaultsAll(t *testing.T) {
	setBase(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Role != RoleAll || cfg.QueueBackend != BackendMemory || cfg.RateLimitBackend != BackendMemory || cfg.CacheBackend != BackendMemory {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.DurableQueue() || cfg.Clustered() || !cfg.ServesWebhook() || !cfg.HandlesUpdates() {
		t.Fatal("ROLE=all must serve the webhook, handle updates and stay un-clustered")
	}
	if cfg.UpdateQueueMax != DefaultUpdateQueueMax || cfg.UpdateMaxAttempts != 3 || cfg.UpdateLease != 45*time.Second {
		t.Fatalf("queue defaults: %+v", cfg)
	}
	if cfg.InstanceID == "" {
		t.Fatal("instance id not derived")
	}
}

func TestClusterSplitRoles(t *testing.T) {
	setBase(t)
	t.Setenv("ROLE", "worker")
	t.Setenv("WEBHOOK_URL", "")
	t.Setenv("WEBHOOK_SECRET", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("a worker needs neither WEBHOOK_URL nor WEBHOOK_SECRET: %v", err)
	}
	if cfg.QueueBackend != BackendPostgres || cfg.RateLimitBackend != BackendPostgres || cfg.ServesWebhook() || !cfg.HandlesUpdates() || !cfg.Clustered() {
		t.Fatalf("worker: %+v", cfg)
	}

	setBase(t)
	t.Setenv("ROLE", "web")
	t.Setenv("WEBHOOK_SECRET", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "WEBHOOK_SECRET") {
		t.Fatalf("web in production without a secret must fail: %v", err)
	}
	t.Setenv("WEBHOOK_SECRET", "s")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ServesWebhook() || cfg.HandlesUpdates() {
		t.Fatalf("web: %+v", cfg)
	}
}

func TestClusterValidation(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"ROLE": "boss"}, "ROLE"},
		{map[string]string{"ROLE": "web", "QUEUE_BACKEND": "memory"}, "shared update queue"},
		{map[string]string{"QUEUE_BACKEND": "redis"}, "REDIS_URL"},
		{map[string]string{"RATELIMIT_BACKEND": "redis"}, "REDIS_URL"},
		{map[string]string{"CACHE_BACKEND": "redis"}, "REDIS_URL"},
		{map[string]string{"QUEUE_BACKEND": "kafka"}, "QUEUE_BACKEND"},
	}
	for _, c := range cases {
		setBase(t)
		for k, v := range c.env {
			t.Setenv(k, v)
		}
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err=%v, want mention of %q", c.env, err, c.want)
		}
	}
}

func TestClusterRedisOptIn(t *testing.T) {
	setBase(t)
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// REDIS_URL alone switches nothing to Redis.
	if cfg.QueueBackend == BackendRedis || cfg.RateLimitBackend == BackendRedis || cfg.CacheBackend == BackendRedis {
		t.Fatalf("Redis used without an explicit backend switch: %+v", cfg)
	}
	t.Setenv("ROLE", "worker")
	t.Setenv("QUEUE_BACKEND", "redis")
	t.Setenv("RATELIMIT_BACKEND", "redis")
	t.Setenv("CACHE_BACKEND", "redis")
	t.Setenv("UPDATE_LEASE_SEC", "5") // below the minimum → ignored
	t.Setenv("UPDATE_MAX_ATTEMPTS", "5")
	t.Setenv("UPDATE_WORKERS", "12")
	t.Setenv("INSTANCE_ID", "w-1")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QueueBackend != BackendRedis || cfg.RateLimitBackend != BackendRedis || cfg.CacheBackend != BackendRedis ||
		cfg.UpdateLease != DefaultUpdateLease || cfg.UpdateMaxAttempts != 5 || cfg.UpdateWorkers != 12 || cfg.InstanceID != "w-1" {
		t.Fatalf("redis worker: %+v", cfg)
	}
}

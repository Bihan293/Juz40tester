package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Bihan293/Juz40tester/internal/config"
)

func TestUsesRedis(t *testing.T) {
	cfg := &config.Config{QueueBackend: config.BackendMemory, RateLimitBackend: config.BackendMemory, CacheBackend: config.BackendMemory}
	if usesRedis(cfg) {
		t.Fatal("no backend is redis")
	}
	for _, set := range []func(*config.Config){
		func(c *config.Config) { c.QueueBackend = config.BackendRedis },
		func(c *config.Config) { c.RateLimitBackend = config.BackendRedis },
		func(c *config.Config) { c.CacheBackend = config.BackendRedis },
	} {
		c := *cfg
		set(&c)
		if !usesRedis(&c) {
			t.Fatalf("redis backend not detected: %+v", c)
		}
	}
}

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

func TestHealthPingWithoutRedis(t *testing.T) {
	if err := healthPing(pinger{}, nil)(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := healthPing(pinger{errors.New("db down")}, nil)(context.Background()); err == nil {
		t.Fatal("db error not reported")
	}
}

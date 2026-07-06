package config

import (
	"context"
	"testing"
	"time"
)

type fakePinger struct {
	err error
	called bool
}

func (f *fakePinger) Ping(ctx context.Context) error {
	f.called = true
	return f.err
}

func TestHealthCheckerAllHealthy(t *testing.T) {
	a, b := &fakePinger{}, &fakePinger{}
	h := NewHealthChecker(a, b)
	if err := h.Check(context.Background()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if !a.called || !b.called {
		t.Fatalf("expected both pingers to be called")
	}
}

func TestHealthCheckerFirstError(t *testing.T) {
	a := &fakePinger{err: errFake("boom")}
	b := &fakePinger{}
	h := NewHealthChecker(a, b)
	err := h.Check(context.Background())
	if err == nil || err.Error() != "boom" {
		t.Fatalf("expected boom, got %v", err)
	}
}

type errFake string
func (e errFake) Error() string { return string(e) }

func TestLoadDefaults(t *testing.T) {
	cfg := Load()
	if cfg.RateLockTTL != DefaultRateLockTTL {
		t.Fatalf("expected default TTL %v, got %v", DefaultRateLockTTL, cfg.RateLockTTL)
	}
	if cfg.RedisURL != DefaultRedisURL {
		t.Fatalf("expected default RedisURL %q, got %q", DefaultRedisURL, cfg.RedisURL)
	}
	if cfg.DatabaseURL != DefaultDatabaseURL {
		t.Fatalf("expected default DatabaseURL %q, got %q", DefaultDatabaseURL, cfg.DatabaseURL)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://example:6380")
	t.Setenv("DATABASE_URL", "sqlite://file:test?mode=memory")
	t.Setenv("RATE_LOCK_TTL_SECONDS", "45")
	t.Setenv("MAX_STALE_AGE_MS", "500")
	t.Setenv("DEFAULT_SPREAD_BPS", "120")
	t.Setenv("PORT", "9000")
	t.Setenv("LOG_LEVEL", "WARN")

	cfg := Load()
	if cfg.RedisURL != "redis://example:6380" {
		t.Fatalf("got %q", cfg.RedisURL)
	}
	if cfg.RateLockTTL != 45*time.Second {
		t.Fatalf("got %v", cfg.RateLockTTL)
	}
	if cfg.MaxStaleAge != 500*time.Millisecond {
		t.Fatalf("got %v", cfg.MaxStaleAge)
	}
	if cfg.DefaultSpreadBPS != 120 {
		t.Fatalf("got %d", cfg.DefaultSpreadBPS)
	}
	if cfg.Port != "9000" || cfg.LogLevel != "warn" {
		t.Fatalf("got %q %q", cfg.Port, cfg.LogLevel)
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{RedisURL: "x", DatabaseURL: "x", RateLockTTL: time.Second}).Validate(); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if err := (Config{RedisURL: "", DatabaseURL: "x", RateLockTTL: time.Second}).Validate(); err == nil {
		t.Fatalf("expected error for missing RedisURL")
	}
	if err := (Config{RedisURL: "x", DatabaseURL: "x", RateLockTTL: 0}).Validate(); err == nil {
		t.Fatalf("expected error for zero TTL")
	}
}

func TestParseSQLiteURL(t *testing.T) {
	dsn, err := ParseSQLiteURL("sqlite://file:test?mode=memory")
	if err != nil || dsn != "file:test?mode=memory" {
		t.Fatalf("got %q err=%v", dsn, err)
	}
	if _, err := ParseSQLiteURL("postgres://x"); err == nil {
		t.Fatalf("expected error for non-sqlite scheme")
	}
}
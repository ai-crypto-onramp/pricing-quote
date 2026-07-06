package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration loaded from environment variables.
type Config struct {
	RedisURL     string
	DatabaseURL  string
	RateLockTTL  time.Duration
	Port         string
	LogLevel     string
	MaxStaleAge  time.Duration
	DefaultSpreadBPS int
}

// Defaults.
const (
	DefaultRedisURL     = "redis://localhost:6379"
	DefaultDatabaseURL  = "sqlite://file:pricing_quote?mode=memory&cache=shared"
	DefaultRateLockTTL  = 30 * time.Second
	DefaultPort         = "8080"
	DefaultLogLevel     = "info"
	DefaultMaxStaleAge  = 250 * time.Millisecond
	DefaultSpreadBPS    = 100
)

// Load reads configuration from environment variables, applying defaults.
func Load() Config {
	cfg := Config{
		RedisURL:         envStr("REDIS_URL", DefaultRedisURL),
		DatabaseURL:      envStr("DATABASE_URL", DefaultDatabaseURL),
		RateLockTTL:      envDurSec("RATE_LOCK_TTL_SECONDS", DefaultRateLockTTL),
		Port:             envStr("PORT", DefaultPort),
		LogLevel:         envStr("LOG_LEVEL", DefaultLogLevel),
		MaxStaleAge:      envDurMS("MAX_STALE_AGE_MS", DefaultMaxStaleAge),
		DefaultSpreadBPS: envInt("DEFAULT_SPREAD_BPS", DefaultSpreadBPS),
	}
	cfg.LogLevel = strings.ToLower(cfg.LogLevel)
	return cfg
}

// Validate returns an error if required configuration is missing or invalid.
func (c Config) Validate() error {
	if c.RedisURL == "" {
		return errors.New("REDIS_URL must be set")
	}
	if c.DatabaseURL == "" {
		return errors.New("DATABASE_URL must be set")
	}
	if c.RateLockTTL <= 0 {
		return errors.New("RATE_LOCK_TTL_SECONDS must be > 0")
	}
	return nil
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDurSec(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if seconds, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(seconds * float64(time.Second))
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

func envDurMS(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if ms, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(ms * float64(time.Millisecond))
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

// ParseSQLiteURL converts a "sqlite://..." URL into a go-sqlite3 driver DSN.
// Supports both on-disk and in-memory DSNs.
func ParseSQLiteURL(url string) (string, error) {
	const prefix = "sqlite://"
	if !strings.HasPrefix(url, prefix) {
		return "", fmt.Errorf("unsupported database url scheme: %s", url)
	}
	return strings.TrimPrefix(url, prefix), nil
}

// HealthChecker verifies startup connectivity to OLTP and Redis.
type HealthChecker struct {
	pingers []Pinger
}

// Pinger is a dependency that can be pinged.
type Pinger interface {
	Ping(ctx context.Context) error
}

// NewHealthChecker returns a HealthChecker that pings the given dependencies.
func NewHealthChecker(pingers ...Pinger) *HealthChecker {
	return &HealthChecker{pingers: pingers}
}

// Check pings every dependency and returns the first error, or nil if all are
// healthy.
func (h *HealthChecker) Check(ctx context.Context) error {
	for _, p := range h.pingers {
		if err := p.Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}
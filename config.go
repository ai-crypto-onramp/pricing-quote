package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// LoadConfig builds a Config from environment variables with documented defaults.
// Missing or invalid values fall back to DefaultConfig values.
func LoadConfig() Config {
	c := DefaultConfig()
	if v := os.Getenv("RATE_LOCK_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.RateLockTTL = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("MAX_STALE_AGE_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxStaleAge = time.Duration(n) * time.Millisecond
		}
	}
	if v := os.Getenv("DEFAULT_SPREAD_BPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.DefaultSpreadBPS = n
		}
	}
	if v := os.Getenv("SLIPPAGE_TOLERANCE_BPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SlippageToleranceBPS = n
		}
	}
	if v := os.Getenv("BULK_QUOTE_MAX_ITEMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.BulkQuoteMaxItems = n
		}
	}
	c.RedisURL = envOr("REDIS_URL", "redis://localhost:6379")
	c.DatabaseURL = envOr("DATABASE_URL", "")
	c.Port = envOr("PORT", "8080")
	c.FeeScheduleURL = envOr("FEE_SCHEDULE_URL", "http://config-svc/v1/fee-schedules")
	c.FXHedgingURL = envOr("FX_HEDGING_URL", "http://fx-hedging:8080")
	c.ExchangeConnectorURL = envOr("EXCHANGE_CONNECTOR_URL", "http://exchange-connectors:8080")
	c.RateFeedTopic = envOr("RATE_FEED_TOPIC", "spot.rates")
	c.OTLPEndpoint = envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	c.LogLevel = envOr("LOG_LEVEL", "info")
	if v := os.Getenv("L1_CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.L1CacheSize = n
		}
	}
	if v := os.Getenv("L1_CACHE_TTL_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.L1CacheTTL = time.Duration(n) * time.Millisecond
		}
	}
	return c
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// ConfigFromEnv is a convenience alias used in tests.
func ConfigFromEnv() Config { return LoadConfig() }

// String returns a log-safe representation of the config (secrets redacted).
func (c Config) String() string {
	return fmt.Sprintf("port=%s ttl=%s stale=%s spread=%d slip=%d bulk=%d redis=%s db_set=%v",
		c.Port, c.RateLockTTL, c.MaxStaleAge, c.DefaultSpreadBPS,
		c.SlippageToleranceBPS, c.BulkQuoteMaxItems,
		maskURL(c.RedisURL), c.DatabaseURL != "")
}

func maskURL(u string) string {
	if i := strings.Index(u, "@"); i >= 0 {
		return "redis://***@" + u[i+1:]
	}
	return u
}
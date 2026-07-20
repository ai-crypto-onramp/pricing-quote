package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	pricing "github.com/ai-crypto-onramp/pricing-quote/internal"
	"github.com/ai-crypto-onramp/pricing-quote/internal/migrations"
	_ "github.com/lib/pq"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run a one-shot /healthz probe and exit 0/1")
	flag.Parse()
	if *healthcheck {
		os.Exit(pricing.RunHealthcheck())
	}
	cfg := pricing.LoadConfig()
	log := pricing.NewLogger(cfg.LogLevel)
	log.Info("starting pricing-quote", pricing.FStr("config", cfg.String()))

	if os.Getenv("DEV_MODE") != "1" {
		if os.Getenv("PRICE_FEED_URL") == "" && os.Getenv("ORACLE_URL") == "" && os.Getenv("RATE_FEED_URL") == "" {
			log.Error("PRICE_FEED_URL (or ORACLE_URL) required in production mode; spot rates are otherwise seeded stubs — set DEV_MODE=1 for local dev", pricing.FStr("hint", "DEV_MODE=1 for local dev"))
			os.Exit(1)
		}
	} else {
		log.Info("DEV_MODE=1: using seeded in-memory spot rates — NOT FOR PRODUCTION")
	}

	if dsn := dbURL(); dsn != "" {
		if err := applyMigrations(dsn); err != nil {
			log.Error("startup migrations failed", pricing.FErr(err))
			os.Exit(1)
		}
		log.Info("database migrations applied")
	}

	if err := pricing.RunWithConfig(cfg, log); err != nil {
		log.Error("server exited with error", pricing.FErr(err))
		os.Exit(1)
	}
}

// dbURL returns the database DSN to use for startup migrations. It prefers
// DB_URL (the variable read by cmd/migrate) and falls back to DATABASE_URL
// (the variable read by the rest of the service config). When neither is set
// the service boots in its in-memory mode without touching Postgres.
func dbURL() string {
	if v := os.Getenv("DB_URL"); v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}

// applyMigrations opens the pricing_quote database, applies all pending
// embedded migrations, and closes the connection. It is idempotent: re-runs
// after make reset-db are a no-op thanks to the schema_migrations table
// maintained by migrations.Up.
func applyMigrations(dsn string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping db: %w", err)
	}
	if err := migrations.Up(ctx, db); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
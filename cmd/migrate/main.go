// Command migrate applies the embedded pricing-quote migrations against the
// database named by DB_URL. It backs the Makefile migrate-up / migrate-down
// targets without pulling in an external migration tool. The pricing-quote
// server itself remains in-memory by default; this command is the only path
// that touches Postgres.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ai-crypto-onramp/pricing-quote/internal/migrations"
	_ "github.com/lib/pq"
)

func main() {
	direction := flag.String("direction", "up", "up or down")
	flag.Parse()

	if err := run(*direction); err != nil {
		log.Fatalf("migrate: %v", err)
	}
}

func run(direction string) error {
	dsn := os.Getenv("DB_URL")
	if dsn == "" {
		return fmt.Errorf("DB_URL is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("sql.Open: %w", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch direction {
	case "up":
		if err := migrations.Up(ctx, db); err != nil {
			return err
		}
		fmt.Println("migrations applied")
	case "down":
		if err := migrations.Down(ctx, db); err != nil {
			return err
		}
		fmt.Println("migrations rolled back")
	default:
		return fmt.Errorf("unknown direction %q (want up or down)", direction)
	}
	return nil
}
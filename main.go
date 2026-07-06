package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ai-crypto-onramp/pricing-quote/internal/config"
	"github.com/ai-crypto-onramp/pricing-quote/internal/store"
	_ "modernc.org/sqlite"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config invalid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	app, err := buildApp(ctx, cfg)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	defer app.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", app.readyz)

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: mux}
	go func() {
		log.Printf("listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-stop
	shutdownCtx, cancelShut := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShut()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

type app struct {
	health *config.HealthChecker
	oltp   store.Store
	locks  store.LockStore
}

func (a *app) Close() {
	if a.oltp != nil {
		_ = a.oltp.Close()
	}
	if a.locks != nil {
		_ = a.locks.Close()
	}
}

func buildApp(ctx context.Context, cfg config.Config) (*app, error) {
	dsn, err := config.ParseSQLiteURL(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	oltp, err := store.NewSQLiteStore(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	locks, err := store.NewRedisLockStore(ctx, cfg.RedisURL, cfg.RateLockTTL)
	if err != nil {
		_ = oltp.Close()
		return nil, err
	}

	health := config.NewHealthChecker(oltp, locks)
	if err := health.Check(ctx); err != nil {
		_ = oltp.Close()
		_ = locks.Close()
		return nil, err
	}

	return &app{health: health, oltp: oltp, locks: locks}, nil
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (a *app) readyz(w http.ResponseWriter, r *http.Request) {
	if err := a.health.Check(r.Context()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy", "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}
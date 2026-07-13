package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "run a one-shot /healthz probe and exit 0/1")
	flag.Parse()
	if *healthcheck {
		os.Exit(runHealthcheck())
	}
	cfg := LoadConfig()
	log := newLogger(cfg.LogLevel)
	log.Info("starting pricing-quote", fStr("config", cfg.String()))
	if err := runWithConfig(cfg, log); err != nil {
		log.Error("server exited with error", fErr(err))
		os.Exit(1)
	}
}

// runHealthcheck performs a single GET /healthz against localhost:8080 and
// returns 0 on success, 1 on failure. Used by the Docker HEALTHCHECK directive
// in the distroless image (which has no shell/curl).
func runHealthcheck() int {
	port := envOr("PORT", "8080")
	url := fmt.Sprintf("http://localhost:%s/healthz", port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
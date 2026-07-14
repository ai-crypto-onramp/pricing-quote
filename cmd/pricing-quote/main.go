package main

import (
	"flag"
	"os"

	pricing "github.com/ai-crypto-onramp/pricing-quote/internal"
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
	if err := pricing.RunWithConfig(cfg, log); err != nil {
		log.Error("server exited with error", pricing.FErr(err))
		os.Exit(1)
	}
}
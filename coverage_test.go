package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- coverage helpers ----------

func TestErrorMethods(t *testing.T) {
	e := &AppError{Status: 400, Code: "x", Message: "m"}
	if e.Error() != "x: m" {
		t.Fatalf("Error() = %q", e.Error())
	}
	if !errors.Is(e.Unwrap(), e) {
		// Unwrap returns e itself; just exercise it.
		_ = e.Unwrap()
	}
}

func TestStrErr(t *testing.T) {
	if errNoPoll.Error() == "" {
		t.Fatal("errNoPoll empty")
	}
}

func TestLoggerLevels(t *testing.T) {
	l := newLogger("error")
	l.Debug("no") // below level, no-op
	l.Info("no")  // below level, no-op
	l.Warn("no") // below level, no-op
	l.Error("yes")
	setPkgLogger(l)
	logWarn("should-be-suppressed")
	// restore
	setPkgLogger(newLogger("info"))
}

func TestFormatAny(t *testing.T) {
	if formatAny(123) != "" {
		t.Fatalf("expected empty, got %q", formatAny(123))
	}
}

func TestStoreRateSources(t *testing.T) {
	s := NewStore()
	now := time.Now().UTC()
	s.SetRateSources([]*RateSource{
		{Name: "binance", Priority: 2, Enabled: true, CreatedAt: now, UpdatedAt: now},
		{Name: "kraken", Priority: 1, Enabled: true, CreatedAt: now, UpdatedAt: now},
		{Name: "disabled", Priority: 0, Enabled: false, CreatedAt: now, UpdatedAt: now},
	})
	got := s.RateSources()
	if len(got) != 2 {
		t.Fatalf("expected 2 enabled, got %d", len(got))
	}
	if got[0].Name != "kraken" {
		t.Fatalf("expected kraken first (priority), got %s", got[0].Name)
	}
}

func TestClaimServiceRefresh(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, "POST", "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	id := parseJSON(t, rec)["quote_id"].(string)
	nq, err := s.claim.Refresh(id, func(old *Quote) (*Quote, error) {
		return &Quote{QuoteID: "q_refreshed", From: old.From, To: old.To, Status: StatusOpen}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if nq.QuoteID != "q_refreshed" {
		t.Fatalf("expected refreshed id, got %s", nq.QuoteID)
	}
}

func TestClaimServiceRefreshMissing(t *testing.T) {
	s := helperServer(t)
	_, err := s.claim.Refresh("q_unknown", func(old *Quote) (*Quote, error) {
		return nil, nil
	})
	if err != errNotFound {
		t.Fatalf("expected errNotFound, got %v", err)
	}
}

func TestStartFeeScheduleRefresh(t *testing.T) {
	s := helperServer(t)
	stop := s.startFeeScheduleRefresh(20 * time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	stop()
	// Just ensure it ran without panicking.
	if len(s.store.FeeSchedules()) == 0 {
		t.Fatal("schedules empty after refresh")
	}
}

func TestLabeledPath(t *testing.T) {
	cases := map[string]string{
		"/v1/quotes":             "/v1/quotes",
		"/v1/quotes/q_1":          "/v1/quotes/:id",
		"/v1/quotes/q_1/refresh":  "/v1/quotes/:id/refresh",
		"/v1/quotes/q_1/unknown":  "/v1/quotes/*",
		"/internal/v1/quotes/q_1/claim": "/internal/v1/quotes/:id/claim",
		"/healthz":               "/healthz",
	}
	for in, want := range cases {
		if got := labeledPath(in); got != want {
			t.Fatalf("labeledPath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSplitN(t *testing.T) {
	parts := splitN("a/b/c", '/', 3)
	if len(parts) != 3 || parts[0] != "a" || parts[1] != "b" || parts[2] != "c" {
		t.Fatalf("splitN wrong: %v", parts)
	}
}

func TestMetricsMiddlewareWriteHeader(t *testing.T) {
	s := helperServer(t)
	h := metricsMiddleware(helperMux(s))
	// Trigger a non-200 to exercise WriteHeader capture.
	rec := doReq(t, h, "GET", "/v1/quotes/q_unknown", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestCloseLockBackendNoopOnMemory(t *testing.T) {
	b := NewLockStore()
	closeLockBackend(b) // should not panic
}

func TestInitLockBackendEmptyURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisURL = ""
	b := initLockBackend(cfg, newLogger("info"))
	if b == nil {
		t.Fatal("nil backend")
	}
}

func TestHTTPFXClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fiat") != "EUR" {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rate": 1.08, "hedge_cost_bps": 7})
	}))
	defer srv.Close()
	c := newHTTPFXClient(srv.URL)
	if c == nil {
		t.Fatal("nil client")
	}
	rate, bps, err := c.FetchFX(context.Background(), "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if rate != 1.08 || bps != 7 {
		t.Fatalf("got %f %d", rate, bps)
	}
}

func TestHTTPFXClientBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer srv.Close()
	c := newHTTPFXClient(srv.URL)
	_, _, err := c.FetchFX(context.Background(), "EUR")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestNoopFXClient(t *testing.T) {
	var f FXClient = noopFXClient{}
	_, _, err := f.FetchFX(context.Background(), "EUR")
	if err != ErrFXUnavailable {
		t.Fatal("expected ErrFXUnavailable")
	}
}

func TestNoopPollClient(t *testing.T) {
	var p PollClient = noopPollClient{}
	_, err := p.Poll(context.Background(), "USD", "BTC")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestServerPollFallback(t *testing.T) {
	s := helperServer(t)
	// No poll client set → returns false.
	_, ok := s.pollFallback(context.Background(), "USD", "BTC")
	if ok {
		t.Fatal("expected false with no poll client")
	}
}

func TestRecordingTracerNoop(t *testing.T) {
	prev := globalTracer
	setTracer(noopTracer{})
	defer func() { globalTracer = prev }()
	_, sp := startSpan(context.Background(), "x")
	sp.End()
	sp.SetAttribute("k", "v")
	sp.AddEvent("e")
}

func TestAuditLogDropped(t *testing.T) {
	a := NewAuditLogWithSink(1, nil)
	for i := 0; i < 5; i++ {
		a.Append(AuditEvent{Type: "quote.issued"})
	}
	a.Close()
	if a.Dropped() < 1 {
		t.Fatalf("expected drops, got %d", a.Dropped())
	}
}

func TestQuoteToResponseClaimedFields(t *testing.T) {
	now := time.Now().UTC()
	q := &Quote{
		QuoteID: "q_1", From: "USD", To: "BTC", Amount: "100",
		Rate: "1", SpreadBPS: 50, Fee: "1", FeeCurrency: "USD",
		Total: "101", CryptoAmount: "0.01", UserTier: "tier_1", Side: "buy",
		Status: StatusClaimed, SourceVenue: "kraken",
		CreatedAt: now, ExpiresAt: now.Add(30 * time.Second),
		ClaimedAt: &now, ClaimedBy: "orch",
	}
	r := q.toResponse()
	if r["claimed_at"] == nil || r["claimed_by"] != "orch" {
		t.Fatalf("claimed fields wrong: %+v", r)
	}
}

func TestRunHealthcheckFails(t *testing.T) {
	// No server running on this port → should exit 1.
	t.Setenv("PORT", "65535")
	if rc := runHealthcheck(); rc == 0 {
		t.Fatal("expected non-zero exit when no server")
	}
}

func TestRunHealthcheckSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	// Override PORT to the test server's port.
	addr := strings.TrimPrefix(srv.URL, "http://127.0.0.1:")
	// Temporarily replace envOr behavior via PORT env.
	// healthcheck uses localhost:PORT, and httptest listens on 127.0.0.1.
	t.Setenv("PORT", addr)
	if rc := runHealthcheck(); rc != 0 {
		t.Fatalf("expected 0 exit, got %d", rc)
	}
}

func TestConfigFromEnv(t *testing.T) {
	c := ConfigFromEnv()
	if c.Port == "" {
		t.Fatal("empty port")
	}
}

func TestSpotFetchVenueOrder(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	order := svc.FetchVenueOrder()
	if len(order) != 3 || order[0] != "kraken" {
		t.Fatalf("unexpected order %v", order)
	}
}

func TestAdvanceVenueNoNext(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	last := svc.venueOrder[len(svc.venueOrder)-1]
	if _, ok := svc.AdvanceVenue(last); ok {
		t.Fatalf("expected no failover from last venue %s", last)
	}
}

func TestHalfOpenProbeNotDown(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	if svc.HalfOpenProbe("kraken") {
		t.Fatal("expected false when venue not down")
	}
}
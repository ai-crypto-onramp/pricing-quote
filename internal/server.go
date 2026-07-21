package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Config holds runtime knobs (env-overridable, but defaults are hardcoded).
type Config struct {
	RateLockTTL          time.Duration
	MaxStaleAge          time.Duration
	DefaultSpreadBPS     int
	SlippageToleranceBPS int
	BulkQuoteMaxItems    int

	Port                 string
	RedisURL             string
	DatabaseURL          string
	FeeScheduleURL       string
	FXHedgingURL         string
	ExchangeConnectorURL string
	RateFeedTopic        string
	OTLPEndpoint         string
	LogLevel             string
	L1CacheSize          int
	L1CacheTTL           time.Duration
}

// DefaultConfig returns the documented default configuration.
func DefaultConfig() Config {
	return Config{
		RateLockTTL:          30 * time.Second,
		MaxStaleAge:          250 * time.Millisecond,
		DefaultSpreadBPS:     100,
		SlippageToleranceBPS: 150,
		BulkQuoteMaxItems:    25,
		Port:                 "8080",
		RedisURL:             "redis://localhost:6379",
		FeeScheduleURL:       "http://config-svc/v1/fee-schedules",
		FXHedgingURL:         "http://fx-hedging:8080",
		ExchangeConnectorURL: "http://exchange-connectors:8080",
		RateFeedTopic:        "spot.rates",
		LogLevel:             "info",
		L1CacheSize:          4096,
		L1CacheTTL:           200 * time.Millisecond,
	}
}

// Server wires the store, spot, pricer, claim, audit, and HTTP handlers.
type Server struct {
	cfg    Config
	store  *Store
	locks  LockBackend
	spot   *SpotService
	pricer *Pricer
	claim  *ClaimService
	audit  *AuditLog
	log    *logger
	wsHub  *wsHub
	feed   FeedSubscriber
	poll   PollClient
}

// NewServer builds a Server with in-memory backing stores and seeded data.
func NewServer(cfg Config) *Server {
	store := NewStore()
	locks := NewLockStore()
	spot := NewSpotService(cfg.MaxStaleAge)
	pricer := NewPricer(store, spot, cfg.DefaultSpreadBPS)
	audit := NewAuditLog()
	claim := NewClaimService(store, locks, spot, audit, cfg.SlippageToleranceBPS)
	seedFeeSchedules(store)
	pricer.ReloadIndex()
	s := &Server{
		cfg:    cfg,
		store:  store,
		locks:  locks,
		spot:   spot,
		pricer: pricer,
		claim:  claim,
		audit:  audit,
		wsHub:  newWSHub(),
	}
	spot.SetOnUpdate(func(pair string, r Rate) {
		s.wsHub.fanout(pair, r)
	})
	return s
}

func seedFeeSchedules(store *Store) {
	now := time.Now().UTC()
	zero := decimal.Zero
	fs := []FeeSchedule{
		{ID: newFeeScheduleID(), UserTier: "TIER_1", Asset: "BTC", SizeBandMin: zero, SizeBandMax: decimal.NewFromInt(1000), Side: "BUY", SpreadBPS: 80, FeeType: "BPS", FeeBPS: 50, Enabled: true, UpdatedAt: now},
		{ID: newFeeScheduleID(), UserTier: "TIER_1", Asset: "BTC", SizeBandMin: decimal.NewFromInt(1000), SizeBandMax: decimal.NewFromInt(10000), Side: "BUY", SpreadBPS: 60, FeeType: "BPS", FeeBPS: 30, Enabled: true, UpdatedAt: now},
		{ID: newFeeScheduleID(), UserTier: "TIER_1", Asset: "ETH", SizeBandMin: zero, SizeBandMax: decimal.NewFromInt(100), Side: "BUY", SpreadBPS: 90, FeeType: "BPS", FeeBPS: 50, Enabled: true, UpdatedAt: now},
		{ID: newFeeScheduleID(), UserTier: "TIER_2", Asset: "BTC", SizeBandMin: zero, SizeBandMax: decimal.NewFromInt(1000), Side: "BUY", SpreadBPS: 70, FeeType: "BPS", FeeBPS: 40, Enabled: true, UpdatedAt: now},
		{ID: newFeeScheduleID(), UserTier: "TIER_2", Asset: "ETH", SizeBandMin: zero, SizeBandMax: decimal.NewFromInt(100), Side: "BUY", SpreadBPS: 75, FeeType: "BPS", FeeBPS: 40, Enabled: true, UpdatedAt: now},
		{ID: newFeeScheduleID(), UserTier: "TIER_2", Asset: "BTC", SizeBandMin: zero, SizeBandMax: decimal.NewFromInt(1000), Side: "SELL", SpreadBPS: 70, FeeType: "BPS", FeeBPS: 40, Enabled: true, UpdatedAt: now},
		{ID: newFeeScheduleID(), UserTier: "TIER_1", Asset: "BTC", SizeBandMin: zero, SizeBandMax: decimal.NewFromInt(1000), Side: "SELL", SpreadBPS: 80, FeeType: "BPS", FeeBPS: 50, Enabled: true, UpdatedAt: now},
	}
	store.SetFeeSchedules(fs)
}

// newFeeScheduleID generates an app-side UUIDv7 identifier for a fee schedule.
func newFeeScheduleID() uuid.UUID {
	id, _ := uuid.NewV7()
	return id
}

// newMux builds the HTTP routing mux for the service.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	srv := NewServer(DefaultConfig())
	srv.register(mux)
	return mux
}

// register attaches all service routes to the mux.
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/metrics", metricsHandler().ServeHTTP)
	mux.HandleFunc("/v1/quotes", s.quotesHandler)
	mux.HandleFunc("/v1/quotes/", s.quoteByIDHandler)
	mux.HandleFunc("/v1/rates/subscribe", s.ratesSubscribeHandler)
	mux.HandleFunc("/internal/v1/quotes/", s.internalQuoteHandler)
	mux.HandleFunc("/internal/v1/fee-schedules/reload", s.feeSchedulesReload)
	mux.HandleFunc("/v1/fee-schedules", s.feeSchedulesHandler)
	mux.HandleFunc("/v1/rate-sources", s.rateSourcesHandler)
	mux.HandleFunc("/v1/audit-events", s.auditEventsHandler)
}

// run starts the HTTP server on addr and blocks until the server exits.
func run(addr string) error {
	return http.ListenAndServe(addr, requestIDMiddleware(newMux()))
}

// RunWithConfig builds a server from cfg, wires the lock backend and logger,
// and starts the HTTP server on cfg.Port. Blocks until the server exits or
// ctx is canceled (graceful shutdown on ctx cancel).
func RunWithConfig(cfg Config, log *logger) error {
	return RunWithConfigCtx(context.Background(), cfg, log)
}

// RunWithConfigCtx is the context-aware variant used by tests to shut the
// server down cleanly.
func RunWithConfigCtx(ctx context.Context, cfg Config, log *logger) error {
	locks := initLockBackend(cfg, log)
	srv := NewServer(cfg)
	srv.locks = locks
	srv.claim = NewClaimService(srv.store, locks, srv.spot, srv.audit, cfg.SlippageToleranceBPS)
	srv.log = log
	mux := http.NewServeMux()
	srv.register(mux)
	h := requestIDMiddleware(mux)
	h = metricsMiddleware(h)
	stopSweep := srv.StartSweeper(60 * time.Second)
	defer stopSweep()
	stopRefresh := srv.startFeeScheduleRefresh(60 * time.Second)
	defer stopRefresh()
	if log != nil {
		log.Info("listening", FStr("port", cfg.Port))
	}
	addr := ":" + cfg.Port
	httpSrv := &http.Server{Addr: addr, Handler: h}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		<-errCh
		closeLockBackend(locks)
		return nil
	case err := <-errCh:
		closeLockBackend(locks)
		return err
	}
}

func healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.spot.IsReady() {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "spot cache cold")
		return
	}
	if s.locks != nil && !s.locks.Ready() {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "lock store unreachable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// requestIDMiddleware injects X-Request-ID into request context (header).
func requestIDMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-ID", rid)
		r.Header.Set("X-Request-ID", rid)
		h.ServeHTTP(w, r)
	})
}

func newRequestID() string {
	id, _ := uuid.NewV7()
	return id.String()
}

// writeError writes a structured error envelope.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":       code,
			"message":    message,
			"request_id": requestIDFromContext(r),
		},
	})
}

func writeErrorApp(w http.ResponseWriter, r *http.Request, e *AppError) {
	writeError(w, r, e.Status, e.Code, e.Message)
}

// quotesHandler dispatches GET /v1/quotes (list) and POST /v1/quotes (single + bulk).
func (s *Server) quotesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.listQuotes(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "could not read body")
		return
	}
	// Detect bulk vs single by presence of "items" array.
	var probe struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Items != nil {
		s.bulkQuotes(w, r, body)
		return
	}
	s.singleQuote(w, r, body)
}

// singleQuote handles POST /v1/quotes (single).
// Breaking: rate/fee/total/crypto_amount are JSON strings (decimal).
func (s *Server) singleQuote(w http.ResponseWriter, r *http.Request, body []byte) {
	ctx, span := startSpan(r.Context(), "POST /v1/quotes")
	defer span.End()
	var req quoteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if err := validateRequest(req); err != nil {
		writeErrorApp(w, r, err.(*AppError))
		return
	}
	q, appErr := s.createQuote(ctx, req)
	if appErr != nil {
		writeErrorApp(w, r, appErr)
		return
	}
	span.SetAttribute("quote_id", q.QuoteID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(q.toResponse())
}

// bulkQuotes handles POST /v1/quotes (bulk).
// Breaking: per-quote rate/fee/total/crypto_amount are JSON strings (decimal).
func (s *Server) bulkQuotes(w http.ResponseWriter, r *http.Request, body []byte) {
	ctx, span := startSpan(r.Context(), "POST /v1/quotes bulk")
	defer span.End()
	var req bulkRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.Items) > s.cfg.BulkQuoteMaxItems {
		writeError(w, r, http.StatusBadRequest, "too_many_items", "bulk quote exceeds max items")
		return
	}
	type itemResp struct {
		Ok    bool   `json:"ok"`
		Quote *Quote `json:"quote,omitempty"`
		Error string `json:"error,omitempty"`
	}
	resps := make([]itemResp, 0, len(req.Items))
	for _, it := range req.Items {
		if err := validateRequest(it); err != nil {
			resps = append(resps, itemResp{Ok: false, Error: err.(*AppError).Code})
			continue
		}
		q, appErr := s.createQuote(ctx, it)
		if appErr != nil {
			resps = append(resps, itemResp{Ok: false, Error: appErr.Code})
			continue
		}
		resps = append(resps, itemResp{Ok: true, Quote: q})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"items": resps})
}

func (s *Server) createQuote(ctx context.Context, req quoteRequest) (*Quote, *AppError) {
	_, span := startSpan(ctx, "Pricer.Compute")
	defer span.End()
	amount, _ := parseAmount(req.Amount)
	res, err := s.pricer.Compute(req.From, req.To, amount, req.UserTier, req.Side)
	if err != nil {
		return nil, errSpotUnavailable
	}
	span.SetAttribute("source_venue", res.SourceVenue)
	id := newQuoteID()
	now := time.Now().UTC()
	exp := now.Add(s.cfg.RateLockTTL)
	q := &Quote{
		QuoteID:      id,
		From:         req.From,
		To:           req.To,
		Amount:       req.Amount,
		Rate:         res.Rate.String(),
		SpreadBPS:    res.SpreadBPS,
		Fee:          res.Fee.String(),
		FeeCurrency:  req.From,
		Total:        res.Total.String(),
		CryptoAmount: res.CryptoAmount.String(),
		UserTier:     req.UserTier,
		Side:         req.Side,
		Status:       StatusOpen,
		SourceVenue:  res.SourceVenue,
		CreatedAt:    now,
		ExpiresAt:    exp,
		LockedRate:   res.Rate,
		SpotPrice:    res.Spot,
	}
	s.store.SaveQuote(q)
	lockPayload, _ := json.Marshal(map[string]any{
		"rate":         res.Rate.String(),
		"from":         req.From,
		"to":           req.To,
		"amount":       req.Amount,
		"expires_at":   exp.Format(time.RFC3339Nano),
		"source_venue": res.SourceVenue,
	})
	s.locks.SetNX(lockKey(id), string(lockPayload), s.cfg.RateLockTTL)
	s.audit.Append(AuditEvent{Type: "quote.issued", QuoteID: id, UserTier: req.UserTier, SourceVenue: res.SourceVenue})
	return q, nil
}

// quoteByIDHandler dispatches GET and POST /v1/quotes/{id}[/refresh].
func (s *Server) quoteByIDHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/quotes/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, r, http.StatusNotFound, "not_found", "quote id required")
		return
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "invalid quote id")
		return
	}
	if len(parts) == 2 && parts[1] == "refresh" {
		if r.Method != http.MethodPost {
			writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		s.refreshQuote(w, r, id)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.getQuote(w, r, id)
}

// getQuote handles GET /v1/quotes/:id.
// Breaking: rate/fee/total/crypto_amount are JSON strings (decimal).
func (s *Server) getQuote(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	_, span := startSpan(r.Context(), "GET /v1/quotes/:id")
	defer span.End()
	span.SetAttribute("quote_id", id.String())
	q := s.store.GetQuote(id)
	if q == nil {
		writeErrorApp(w, r, errNotFound)
		return
	}
	if q.Status == StatusExpired || (q.Status == StatusOpen && time.Now().UTC().After(q.ExpiresAt)) {
		s.store.UpdateQuote(id, func(row *Quote) { row.Status = StatusExpired })
		q.Status = StatusExpired
		writeErrorApp(w, r, errExpired)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(q.toResponse())
}

// refreshQuote handles POST /v1/quotes/:id/refresh.
// Breaking: rate/fee/total/crypto_amount are JSON strings (decimal).
func (s *Server) refreshQuote(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	_, span := startSpan(r.Context(), "POST /v1/quotes/:id/refresh")
	defer span.End()
	span.SetAttribute("quote_id", id.String())
	old := s.store.GetQuote(id)
	if old == nil {
		writeErrorApp(w, r, errNotFound)
		return
	}
	s.store.UpdateQuote(id, func(row *Quote) { row.Status = StatusCanceled })
	s.locks.Del(lockKey(id))
	s.audit.Append(AuditEvent{Type: "quote.refreshed", QuoteID: id, UserTier: old.UserTier, SourceVenue: old.SourceVenue})
	amount, _ := parseAmount(old.Amount)
	res, err := s.pricer.Compute(old.From, old.To, amount, old.UserTier, old.Side)
	if err != nil {
		writeErrorApp(w, r, errSpotUnavailable)
		return
	}
	newID := newQuoteID()
	now := time.Now().UTC()
	exp := now.Add(s.cfg.RateLockTTL)
	nq := &Quote{
		QuoteID:      newID,
		From:         old.From,
		To:           old.To,
		Amount:       old.Amount,
		Rate:         res.Rate.String(),
		SpreadBPS:    res.SpreadBPS,
		Fee:          res.Fee.String(),
		FeeCurrency:  old.From,
		Total:        res.Total.String(),
		CryptoAmount: res.CryptoAmount.String(),
		UserTier:     old.UserTier,
		Side:         old.Side,
		Status:       StatusOpen,
		SourceVenue:  res.SourceVenue,
		CreatedAt:    now,
		ExpiresAt:    exp,
		LockedRate:   res.Rate,
		SpotPrice:    res.Spot,
	}
	s.store.SaveQuote(nq)
	lockPayload, _ := json.Marshal(map[string]any{
		"rate":         res.Rate.String(),
		"from":         old.From,
		"to":           old.To,
		"amount":       old.Amount,
		"expires_at":   exp.Format(time.RFC3339Nano),
		"source_venue": res.SourceVenue,
	})
	s.locks.SetNX(lockKey(newID), string(lockPayload), s.cfg.RateLockTTL)
	s.audit.Append(AuditEvent{Type: "quote.issued", QuoteID: newID, UserTier: old.UserTier, SourceVenue: res.SourceVenue})
	span.SetAttribute("new_quote_id", newID.String())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(nq.toResponse())
}

// internalQuoteHandler dispatches POST /internal/v1/quotes/{id}/claim.
// Breaking: returned quote rate/fee/total/crypto_amount are JSON strings (decimal).
func (s *Server) internalQuoteHandler(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/internal/v1/quotes/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, r, http.StatusNotFound, "not_found", "quote id required")
		return
	}
	id, err := uuid.Parse(parts[0])
	if err != nil {
		writeError(w, r, http.StatusNotFound, "not_found", "invalid quote id")
		return
	}
	if len(parts) != 2 || parts[1] != "claim" {
		writeError(w, r, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	var req ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.ClaimedBy) == "" {
		writeError(w, r, http.StatusBadRequest, "bad_request", "claimed_by required")
		return
	}
	_, span := startSpan(r.Context(), "POST /internal/v1/quotes/:id/claim")
	defer span.End()
	span.SetAttribute("quote_id", id.String())
	res := s.claim.Claim(id, req.ClaimedBy)
	if res.Reason != "" {
		globalMetrics.claimTotal.WithLabelValues(res.Reason).Inc()
		switch res.Reason {
		case "missing":
			writeError(w, r, http.StatusNotFound, "not_found", "quote not found")
			return
		case "EXPIRED":
			writeError(w, r, http.StatusGone, "EXPIRED", "quote expired")
			return
		default:
			writeError(w, r, http.StatusConflict, res.Reason, "claim rejected: "+res.Reason)
			return
		}
	}
	globalMetrics.claimTotal.WithLabelValues("ok").Inc()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Quote.toResponse())
}

// feeSchedulesReload handles POST /internal/v1/fee-schedules/reload.
func (s *Server) feeSchedulesReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	seedFeeSchedules(s.store)
	s.pricer.ReloadIndex()
	if s.log != nil {
		s.log.Info("fee schedules reloaded", fInt("count", s.pricer.index.Len()))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "reloaded"})
}

// feeSchedulesHandler handles GET /v1/fee-schedules.
// Breaking: fee_amount, size_band_min, size_band_max are now JSON strings.
func (s *Server) feeSchedulesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	fs := s.store.FeeSchedules()
	out := make([]map[string]any, 0, len(fs))
	for _, f := range fs {
		out = append(out, map[string]any{
			"id":            f.ID,
			"user_tier":     f.UserTier,
			"asset":         f.Asset,
			"size_band_min": f.SizeBandMin.String(),
			"size_band_max": f.SizeBandMax.String(),
			"side":          f.Side,
			"spread_bps":    f.SpreadBPS,
			"fee_type":      f.FeeType,
			"fee_amount":    f.FeeAmount.String(),
			"fee_bps":       f.FeeBPS,
			"enabled":       f.Enabled,
			"updated_at":    f.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"fee_schedules": out})
}

// rateSourcesHandler handles GET /v1/rate-sources.
func (s *Server) rateSourcesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	rs := s.store.RateSources()
	out := make([]map[string]any, 0, len(rs))
	for _, src := range rs {
		out = append(out, map[string]any{
			"name":         src.Name,
			"priority":     src.Priority,
			"enabled":      src.Enabled,
			"endpoint_ref": src.EndpointRef,
			"weight":       src.Weight,
			"created_at":   src.CreatedAt.UTC().Format(time.RFC3339Nano),
			"updated_at":   src.UpdatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rate_sources": out})
}

// listQuotes handles GET /v1/quotes (list all quotes, newest first).
// Breaking: per-quote rate/fee/total/crypto_amount are JSON strings (decimal).
func (s *Server) listQuotes(w http.ResponseWriter, r *http.Request) {
	_, span := startSpan(r.Context(), "GET /v1/quotes")
	defer span.End()
	qs := s.store.ListQuotes()
	out := make([]map[string]any, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.toResponse())
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"quotes": out})
}

// auditEventsHandler handles GET /v1/audit-events.
func (s *Server) auditEventsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"events": s.audit.Events()})
}

// toResponse renders the quote for the API surface.
func (q *Quote) toResponse() map[string]any {
	resp := map[string]any{
		"quote_id":      q.QuoteID,
		"from":          q.From,
		"to":            q.To,
		"amount":        q.Amount,
		"rate":          q.Rate,
		"spread_bps":    q.SpreadBPS,
		"fee":           q.Fee,
		"fee_currency":  q.FeeCurrency,
		"total":         q.Total,
		"crypto_amount": q.CryptoAmount,
		"user_tier":     q.UserTier,
		"side":          q.Side,
		"status":        string(q.Status),
		"source_venue":  q.SourceVenue,
		"created_at":    q.CreatedAt.UTC().Format(time.RFC3339Nano),
		"expires_at":    q.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	if q.ClaimedAt != nil {
		resp["claimed_at"] = q.ClaimedAt.UTC().Format(time.RFC3339Nano)
	}
	if q.ClaimedBy != "" {
		resp["claimed_by"] = q.ClaimedBy
	}
	return resp
}

// StartSweeper launches a background goroutine that marks expired unclaimed
// quotes as expired. Returns a stop function.
func (s *Server) StartSweeper(interval time.Duration) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.sweepExpired()
			}
		}
	}()
	return func() { close(stop) }
}

func (s *Server) sweepExpired() {
	now := time.Now().UTC()
	for _, q := range s.store.ListQuotes() {
		if q.Status == StatusOpen && now.After(q.ExpiresAt) {
			s.store.UpdateQuote(q.QuoteID, func(row *Quote) { row.Status = StatusExpired })
			s.audit.Append(AuditEvent{Type: "quote.expired", QuoteID: q.QuoteID, UserTier: q.UserTier, SourceVenue: q.SourceVenue})
		}
	}
}

// startFeeScheduleRefresh launches a background goroutine that re-seeds the
// fee schedules on the given interval (Stage 3 hot-reload tick). Returns a stop
// function. In production this would re-fetch from FEE_SCHEDULE_URL; here it
// re-applies the seeded defaults to keep the in-memory index warm.
func (s *Server) startFeeScheduleRefresh(interval time.Duration) func() {
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				seedFeeSchedules(s.store)
				if s.log != nil {
					s.log.Debug("fee schedules refreshed")
				}
			}
		}
	}()
	return func() { close(stop) }
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
	}
	return buf, nil
}

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// helperServer returns a Server-backed mux with requestID middleware applied.
func helperServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(DefaultConfig())
}

func helperMux(s *Server) http.Handler {
	mux := http.NewServeMux()
	s.register(mux)
	return requestIDMiddleware(mux)
}

func doReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func parseJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON body %q: %v", rec.Body.String(), err)
	}
	return m
}

// ---------- Single quote ----------

func TestSingleQuoteSuccess(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{
		From: "USD", To: "BTC", Amount: "500.00", UserTier: "tier_2", Side: "buy",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	if id, _ := body["quote_id"].(string); !strings.HasPrefix(id, "q_") {
		t.Fatalf("bad quote_id %v", body["quote_id"])
	}
	if body["status"] != "open" {
		t.Fatalf("bad status %v", body["status"])
	}
	if body["from"] != "USD" || body["to"] != "BTC" {
		t.Fatalf("bad from/to %v/%v", body["from"], body["to"])
	}
	// Audit event emitted.
	events := s.audit.Events()
	if len(events) == 0 || events[0].Type != "quote.issued" {
		t.Fatalf("expected quote.issued audit event, got %+v", events)
	}
}

func TestSingleQuoteValidationErrors(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	cases := []struct {
		name string
		req  quoteRequest
		code string
	}{
		{"bad from", quoteRequest{From: "us", To: "BTC", Amount: "10", UserTier: "tier_1", Side: "buy"}, "invalid_currency"},
		{"bad to", quoteRequest{From: "USD", To: "bitcoin", Amount: "10", UserTier: "tier_1", Side: "buy"}, "invalid_currency"},
		{"bad amount zero", quoteRequest{From: "USD", To: "BTC", Amount: "0", UserTier: "tier_1", Side: "buy"}, "invalid_amount"},
		{"bad amount negative", quoteRequest{From: "USD", To: "BTC", Amount: "-5", UserTier: "tier_1", Side: "buy"}, "invalid_amount"},
		{"bad tier", quoteRequest{From: "USD", To: "BTC", Amount: "10", UserTier: "tier_9", Side: "buy"}, "invalid_tier"},
		{"bad side", quoteRequest{From: "USD", To: "BTC", Amount: "10", UserTier: "tier_1", Side: "trade"}, "invalid_side"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doReq(t, h, http.MethodPost, "/v1/quotes", c.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := parseJSON(t, rec)
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil || errObj["code"] != c.code {
				t.Fatalf("expected error.code=%s got %v", c.code, body)
			}
		})
	}
}

// ---------- Bulk quote ----------

func TestBulkQuoteSuccess(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	body := bulkRequest{Items: []quoteRequest{
		{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"},
		{From: "USD", To: "ETH", Amount: "50", UserTier: "tier_2", Side: "buy"},
	}}
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items got %d", len(items))
	}
}

func TestBulkQuoteExceedsMaxItems(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	items := make([]quoteRequest, s.cfg.BulkQuoteMaxItems+1)
	for i := range items {
		items[i] = quoteRequest{From: "USD", To: "BTC", Amount: "10", UserTier: "tier_1", Side: "buy"}
	}
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", bulkRequest{Items: items})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "too_many_items" {
		t.Fatalf("expected too_many_items got %v", body)
	}
}

func TestBulkQuotePerItemError(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	body := bulkRequest{Items: []quoteRequest{
		{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"},
		{From: "usd", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"}, // bad
	}}
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("expected 2 items got %d", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["ok"] != true {
		t.Fatalf("first should be ok, got %v", first)
	}
	second, _ := items[1].(map[string]any)
	if second["ok"] != false || second["error"] != "invalid_currency" {
		t.Fatalf("second should fail, got %v", second)
	}
}

// ---------- GET quote ----------

func TestGetQuoteSuccess(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	rec2 := doReq(t, h, http.MethodGet, "/v1/quotes/"+id, nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if parseJSON(t, rec2)["quote_id"] != id {
		t.Fatalf("mismatched quote_id")
	}
}

func TestGetQuoteNotFound(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodGet, "/v1/quotes/q_doesnotexist", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetQuoteExpired(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	// Create a quote with a very short TTL by overriding cfg.
	s.cfg.RateLockTTL = 50 * time.Millisecond
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	time.Sleep(80 * time.Millisecond)
	rec2 := doReq(t, h, http.MethodGet, "/v1/quotes/"+id, nil)
	if rec2.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body := parseJSON(t, rec2)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "expired" {
		t.Fatalf("expected expired, got %v", body)
	}
}

// ---------- Refresh ----------

func TestQuoteRefresh(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	rec2 := doReq(t, h, http.MethodPost, "/v1/quotes/"+id+"/refresh", nil)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	newID := parseJSON(t, rec2)["quote_id"].(string)
	if newID == id {
		t.Fatalf("refresh should produce a new id")
	}
	// Old should be canceled.
	old := s.store.GetQuote(id)
	if old.Status != StatusCanceled {
		t.Fatalf("expected old canceled, got %s", old.Status)
	}
	// New is open.
	if nq := s.store.GetQuote(newID); nq.Status != StatusOpen {
		t.Fatalf("expected new open, got %s", nq.Status)
	}
}

// ---------- Claim ----------

func TestClaimSuccess(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	rec2 := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body := parseJSON(t, rec2)
	if body["status"] != "claimed" {
		t.Fatalf("expected claimed, got %v", body["status"])
	}
}

func TestClaimExpired(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	s.cfg.RateLockTTL = 50 * time.Millisecond
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	time.Sleep(80 * time.Millisecond)
	rec2 := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec2.Code != http.StatusGone {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestClaimMissing(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/internal/v1/quotes/q_unknown/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClaimAlreadyClaimed(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	rec1 := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec1.Code != http.StatusOK {
		t.Fatalf("first claim status=%d body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator2"})
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second claim status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body := parseJSON(t, rec2)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "already_claimed" {
		t.Fatalf("expected already_claimed, got %v", body)
	}
}

func TestClaimSlippageExceeded(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	s.cfg.SlippageToleranceBPS = 10
	s.claim.slippageToleranceBPS = 10
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	// Move the spot significantly: update the BTC mid price up by 20%.
	s.spot.Update(Rate{From: "USD", To: "BTC", Bid: 78000, Ask: 80000, Mid: 79000, SourceVenue: "kraken"})
	rec2 := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec2.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body := parseJSON(t, rec2)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "slippage_exceeded" {
		t.Fatalf("expected slippage_exceeded, got %v", body)
	}
	// Audit slippage rejected emitted.
	found := false
	for _, e := range s.audit.Events() {
		if e.Type == "quote.slippage_rejected" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected slippage_rejected audit event")
	}
}

// ---------- Fee schedule ----------

func TestFeeScheduleMatching(t *testing.T) {
	store := NewStore()
	now := time.Now().UTC()
	store.SetFeeSchedules([]FeeSchedule{
		{UserTier: "tier_2", Asset: "BTC", SizeBandMin: 0, SizeBandMax: 1000, Side: "buy", SpreadBPS: 70, FeeType: "bps", FeeBPS: 40, Enabled: true, UpdatedAt: now},
		{UserTier: "tier_2", Asset: "BTC", SizeBandMin: 1000, SizeBandMax: 10000, Side: "buy", SpreadBPS: 50, FeeType: "bps", FeeBPS: 20, Enabled: true, UpdatedAt: now},
	})
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	res, err := p.Compute("USD", "BTC", 500, "tier_2", "buy")
	if err != nil {
		t.Fatal(err)
	}
	if res.SpreadBPS != 70 {
		t.Fatalf("expected 70 got %d", res.SpreadBPS)
	}
	res, _ = p.Compute("USD", "BTC", 5000, "tier_2", "buy")
	if res.SpreadBPS != 50 {
		t.Fatalf("expected 50 got %d", res.SpreadBPS)
	}
}

func TestFeeScheduleDefaultFallback(t *testing.T) {
	store := NewStore()
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	res, err := p.Compute("USD", "BTC", 500, "tier_3", "buy")
	if err != nil {
		t.Fatal(err)
	}
	if res.SpreadBPS != 100 {
		t.Fatalf("expected default 100 got %d", res.SpreadBPS)
	}
}

func TestFeeScheduleHotReload(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	// Replace schedules with a single high-spread one.
	s.store.SetFeeSchedules([]FeeSchedule{
		{UserTier: "tier_1", Asset: "BTC", SizeBandMin: 0, SizeBandMax: 10000, Side: "buy", SpreadBPS: 200, FeeType: "flat", FeeAmount: 1, Enabled: true},
	})
	rec := doReq(t, h, http.MethodPost, "/internal/v1/fee-schedules/reload", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// After reload, schedules should be back to seeded defaults.
	fs := s.store.FeeSchedules()
	if len(fs) == 0 || fs[0].UserTier != "tier_1" {
		t.Fatalf("reload did not restore schedules: %+v", fs)
	}
}

// ---------- Spot cache ----------

func TestSpotCacheHit(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	r, err := svc.Get("USD", "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if r.Mid != 65000 {
		t.Fatalf("expected mid 65000 got %f", r.Mid)
	}
}

func TestSpotCacheStaleTriggersReseed(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	svc.SetMaxStaleAge(20 * time.Millisecond)
	_, err := svc.Get("USD", "BTC")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	r, err := svc.Get("USD", "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if r.Mid != 65000 {
		t.Fatalf("reseed failed, got %f", r.Mid)
	}
}

func TestSpotCacheLastGoodFallback(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	// Inject a rate for an unknown pair via Update.
	svc.Update(Rate{From: "USD", To: "DOGE", Bid: 0.1, Ask: 0.11, Mid: 0.105, SourceVenue: "kraken"})
	// Now evict by clearing cache indirectly via short TTL & long sleep.
	svc.SetMaxStaleAge(20 * time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	r, err := svc.Get("USD", "DOGE")
	if err != nil {
		t.Fatal(err)
	}
	// last-good served, but stale flag set.
	if !r.Stale {
		t.Fatalf("expected stale flag, got %+v", r)
	}
	if r.Mid != 0.105 {
		t.Fatalf("expected last-good 0.105, got %f", r.Mid)
	}
}

// ---------- Lock store ----------

func TestLockStoreSetNXGetDel(t *testing.T) {
	l := NewLockStore()
	if !l.SetNX("k1", "v1", time.Second) {
		t.Fatal("expected SetNX true")
	}
	if l.SetNX("k1", "v2", time.Second) {
		t.Fatal("expected SetNX false on duplicate")
	}
	v, ok := l.Get("k1")
	if !ok || v != "v1" {
		t.Fatalf("Get got %q %v", v, ok)
	}
	if !l.Del("k1") {
		t.Fatal("Del should return true")
	}
	if _, ok := l.Get("k1"); ok {
		t.Fatal("expected missing after Del")
	}
}

func TestLockStoreTTLExpiry(t *testing.T) {
	l := NewLockStore()
	l.SetNX("k1", "v1", 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if _, ok := l.Get("k1"); ok {
		t.Fatal("expected expiry")
	}
}

func TestLockStoreClaim(t *testing.T) {
	l := NewLockStore()
	l.SetNX("k1", "v1", time.Second)
	v, ok := l.Claim("k1")
	if !ok || v != "v1" {
		t.Fatalf("Claim got %q %v", v, ok)
	}
	if _, ok := l.Get("k1"); ok {
		t.Fatal("expected missing after Claim")
	}
	if _, ok := l.Claim("k1"); ok {
		t.Fatal("second Claim should fail")
	}
}

// ---------- Sweeper ----------

func TestSweeperMarksExpired(t *testing.T) {
	s := helperServer(t)
	s.cfg.RateLockTTL = 30 * time.Millisecond
	q, _ := s.createQuote(quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	stop := s.StartSweeper(20 * time.Millisecond)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	updated := s.store.GetQuote(q.QuoteID)
	if updated.Status != StatusExpired {
		t.Fatalf("expected expired, got %s", updated.Status)
	}
}

// ---------- /healthz & /readyz ----------

func TestHealthzEndpoint(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := parseJSON(t, rec)
	if body["status"] != "ok" {
		t.Fatalf("expected ok got %v", body["status"])
	}
}

func TestReadyzEndpoint(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	if body["status"] != "ready" {
		t.Fatalf("expected ready got %v", body["status"])
	}
}

func TestReadyzNotReady(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	s.spot.ready = false
	rec := doReq(t, h, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---------- Audit events ----------

func TestAuditEventsEmitted(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec2 := doReq(t, h, http.MethodGet, "/v1/audit-events", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	m := parseJSON(t, rec2)
	events, _ := m["events"].([]any)
	if len(events) == 0 {
		t.Fatal("expected audit events")
	}
	first, _ := events[0].(map[string]any)
	if first["type"] != "quote.issued" {
		t.Fatalf("expected quote.issued got %v", first["type"])
	}
}

// ---------- Error envelope ----------

func TestErrorEnvelopeShape(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "xx", To: "BTC", Amount: "1", UserTier: "tier_1", Side: "buy"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	body := parseJSON(t, rec)
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %v", body)
	}
	for _, k := range []string{"code", "message", "request_id"} {
		if _, has := errObj[k]; !has {
			t.Fatalf("error.%s missing: %v", k, errObj)
		}
	}
}

// ---------- Sell side ----------

func TestSellQuote(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "0.1", UserTier: "tier_1", Side: "sell"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	if body["side"] != "sell" {
		t.Fatalf("expected sell got %v", body["side"])
	}
}

// ---------- Venue circuit breaker ----------

func TestVenueCircuitBreaker(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	for i := 0; i < 3; i++ {
		svc.RecordVenueError("kraken")
	}
	if !svc.IsVenueDown("kraken") {
		t.Fatal("expected kraken to be down")
	}
	svc.RecordVenueSuccess("kraken")
	if svc.IsVenueDown("kraken") {
		t.Fatal("expected kraken to be up after success")
	}
}

func TestNewMuxRoutingStillWorks(t *testing.T) {
	mux := newMux()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status=%d", rec.Code)
	}
}
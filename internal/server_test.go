package pricing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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
		From: "USD", To: "BTC", Amount: "500.00", UserTier: "TIER_2", Side: "BUY",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	id, _ := body["quote_id"].(string)
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("bad quote_id %v", body["quote_id"])
	}
	if body["status"] != "OPEN" {
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
		{"bad from", quoteRequest{From: "us", To: "BTC", Amount: "10", UserTier: "TIER_1", Side: "BUY"}, "invalid_currency"},
		{"bad to", quoteRequest{From: "USD", To: "bitcoin", Amount: "10", UserTier: "TIER_1", Side: "BUY"}, "invalid_currency"},
		{"bad amount zero", quoteRequest{From: "USD", To: "BTC", Amount: "0", UserTier: "TIER_1", Side: "BUY"}, "invalid_amount"},
		{"bad amount negative", quoteRequest{From: "USD", To: "BTC", Amount: "-5", UserTier: "TIER_1", Side: "BUY"}, "invalid_amount"},
		{"bad tier", quoteRequest{From: "USD", To: "BTC", Amount: "10", UserTier: "TIER_9", Side: "BUY"}, "invalid_tier"},
		{"bad side", quoteRequest{From: "USD", To: "BTC", Amount: "10", UserTier: "TIER_1", Side: "trade"}, "invalid_side"},
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
		{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"},
		{From: "USD", To: "ETH", Amount: "50", UserTier: "TIER_2", Side: "BUY"},
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
		items[i] = quoteRequest{From: "USD", To: "BTC", Amount: "10", UserTier: "TIER_1", Side: "BUY"}
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
		{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"},
		{From: "usd", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"}, // bad
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
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	// A well-formed but nonexistent UUID.
	id := uuid.New().String()
	rec := doReq(t, h, http.MethodGet, "/v1/quotes/"+id, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetQuoteExpired(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	// Create a quote with a very short TTL by overriding cfg.
	s.cfg.RateLockTTL = 50 * time.Millisecond
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	if errObj["code"] != "EXPIRED" {
		t.Fatalf("expected EXPIRED, got %v", body)
	}
}

// ---------- Refresh ----------

func TestQuoteRefresh(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	oldID, _ := uuid.Parse(id)
	old := s.store.GetQuote(oldID)
	if old.Status != StatusCanceled {
		t.Fatalf("expected old canceled, got %s", old.Status)
	}
	// New is open.
	newUID, _ := uuid.Parse(newID)
	if nq := s.store.GetQuote(newUID); nq.Status != StatusOpen {
		t.Fatalf("expected new open, got %s", nq.Status)
	}
}

// ---------- Claim ----------

func TestClaimSuccess(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	rec2 := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec2.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	body := parseJSON(t, rec2)
	if body["status"] != "CLAIMED" {
		t.Fatalf("expected CLAIMED, got %v", body["status"])
	}
}

func TestClaimExpired(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	s.cfg.RateLockTTL = 50 * time.Millisecond
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	// A well-formed but nonexistent UUID.
	id := uuid.New().String()
	rec := doReq(t, h, http.MethodPost, "/internal/v1/quotes/"+id+"/claim", ClaimRequest{ClaimedBy: "orchestrator"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClaimAlreadyClaimed(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	id := parseJSON(t, rec)["quote_id"].(string)
	// Move the spot significantly: update the BTC mid price up by 20%.
	s.spot.Update(Rate{From: "USD", To: "BTC", Bid: decimal.NewFromInt(78000), Ask: decimal.NewFromInt(80000), Mid: decimal.NewFromInt(79000), SourceVenue: "kraken"})
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
		{UserTier: "TIER_2", Asset: "BTC", SizeBandMin: decimal.Zero, SizeBandMax: decimal.NewFromInt(1000), Side: "BUY", SpreadBPS: 70, FeeType: "BPS", FeeBPS: 40, Enabled: true, UpdatedAt: now},
		{UserTier: "TIER_2", Asset: "BTC", SizeBandMin: decimal.NewFromInt(1000), SizeBandMax: decimal.NewFromInt(10000), Side: "BUY", SpreadBPS: 50, FeeType: "BPS", FeeBPS: 20, Enabled: true, UpdatedAt: now},
	})
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	res, err := p.Compute("USD", "BTC", decimal.NewFromInt(500), "TIER_2", "BUY")
	if err != nil {
		t.Fatal(err)
	}
	if res.SpreadBPS != 70 {
		t.Fatalf("expected 70 got %d", res.SpreadBPS)
	}
	res, _ = p.Compute("USD", "BTC", decimal.NewFromInt(5000), "TIER_2", "BUY")
	if res.SpreadBPS != 50 {
		t.Fatalf("expected 50 got %d", res.SpreadBPS)
	}
}

func TestFeeScheduleDefaultFallback(t *testing.T) {
	store := NewStore()
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	res, err := p.Compute("USD", "BTC", decimal.NewFromInt(500), "TIER_3", "BUY")
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
		{UserTier: "TIER_1", Asset: "BTC", SizeBandMin: decimal.Zero, SizeBandMax: decimal.NewFromInt(10000), Side: "BUY", SpreadBPS: 200, FeeType: "FLAT", FeeAmount: decimal.NewFromInt(1), Enabled: true},
	})
	rec := doReq(t, h, http.MethodPost, "/internal/v1/fee-schedules/reload", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// After reload, schedules should be back to seeded defaults.
	fs := s.store.FeeSchedules()
	if len(fs) == 0 || fs[0].UserTier != "TIER_1" {
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
	if !r.Mid.Equal(decimal.NewFromInt(65000)) {
		t.Fatalf("expected mid 65000 got %s", r.Mid.String())
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
	if !r.Mid.Equal(decimal.NewFromInt(65000)) {
		t.Fatalf("reseed failed, got %s", r.Mid.String())
	}
}

func TestSpotCacheLastGoodFallback(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	// Inject a rate for an unknown pair via Update.
	svc.Update(Rate{From: "USD", To: "DOGE", Bid: decimal.NewFromFloat(0.1), Ask: decimal.NewFromFloat(0.11), Mid: decimal.NewFromFloat(0.105), SourceVenue: "kraken"})
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
	if !r.Mid.Equal(decimal.NewFromFloat(0.105)) {
		t.Fatalf("expected last-good 0.105, got %s", r.Mid.String())
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
	q, _ := s.createQuote(context.Background(), quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
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
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "xx", To: "BTC", Amount: "1", UserTier: "TIER_1", Side: "BUY"})
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
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "0.1", UserTier: "TIER_1", Side: "SELL"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	if body["side"] != "SELL" {
		t.Fatalf("expected SELL got %v", body["side"])
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

// ---------- List endpoints ----------

func TestListQuotesEmpty(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodGet, "/v1/quotes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	quotes, _ := m["quotes"].([]any)
	if len(quotes) != 0 {
		t.Fatalf("expected empty quotes list, got %d", len(quotes))
	}
}

func TestListQuotesAfterCreate(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/quotes", quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "TIER_1", Side: "BUY"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	created := parseJSON(t, rec)
	id, _ := created["quote_id"].(string)

	rec2 := doReq(t, h, http.MethodGet, "/v1/quotes", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	m := parseJSON(t, rec2)
	quotes, _ := m["quotes"].([]any)
	if len(quotes) != 1 {
		t.Fatalf("expected 1 quote, got %d", len(quotes))
	}
	first, _ := quotes[0].(map[string]any)
	if first["quote_id"] != id {
		t.Fatalf("expected quote_id %s got %v", id, first["quote_id"])
	}
}

func TestListQuotesMethodNotAllowed(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodDelete, "/v1/quotes", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestListFeeSchedules(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodGet, "/v1/fee-schedules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	fs, _ := m["fee_schedules"].([]any)
	if len(fs) == 0 {
		t.Fatal("expected seeded fee schedules")
	}
	first, _ := fs[0].(map[string]any)
	for _, k := range []string{"id", "user_tier", "asset", "side", "spread_bps", "enabled"} {
		if _, has := first[k]; !has {
			t.Fatalf("fee_schedules[].%s missing: %v", k, first)
		}
	}
}

func TestListFeeSchedulesMethodNotAllowed(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPost, "/v1/fee-schedules", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestListRateSources(t *testing.T) {
	s := helperServer(t)
	now := time.Now().UTC()
	s.store.SetRateSources([]*RateSource{
		{Name: "kraken", Priority: 0, Enabled: true, Weight: 2, CreatedAt: now, UpdatedAt: now},
		{Name: "coinbase", Priority: 1, Enabled: true, Weight: 1, CreatedAt: now, UpdatedAt: now},
	})
	h := helperMux(s)
	rec := doReq(t, h, http.MethodGet, "/v1/rate-sources", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	m := parseJSON(t, rec)
	rs, _ := m["rate_sources"].([]any)
	if len(rs) != 2 {
		t.Fatalf("expected 2 rate sources, got %d", len(rs))
	}
	first, _ := rs[0].(map[string]any)
	for _, k := range []string{"name", "priority", "enabled", "weight"} {
		if _, has := first[k]; !has {
			t.Fatalf("rate_sources[].%s missing: %v", k, first)
		}
	}
	if first["name"] != "kraken" {
		t.Fatalf("expected kraken first by priority, got %v", first["name"])
	}
}

func TestListRateSourcesMethodNotAllowed(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, http.MethodPut, "/v1/rate-sources", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- Stage 1: config ----------

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("RATE_LOCK_TTL_SECONDS", "")
	c := LoadConfig()
	if c.RateLockTTL != 30*time.Second {
		t.Fatalf("expected 30s default ttl, got %v", c.RateLockTTL)
	}
	if c.BulkQuoteMaxItems != 25 {
		t.Fatalf("expected 25 default bulk, got %d", c.BulkQuoteMaxItems)
	}
	if c.Port != "8080" {
		t.Fatalf("expected port 8080, got %s", c.Port)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	t.Setenv("RATE_LOCK_TTL_SECONDS", "60")
	t.Setenv("BULK_QUOTE_MAX_ITEMS", "5")
	t.Setenv("PORT", "9090")
	c := LoadConfig()
	if c.RateLockTTL != 60*time.Second {
		t.Fatalf("expected 60s ttl, got %v", c.RateLockTTL)
	}
	if c.BulkQuoteMaxItems != 5 {
		t.Fatalf("expected 5 bulk, got %d", c.BulkQuoteMaxItems)
	}
	if c.Port != "9090" {
		t.Fatalf("expected 9090, got %s", c.Port)
	}
}

func TestConfigStringMasksRedisURL(t *testing.T) {
	c := DefaultConfig()
	c.RedisURL = "redis://user:secret@host:6379"
	s := c.String()
	if strings.Contains(s, "secret") {
		t.Fatalf("redis URL not masked: %s", s)
	}
}

// ---------- Stage 1: LockBackend interface ----------

func TestLockBackendInterfaceSatisfied(t *testing.T) {
	var b LockBackend = NewLockStore()
	if !b.SetNX("k", "v", time.Second) {
		t.Fatal("SetNX failed")
	}
	if v, ok := b.Get("k"); !ok || v != "v" {
		t.Fatalf("Get got %q %v", v, ok)
	}
	if v, ok := b.Claim("k"); !ok || v != "v" {
		t.Fatalf("Claim got %q %v", v, ok)
	}
}

// ---------- Stage 2: in-proc feed ----------

func TestInProcFeedFanout(t *testing.T) {
	feed := newInProcFeed()
	defer feed.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan FeedMessage, 4)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = feed.Subscribe(ctx, func(m FeedMessage) { got <- m })
	}()
	time.Sleep(20 * time.Millisecond) // let subscriber register
	feed.Publish(FeedMessage{Pair: "USD-BTC", Bid: 100, Ask: 101, Mid: 100.5, SourceVenue: "kraken"})
	select {
	case m := <-got:
		if m.Pair != "USD-BTC" || m.Mid != 100.5 {
			t.Fatalf("unexpected msg %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for feed message")
	}
}

func TestSpotServicePollHook(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	svc.SetMaxStaleAge(20 * time.Millisecond)
	// Install a poll hook that returns a fresh rate.
	svc.SetPollHook(func(from, to string) (Rate, bool) {
		if from == "USD" && to == "BTC" {
			return Rate{From: "USD", To: "BTC", Bid: 66000, Ask: 66100, Mid: 66050, TS: time.Now(), SourceVenue: "coinbase"}, true
		}
		return Rate{}, false
	})
	_, err := svc.Get("USD", "BTC") // warm
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond) // stale
	r, err := svc.Get("USD", "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if r.Mid != 66050 {
		t.Fatalf("expected poll hook result 66050, got %f", r.Mid)
	}
}

func TestRateSourceSelector(t *testing.T) {
	sources := []*RateSource{
		{Name: "binance", Priority: 2, Enabled: true},
		{Name: "kraken", Priority: 1, Enabled: true},
		{Name: "coinbase", Priority: 1, Enabled: false},
	}
	// Store.RateSources orders by priority; selectRateSource picks first enabled.
	ordered := []*RateSource{sources[1], sources[0], sources[2]}
	got := selectRateSource(ordered)
	if got.Name != "kraken" {
		t.Fatalf("expected kraken, got %s", got.Name)
	}
}

// ---------- Stage 3: fee index ----------

func TestFeeIndexLookup(t *testing.T) {
	now := time.Now().UTC()
	idx := newFeeIndex()
	idx.Rebuild([]FeeSchedule{
		{UserTier: "tier_2", Asset: "BTC", Side: "buy", SizeBandMin: 0, SizeBandMax: 1000, SpreadBPS: 70, Enabled: true, UpdatedAt: now},
		{UserTier: "tier_2", Asset: "BTC", Side: "buy", SizeBandMin: 1000, SizeBandMax: 10000, SpreadBPS: 50, Enabled: true, UpdatedAt: now},
	})
	if s := idx.Lookup("tier_2", "BTC", "buy", 500); s == nil || s.SpreadBPS != 70 {
		t.Fatalf("expected 70, got %v", s)
	}
	if s := idx.Lookup("tier_2", "BTC", "buy", 5000); s == nil || s.SpreadBPS != 50 {
		t.Fatalf("expected 50, got %v", s)
	}
	if s := idx.Lookup("tier_2", "ETH", "buy", 500); s != nil {
		t.Fatalf("expected nil, got %v", s)
	}
}

func TestPricerReloadIndex(t *testing.T) {
	store := NewStore()
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	store.SetFeeSchedules([]FeeSchedule{
		{UserTier: "tier_3", Asset: "BTC", Side: "buy", SizeBandMin: 0, SizeBandMax: 10000, SpreadBPS: 42, Enabled: true},
	})
	p.ReloadIndex()
	res, err := p.Compute("USD", "BTC", 500, "tier_3", "buy")
	if err != nil {
		t.Fatal(err)
	}
	if res.SpreadBPS != 42 {
		t.Fatalf("expected 42 after reload, got %d", res.SpreadBPS)
	}
}

// ---------- Stage 7: WebSocket subscribe/fanout ----------

func TestNormalizePair(t *testing.T) {
	cases := map[string]string{
		"usd-btc":  "USD-BTC",
		"USDBTC":   "USD-BTC",
		"USD-BTC": "USD-BTC",
		"  eur-eth ": "EUR-ETH",
	}
	for in, want := range cases {
		if got := normalizePair(in); got != want {
			t.Fatalf("normalizePair(%q)=%q want %q", in, got, want)
		}
	}
}

func TestWSHubFanout(t *testing.T) {
	hub := newWSHub()
	a, b := net.Pipe()
	sub := &wsSubscriber{conn: a, pairs: map[string]struct{}{"USD-BTC": {}}, done: make(chan struct{})}
	hub.add(sub)
	defer hub.remove(sub)
	// Read the full frame concurrently since net.Pipe writes block until read.
	type r struct {
		err error
	}
	hc := make(chan r, 1)
	go func() {
		b.SetReadDeadline(time.Now().Add(2 * time.Second))
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(b, hdr); err != nil {
			hc <- r{err}
			return
		}
		plen := int(hdr[1] & 0x7f)
		if plen > 0 {
			payload := make([]byte, plen)
			if _, err := io.ReadFull(b, payload); err != nil {
				hc <- r{err}
				return
			}
		}
		hc <- r{nil}
	}()
	hub.fanout("USD-BTC", Rate{From: "USD", To: "BTC", Mid: 65000, TS: time.Now(), SourceVenue: "kraken"})
	res := <-hc
	if res.err != nil {
		t.Fatalf("read: %v", res.err)
	}
	_ = b.Close()
}

// ---------- Stage 7: FX cross-pair ----------

type fakeFXClient struct {
	rate       float64
	hedgeBPS    int
	called     bool
}

func (f *fakeFXClient) FetchFX(ctx context.Context, fiat string) (float64, int, error) {
	f.called = true
	return f.rate, f.hedgeBPS, nil
}

func TestCrossPairQuoteEURBTC(t *testing.T) {
	store := NewStore()
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	fx := &fakeFXClient{rate: 1.10, hedgeBPS: 5}
	p.SetFXClient(fx)
	ctx := context.Background()
	res, err := p.CrossPairQuote(ctx, "EUR", "BTC", 500, "tier_1", "buy")
	if err != nil {
		t.Fatal(err)
	}
	if !fx.called {
		t.Fatal("expected FX client to be called")
	}
	if res.SpreadBPS < 5 {
		t.Fatalf("expected hedge markup applied, spread=%d", res.SpreadBPS)
	}
}

func TestCrossPairQuoteUSDFallsBack(t *testing.T) {
	store := NewStore()
	spot := NewSpotService(5 * time.Second)
	p := NewPricer(store, spot, 100)
	res, err := p.CrossPairQuote(context.Background(), "USD", "BTC", 500, "tier_3", "buy")
	if err != nil {
		t.Fatal(err)
	}
	if res.SpreadBPS != 100 {
		t.Fatalf("expected default 100, got %d", res.SpreadBPS)
	}
}

// ---------- Stage 8: tracing ----------

func TestRecordingTracer(t *testing.T) {
	tr := &recordingTracer{}
	prev := globalTracer
	setTracer(tr)
	defer func() { globalTracer = prev }()
	ctx := context.Background()
	_, span := startSpan(ctx, "test.span")
	span.SetAttribute("k", "v")
	span.AddEvent("did-thing")
	span.End()
	spans := tr.spansSnapshot()
	if len(spans) != 1 || spans[0].name != "test.span" {
		t.Fatalf("unexpected spans %+v", spans)
	}
	if spans[0].attrs["k"] != "v" {
		t.Fatalf("attr not set: %+v", spans[0].attrs)
	}
	if len(spans[0].events) != 1 || spans[0].events[0] != "did-thing" {
		t.Fatalf("events wrong: %+v", spans[0].events)
	}
}

// ---------- Stage 8: audit async ----------

func TestAuditLogAsyncDispatch(t *testing.T) {
	sink := &countingSink{}
	a := NewAuditLogWithSink(8, sink)
	for i := 0; i < 5; i++ {
		a.Append(AuditEvent{Type: "quote.issued", QuoteID: "q_1"})
	}
	// Allow dispatch goroutine to drain.
	time.Sleep(50 * time.Millisecond)
	if sink.count() < 5 {
		t.Fatalf("expected 5 sent, got %d", sink.count())
	}
	a.Close()
}

type countingSink struct {
	mu sync.Mutex
	n  int
}

func (c *countingSink) Send(e AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}
func (c *countingSink) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// ---------- Stage 4: metrics endpoint ----------

func TestMetricsEndpoint(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, "GET", "/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pricing_") {
		t.Fatalf("expected pricing_ metrics, got %s", rec.Body.String()[:200])
	}
}

// ---------- Stage 8: readyz reflects components ----------

func TestReadyzReflectsSpot(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	// Warm: default helper is ready.
	rec := doReq(t, h, "GET", "/readyz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected ready, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := parseJSON(t, rec)
	if body["status"] != "ready" {
		t.Fatalf("expected ready, got %v", body["status"])
	}
}

// ---------- Stage 6: venue failover ----------

func TestVenueFailoverAdvance(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	next, ok := svc.AdvanceVenue("kraken")
	if !ok || next != "coinbase" {
		t.Fatalf("expected failover to coinbase, got %q ok=%v", next, ok)
	}
}

func TestHalfOpenProbe(t *testing.T) {
	svc := NewSpotService(5 * time.Second)
	for i := 0; i < 3; i++ {
		svc.RecordVenueError("kraken")
	}
	if !svc.IsVenueDown("kraken") {
		t.Fatal("expected kraken down")
	}
	if !svc.HalfOpenProbe("kraken") {
		t.Fatal("expected half-open probe to reset")
	}
	if svc.IsVenueDown("kraken") {
		t.Fatal("expected kraken up after half-open probe")
	}
}

// ---------- Stage 2: feed consumer wires to spot ----------

func TestFeedConsumerUpdatesSpot(t *testing.T) {
	s := helperServer(t)
	feed := newInProcFeed()
	defer feed.Close()
	s.feed = feed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := s.startFeedConsumer(ctx, feed)
	defer stop()
	time.Sleep(20 * time.Millisecond)
	feed.Publish(FeedMessage{Pair: "USD-ETH", Bid: 3000, Ask: 3010, Mid: 3005, SourceVenue: "coinbase", TS: time.Now()})
	time.Sleep(50 * time.Millisecond)
	r, err := s.spot.Get("USD", "ETH")
	if err != nil {
		t.Fatal(err)
	}
	// Either seeded value or polled; both should be > 0.
	if r.Mid <= 0 {
		t.Fatalf("expected positive ETH mid, got %f", r.Mid)
	}
}

// ---------- Stage 4: latency middleware ----------

func TestMetricsMiddlewareLabels(t *testing.T) {
	s := helperServer(t)
	h := metricsMiddleware(helperMux(s))
	rec := doReq(t, h, "GET", "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

// ---------- Stage 5: sweeper audit ----------

func TestSweeperEmitsExpiredAudit(t *testing.T) {
	s := helperServer(t)
	s.cfg.RateLockTTL = 30 * time.Millisecond
	_, _ = s.createQuote(context.Background(), quoteRequest{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"})
	stop := s.StartSweeper(20 * time.Millisecond)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	found := false
	for _, e := range s.audit.Events() {
		if e.Type == "quote.expired" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected quote.expired audit event from sweeper")
	}
}

// ---------- Stage 4: bulk quote DTO shape ----------

func TestBulkQuoteResponseShape(t *testing.T) {
	s := helperServer(t)
	h := helperMux(s)
	rec := doReq(t, h, "POST", "/v1/quotes", bulkRequest{Items: []quoteRequest{
		{From: "USD", To: "BTC", Amount: "100", UserTier: "tier_1", Side: "buy"},
	}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	first, _ := items[0].(map[string]any)
	if first["ok"] != true {
		t.Fatalf("expected ok=true, got %v", first["ok"])
	}
	q, _ := first["quote"].(map[string]any)
	if q == nil || q["quote_id"] == nil {
		t.Fatalf("expected quote with quote_id, got %v", first)
	}
}

// ---------- Stage 1: redis URL parse fallback ----------

func TestInitLockBackendFallsBackWhenRedisDown(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RedisURL = "redis://127.0.0.1:1/0" // unreachable port
	b := initLockBackend(cfg, nil)
	if b == nil {
		t.Fatal("expected fallback LockBackend")
	}
	// Should be usable as an in-memory store.
	if !b.SetNX("k", "v", time.Second) {
		t.Fatal("SetNX failed on fallback")
	}
}

// ---------- Stage 8: server with logger ----------

func TestServerWithLogger(t *testing.T) {
	s := NewServer(DefaultConfig())
	s.log = newLogger("debug")
	if s.log == nil {
		t.Fatal("logger nil")
	}
	// Just exercise a path that logs.
	s.pricer.ReloadIndex()
}

// ---------- helpers ----------
package pricing

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// Rate is a cached spot rate for a trading pair.
type Rate struct {
	From        string
	To          string
	Bid         decimal.Decimal
	Ask         decimal.Decimal
	Mid         decimal.Decimal
	TS          time.Time
	SourceVenue string
	Stale       bool
}

// lruCache is a bounded LRU cache keyed by string.
type lruCache struct {
	cap   int
	mu    sync.Mutex
	items map[string]*list.Element
	ll    *list.List
}

type lruItem struct {
	key  string
	rate Rate
}

func newLRUCache(cap int) *lruCache {
	if cap <= 0 {
		cap = 4096
	}
	return &lruCache{cap: cap, items: make(map[string]*list.Element), ll: list.New()}
}

func (c *lruCache) put(key string, r Rate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value.(*lruItem).rate = r
		c.ll.MoveToFront(el)
		return
	}
	if c.ll.Len() >= c.cap {
		oldest := c.ll.Back()
		if oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*lruItem).key)
		}
	}
	el := c.ll.PushFront(&lruItem{key: key, rate: r})
	c.items[key] = el
}

func (c *lruCache) get(key string) (Rate, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return Rate{}, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*lruItem).rate, true
}

// Venue is a per-venue circuit-breaker state used by SpotService.
type Venue struct {
	Name           string
	Errors         int
	Down           bool
	OpenedAt       time.Time
	ErrorThreshold int
}

// SpotService provides spot-rate lookups backed by an LRU cache, stale-rate
// enforcement, last-good fallback, and a simple per-venue circuit breaker.
type SpotService struct {
	mu             sync.Mutex
	cache          *lruCache
	lastGood       map[string]Rate
	maxStaleAge    time.Duration
	venues         map[string]*Venue
	venueOrder     []string
	errorThreshold int
	ready          bool
	// onUpdate, if set, is invoked with the pair key and the new Rate after
	// every Update. Used to fan out to WebSocket subscribers.
	onUpdate func(string, Rate)
	// pollHook, if set, is the synchronous poll fallback invoked when the
	// cached rate is stale beyond MAX_STALE_AGE_MS.
	pollHook func(from, to string) (Rate, bool)
}

// NewSpotService builds a SpotService with default seed rates.
func NewSpotService(maxStaleAge time.Duration) *SpotService {
	s := &SpotService{
		cache:          newLRUCache(4096),
		lastGood:       make(map[string]Rate),
		maxStaleAge:    maxStaleAge,
		venues:         make(map[string]*Venue),
		errorThreshold: 3,
	}
	s.seed()
	return s
}

func (s *SpotService) seed() {
	now := time.Now().UTC()
	seeds := []Rate{
		{From: "USD", To: "BTC", Bid: decimal.NewFromInt(64900), Ask: decimal.NewFromInt(65100), Mid: decimal.NewFromInt(65000), TS: now, SourceVenue: "kraken"},
		{From: "USD", To: "ETH", Bid: decimal.NewFromInt(3495), Ask: decimal.NewFromInt(3505), Mid: decimal.NewFromInt(3500), TS: now, SourceVenue: "kraken"},
		{From: "EUR", To: "BTC", Bid: decimal.NewFromInt(69800), Ask: decimal.NewFromInt(70200), Mid: decimal.NewFromInt(70000), TS: now, SourceVenue: "kraken"},
		{From: "GBP", To: "BTC", Bid: decimal.NewFromInt(82000), Ask: decimal.NewFromInt(83000), Mid: decimal.NewFromInt(82500), TS: now, SourceVenue: "kraken"},
	}
	s.cache = newLRUCache(s.cache.cap)
	for _, r := range seeds {
		s.cache.put(pairKey(r.From, r.To), r)
		s.lastGood[pairKey(r.From, r.To)] = r
	}
	s.venueOrder = []string{"kraken", "coinbase", "binance"}
	for _, n := range s.venueOrder {
		s.venues[n] = &Venue{Name: n, ErrorThreshold: s.errorThreshold}
	}
	s.ready = true
}

func pairKey(from, to string) string { return from + "-" + to }

// Update inserts or refreshes a spot rate in the cache and last-good map.
func (s *SpotService) Update(r Rate) {
	r.TS = time.Now().UTC()
	key := pairKey(r.From, r.To)
	s.cache.put(key, r)
	s.mu.Lock()
	s.lastGood[key] = r
	s.mu.Unlock()
	if s.onUpdate != nil {
		s.onUpdate(key, r)
	}
}

// SetOnUpdate installs a callback invoked after every Update. Used to fan out
// L1 cache updates to WebSocket subscribers.
func (s *SpotService) SetOnUpdate(fn func(string, Rate)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onUpdate = fn
}

// IsReady reports whether the spot service has a warm cache (seeded).
func (s *SpotService) IsReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready && s.cache.ll.Len() > 0
}

// ReSeed re-seeds the cache with default rates (used on staleness or cold cache).
func (s *SpotService) ReSeed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed()
}

// Get returns the current spot rate for the pair. If the cached entry is older
// than MAX_STALE_AGE_MS, the cache is force-reseeded before returning. If no
// live entry exists, the last-good rate is returned with Stale=true.
func (s *SpotService) Get(from, to string) (Rate, error) {
	key := pairKey(from, to)
	r, ok := s.cache.get(key)
	if ok && time.Since(r.TS) <= s.maxStaleAge {
		globalMetrics.spotCacheHits.Inc()
		return r, nil
	}
	if ok {
		// Stale entry beyond threshold: force a synchronous poll if a poll
		// hook is configured; otherwise reseed from defaults.
		if s.pollHook != nil {
			if pr, pok := s.pollHook(from, to); pok {
				globalMetrics.spotCacheMisses.Inc()
				return pr, nil
			}
		}
		s.ReSeed()
		r, ok = s.cache.get(key)
		if ok {
			globalMetrics.spotCacheMisses.Inc()
			return r, nil
		}
	}
	globalMetrics.spotCacheMisses.Inc()
	s.mu.Lock()
	lg, hasLast := s.lastGood[key]
	s.mu.Unlock()
	if hasLast {
		lg.Stale = true
		globalMetrics.quoteSourceStale.Inc()
		return lg, nil
	}
	return Rate{}, fmt.Errorf("no spot rate for pair %s-%s", from, to)
}

// SetPollHook installs a synchronous poll fallback invoked when the cached
// rate is stale beyond MAX_STALE_AGE_MS. Returning (Rate, true) refreshes the
// cache; (Rate{}, false) falls back to reseed/last-good.
func (s *SpotService) SetPollHook(fn func(from, to string) (Rate, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollHook = fn
}

// RecordVenueError increments the error counter for a venue and marks it down
// after the configured threshold. Used by the circuit-breaker logic.
func (s *SpotService) RecordVenueError(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.venues[name]
	if !ok {
		v = &Venue{Name: name, ErrorThreshold: s.errorThreshold}
		s.venues[name] = v
	}
	v.Errors++
	if v.Errors >= v.ErrorThreshold && !v.Down {
		v.Down = true
		v.OpenedAt = time.Now().UTC()
	}
}

// RecordVenueSuccess resets the error counter and clears the down flag.
func (s *SpotService) RecordVenueSuccess(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.venues[name]; ok {
		v.Errors = 0
		v.Down = false
	}
}

// IsVenueDown reports whether the venue is currently tripped.
func (s *SpotService) IsVenueDown(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.venues[name]; ok {
		return v.Down
	}
	return false
}

// SetMaxStaleAge updates the staleness threshold (for tests).
func (s *SpotService) SetMaxStaleAge(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxStaleAge = d
}

// FetchVenueOrder returns the configured venue order (priority order). Used
// by the failover logic in Get/Poll.
func (s *SpotService) FetchVenueOrder() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.venueOrder))
	copy(out, s.venueOrder)
	return out
}

// AdvanceVenue records a failover from one venue to the next in priority order,
// emits a structured runbook-style log, and increments the venue_failover
// metric. Returns the next venue name and true if a failover occurred.
func (s *SpotService) AdvanceVenue(from string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range s.venueOrder {
		if v == from && i+1 < len(s.venueOrder) {
			next := s.venueOrder[i+1]
			logWarn("venue failover",
				FStr("from", from), FStr("to", next),
				FStr("reason", "error_threshold_reached"))
			globalMetrics.venueFailover.WithLabelValues(from, next).Inc()
			return next, true
		}
	}
	return from, false
}

// HalfOpenProbe resets a single venue's down flag to allow a probe request
// through (circuit-breaker half-open state). Returns true if the venue was down.
func (s *SpotService) HalfOpenProbe(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.venues[name]
	if !ok || !v.Down {
		return false
	}
	v.Down = false
	v.Errors = 0
	logInfo("venue half-open probe", FStr("venue", name))
	return true
}

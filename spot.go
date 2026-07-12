package main

import (
	"container/list"
	"fmt"
	"sync"
	"time"
)

// Rate is a cached spot rate for a trading pair.
type Rate struct {
	From        string
	To          string
	Bid         float64
	Ask         float64
	Mid         float64
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
	Name         string
	Errors       int
	Down         bool
	OpenedAt     time.Time
	ErrorThreshold int
}

// SpotService provides spot-rate lookups backed by an LRU cache, stale-rate
// enforcement, last-good fallback, and a simple per-venue circuit breaker.
type SpotService struct {
	mu            sync.Mutex
	cache         *lruCache
	lastGood      map[string]Rate
	maxStaleAge   time.Duration
	venues        map[string]*Venue
	venueOrder    []string
	errorThreshold int
	ready         bool
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
		{From: "USD", To: "BTC", Bid: 64900, Ask: 65100, Mid: 65000, TS: now, SourceVenue: "kraken"},
		{From: "USD", To: "ETH", Bid: 3495, Ask: 3505, Mid: 3500, TS: now, SourceVenue: "kraken"},
		{From: "EUR", To: "BTC", Bid: 69800, Ask: 70200, Mid: 70000, TS: now, SourceVenue: "kraken"},
		{From: "GBP", To: "BTC", Bid: 82000, Ask: 83000, Mid: 82500, TS: now, SourceVenue: "kraken"},
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
	s.cache.put(pairKey(r.From, r.To), r)
	s.mu.Lock()
	s.lastGood[pairKey(r.From, r.To)] = r
	s.mu.Unlock()
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
		return r, nil
	}
	if ok {
		s.ReSeed()
		r, ok = s.cache.get(key)
		if ok {
			return r, nil
		}
	}
	s.mu.Lock()
	lg, hasLast := s.lastGood[key]
	s.mu.Unlock()
	if hasLast {
		lg.Stale = true
		return lg, nil
	}
	return Rate{}, fmt.Errorf("no spot rate for pair %s-%s", from, to)
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
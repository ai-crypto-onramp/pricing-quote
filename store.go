package main

import (
	"sort"
	"sync"
	"time"
)

// QuoteStatus enumerates quote lifecycle states.
type QuoteStatus string

const (
	StatusOpen     QuoteStatus = "open"
	StatusClaimed  QuoteStatus = "claimed"
	StatusExpired  QuoteStatus = "expired"
	StatusCanceled QuoteStatus = "canceled"
)

// Quote is the durable record of an issued quote.
type Quote struct {
	QuoteID      string      `json:"quote_id"`
	From         string      `json:"from"`
	To           string      `json:"to"`
	Amount       string      `json:"amount"`
	Rate         string      `json:"rate"`
	SpreadBPS    int         `json:"spread_bps"`
	Fee          string      `json:"fee"`
	FeeCurrency  string      `json:"fee_currency"`
	Total        string      `json:"total"`
	CryptoAmount string      `json:"crypto_amount"`
	UserTier     string      `json:"user_tier"`
	Side         string      `json:"side"`
	Status       QuoteStatus `json:"status"`
	SourceVenue  string      `json:"source_venue"`
	CreatedAt    time.Time   `json:"created_at"`
	ExpiresAt    time.Time   `json:"expires_at"`
	ClaimedAt    *time.Time  `json:"claimed_at,omitempty"`
	ClaimedBy    string      `json:"claimed_by,omitempty"`

	// LockedRate is the effective rate captured at quote time, used for the
	// slippage comparison at claim. Not serialized.
	LockedRate float64 `json:"-"`
	// SpotPrice is the fiat-per-crypto spot captured at quote time.
	SpotPrice float64 `json:"-"`
}

// FeeSchedule defines spread/fee per (tier, asset, side, size band).
type FeeSchedule struct {
	ID          int       `json:"id"`
	UserTier    string    `json:"user_tier"`
	Asset       string    `json:"asset"`
	SizeBandMin float64   `json:"size_band_min"`
	SizeBandMax float64   `json:"size_band_max"`
	Side        string    `json:"side"`
	SpreadBPS   int       `json:"spread_bps"`
	FeeType     string    `json:"fee_type"`
	FeeAmount   float64   `json:"fee_amount"`
	FeeBPS      int       `json:"fee_bps"`
	Enabled     bool      `json:"enabled"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RateSource is an upstream venue registry entry.
type RateSource struct {
	Name        string    `json:"name"`
	Priority    int       `json:"priority"`
	Enabled     bool      `json:"enabled"`
	EndpointRef string    `json:"endpoint_ref"`
	Weight      int       `json:"weight"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store is the in-memory OLTP store for quotes, fee_schedules, rate_sources.
type Store struct {
	mu        sync.RWMutex
	quotes    map[string]*Quote
	schedules []FeeSchedule
	sources   map[string]*RateSource
	nextID    int
}

// NewStore returns an empty in-memory Store.
func NewStore() *Store {
	return &Store{
		quotes:  make(map[string]*Quote),
		sources: make(map[string]*RateSource),
	}
}

// SaveQuote persists a quote row keyed by quote_id.
func (s *Store) SaveQuote(q *Quote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *q
	s.quotes[q.QuoteID] = &cp
}

// GetQuote loads a quote by id. Returns nil if not found.
func (s *Store) GetQuote(id string) *Quote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.quotes[id]
	if !ok {
		return nil
	}
	cp := *q
	return &cp
}

// UpdateQuote applies mutator fn to the quote identified by id under the lock.
// Returns the updated quote and false if the quote was not found.
func (s *Store) UpdateQuote(id string, fn func(*Quote)) (*Quote, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.quotes[id]
	if !ok {
		return nil, false
	}
	fn(q)
	cp := *q
	return &cp, true
}

// ListQuotes returns a snapshot of all quotes ordered by creation time.
func (s *Store) ListQuotes() []*Quote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Quote, 0, len(s.quotes))
	for _, q := range s.quotes {
		cp := *q
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// SetFeeSchedules replaces the in-memory fee schedule set.
func (s *Store) SetFeeSchedules(fs []FeeSchedule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules = append([]FeeSchedule(nil), fs...)
}

// FeeSchedules returns a copy of the current fee schedules.
func (s *Store) FeeSchedules() []FeeSchedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]FeeSchedule(nil), s.schedules...)
}

// SetRateSources replaces the in-memory rate source set.
func (s *Store) SetRateSources(rs []*RateSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources = make(map[string]*RateSource, len(rs))
	for _, r := range rs {
		cp := *r
		s.sources[r.Name] = &cp
	}
}

// RateSources returns the current enabled rate sources ordered by priority.
func (s *Store) RateSources() []*RateSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RateSource, 0, len(s.sources))
	for _, r := range s.sources {
		if r.Enabled {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// lockEntry is a single locked-quote key with its expiry.
type lockEntry struct {
	value     string
	expiresAt time.Time
	timer     *time.Timer
}

// LockBackend is the locked-quote store contract used by the server. The
// in-memory LockStore satisfies it; redisLockBackend (redis_lock.go) is a
// Redis-backed implementation wired when REDIS_URL is set.
type LockBackend interface {
	// SetNX sets key=value with ttl only if key is absent. Returns true if set.
	SetNX(key, value string, ttl time.Duration) bool
	// Get returns the value for key if present and not expired.
	Get(key string) (string, bool)
	// Del removes the key. Returns true if the key existed.
	Del(key string) bool
	// Claim atomically returns the value and deletes the key if present and
	// not expired. Returns ("", false) otherwise.
	Claim(key string) (string, bool)
	// Ready reports whether the backing store is reachable. In-memory stores
	// are always ready; the Redis adapter pings the server.
	Ready() bool
}

// claimLuaScript is the Redis Lua script implementing an atomic claim: it
// returns the prior value and deletes the key in a single round-trip when the
// key exists and has not expired (TTL > 0). Used by redisLockBackend.
const claimLuaScript = `local v = redis.call('GET', KEYS[1])
if v == false then return nil end
local ttl = redis.call('PTTL', KEYS[1])
if ttl == nil or ttl < 0 then return nil end
redis.call('DEL', KEYS[1])
return v
`

// LockStore is the in-memory LockBackend with TTL and atomic claim. It is the
// default implementation used in tests and when REDIS_URL is unset.
type LockStore struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

// NewLockStore returns an empty LockStore.
func NewLockStore() *LockStore {
	return &LockStore{locks: make(map[string]*lockEntry)}
}

// Ready reports whether the in-memory store is reachable (always true).
func (l *LockStore) Ready() bool { return true }

// SetNX sets key=value with ttl only if key is absent or expired. Returns true
// if the value was set.
func (l *LockStore) SetNX(key, value string, ttl time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.locks[key]; ok {
		if time.Now().Before(e.expiresAt) {
			return false
		}
		if e.timer != nil {
			e.timer.Stop()
		}
	}
	entry := &lockEntry{value: value, expiresAt: time.Now().Add(ttl)}
	entry.timer = time.AfterFunc(ttl, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if cur, ok := l.locks[key]; ok && cur == entry {
			delete(l.locks, key)
		}
	})
	l.locks[key] = entry
	return true
}

// Get returns the value for key if present and not expired.
func (l *LockStore) Get(key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.locks[key]
	if !ok {
		return "", false
	}
	if !time.Now().Before(e.expiresAt) {
		if e.timer != nil {
			e.timer.Stop()
		}
		delete(l.locks, key)
		return "", false
	}
	return e.value, true
}

// Del removes the key and stops its expiry timer. Returns true if the key
// existed.
func (l *LockStore) Del(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.locks[key]
	if !ok {
		return false
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	delete(l.locks, key)
	return true
}

// Claim atomically returns the value and deletes the key if it exists and has
// not expired. Returns ("", false) if the key is missing or expired.
func (l *LockStore) Claim(key string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.locks[key]
	if !ok {
		return "", false
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	delete(l.locks, key)
	if !time.Now().Before(e.expiresAt) {
		return "", false
	}
	return e.value, true
}
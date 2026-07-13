package main

import (
	"math"
)

// Pricer computes quote prices by combining the spot rate with a matching fee
// schedule. It is safe for concurrent use because it only reads from the Store.
type Pricer struct {
	store            *Store
	spot             *SpotService
	defaultSpreadBPS int
	index            *feeIndex
	fx               FXClient
}

// NewPricer returns a Pricer backed by the given store and spot service.
func NewPricer(store *Store, spot *SpotService, defaultSpreadBPS int) *Pricer {
	idx := newFeeIndex()
	idx.Rebuild(store.FeeSchedules())
	return &Pricer{store: store, spot: spot, defaultSpreadBPS: defaultSpreadBPS, index: idx}
}

// SetFXClient wires the fx-hedging client used for cross-pair (non-USD fiat) quotes.
func (p *Pricer) SetFXClient(fx FXClient) { p.fx = fx }

// ReloadIndex rebuilds the in-memory fee index from the store's current
// schedules. Called after hot-reload.
func (p *Pricer) ReloadIndex() {
	p.index.Rebuild(p.store.FeeSchedules())
	globalMetrics.loadedSchedules.Set(float64(p.index.Len()))
}

// ComputeResult is the output of Pricer.Compute.
type ComputeResult struct {
	Rate         float64
	Spot         float64
	SpreadBPS    int
	Fee          float64
	Total        float64
	CryptoAmount float64
	SourceVenue  string
}

// Compute returns the effective rate, spread, fee, total (fiat), and crypto
// amount for a quote request. Side is "buy" (user buys crypto, pays fiat) or
// "sell" (user sells crypto, receives fiat). For buys, amount is fiat; for
// sells, amount is crypto.
func (p *Pricer) Compute(from, to string, amount float64, userTier, side string) (ComputeResult, error) {
	r, err := p.spot.Get(from, to)
	if err != nil {
		return ComputeResult{}, err
	}
	asset := to
	sched := p.matchSchedule(userTier, asset, side, amount)
	spreadBPS := p.defaultSpreadBPS
	if sched != nil {
		spreadBPS = sched.SpreadBPS
	} else {
		// Unknown combination: fall back to DEFAULT_SPREAD_BPS with a warning.
		logWarn("no fee schedule match; using default spread",
			fStr("tier", userTier), fStr("asset", asset), fStr("side", side),
			fInt("spread_bps", p.defaultSpreadBPS))
	}

	spot := r.Mid
	if spot <= 0 {
		spot = r.Bid + (r.Ask-r.Bid)/2
	}
	if spot <= 0 {
		return ComputeResult{}, errBadRate
	}
	spread := float64(spreadBPS) / 10000.0

	var rate, fee, total, cryptoAmount float64
	switch side {
	case "buy":
		rate = spot * (1 + spread)
		fee = computeFee(sched, amount)
		total = amount + fee
		cryptoAmount = amount / rate
	case "sell":
		rate = spot * (1 - spread)
		cryptoAmount = amount
		fee = computeFee(sched, total)
		total = amount*rate - fee
	}

	return ComputeResult{
		Rate:         rate,
		Spot:         spot,
		SpreadBPS:     spreadBPS,
		Fee:          round(fee, 8),
		Total:        round(total, 8),
		CryptoAmount: round(cryptoAmount, 8),
		SourceVenue:  r.SourceVenue,
	}, nil
}

func (p *Pricer) matchSchedule(tier, asset, side string, amount float64) *FeeSchedule {
	if s := p.index.Lookup(tier, asset, side, amount); s != nil {
		return s
	}
	// Fallback: linear scan of store (covers schedules with side="" or other
	// edge cases not indexed by the strict key).
	all := p.store.FeeSchedules()
	var best *FeeSchedule
	for i := range all {
		s := &all[i]
		if !s.Enabled {
			continue
		}
		if s.UserTier != tier {
			continue
		}
		if s.Asset != asset {
			continue
		}
		if s.Side != "" && s.Side != side {
			continue
		}
		if amount < s.SizeBandMin || (s.SizeBandMax > 0 && amount > s.SizeBandMax) {
			continue
		}
		best = s
		break
	}
	return best
}

func computeFee(s *FeeSchedule, base float64) float64 {
	if s == nil {
		return 0
	}
	switch s.FeeType {
	case "flat":
		return s.FeeAmount
	case "bps":
		return base * float64(s.FeeBPS) / 10000.0
	default:
		return 0
	}
}

func round(v float64, places int) float64 {
	pow := math.Pow(10, float64(places))
	return math.Round(v*pow) / pow
}

var errBadRate = errStr("invalid spot rate")

type errStr string

func (e errStr) Error() string { return string(e) }
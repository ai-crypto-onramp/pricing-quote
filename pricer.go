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
}

// NewPricer returns a Pricer backed by the given store and spot service.
func NewPricer(store *Store, spot *SpotService, defaultSpreadBPS int) *Pricer {
	return &Pricer{store: store, spot: spot, defaultSpreadBPS: defaultSpreadBPS}
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
package pricing

import (
	"github.com/shopspring/decimal"
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
	Rate         decimal.Decimal
	Spot         decimal.Decimal
	SpreadBPS    int
	Fee          decimal.Decimal
	Total        decimal.Decimal
	CryptoAmount decimal.Decimal
	SourceVenue  string
}

// Compute returns the effective rate, spread, fee, total (fiat), and crypto
// amount for a quote request. Side is "BUY" (user buys crypto, pays fiat) or
// "SELL" (user sells crypto, receives fiat). For buys, amount is fiat; for
// sells, amount is crypto.
func (p *Pricer) Compute(from, to string, amount decimal.Decimal, userTier, side string) (ComputeResult, error) {
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
			FStr("tier", userTier), FStr("asset", asset), FStr("side", side),
			fInt("spread_bps", p.defaultSpreadBPS))
	}

	spot := r.Mid
	if spot.LessThanOrEqual(decimal.Zero) {
		spot = r.Bid.Add(r.Ask.Sub(r.Bid).Div(decimal.NewFromInt(2)))
	}
	if spot.LessThanOrEqual(decimal.Zero) {
		return ComputeResult{}, errBadRate
	}
	spread := decimal.NewFromInt(int64(spreadBPS)).Div(decimal.NewFromInt(10000))

	var rate, fee, total, cryptoAmount decimal.Decimal
	switch side {
	case "BUY":
		rate = spot.Mul(decimal.NewFromInt(1).Add(spread))
		fee = computeFee(sched, amount)
		total = amount.Add(fee)
		cryptoAmount = amount.Div(rate)
	case "SELL":
		rate = spot.Mul(decimal.NewFromInt(1).Sub(spread))
		cryptoAmount = amount
		fee = computeFee(sched, total)
		total = amount.Mul(rate).Sub(fee)
	}

	return ComputeResult{
		Rate:         rate,
		Spot:         spot,
		SpreadBPS:    spreadBPS,
		Fee:          round(fee, 8),
		Total:        round(total, 8),
		CryptoAmount: round(cryptoAmount, 8),
		SourceVenue:  r.SourceVenue,
	}, nil
}

func (p *Pricer) matchSchedule(tier, asset, side string, amount decimal.Decimal) *FeeSchedule {
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
		if amount.LessThan(s.SizeBandMin) || (s.SizeBandMax.GreaterThan(decimal.Zero) && amount.GreaterThan(s.SizeBandMax)) {
			continue
		}
		best = s
		break
	}
	return best
}

func computeFee(s *FeeSchedule, base decimal.Decimal) decimal.Decimal {
	if s == nil {
		return decimal.Zero
	}
	switch s.FeeType {
	case "FLAT":
		return s.FeeAmount
	case "BPS":
		return base.Mul(decimal.NewFromInt(int64(s.FeeBPS))).Div(decimal.NewFromInt(10000))
	default:
		return decimal.Zero
	}
}

func round(v decimal.Decimal, places int) decimal.Decimal {
	pow := decimal.NewFromInt(10).Pow(decimal.NewFromInt(int64(places)))
	return v.Mul(pow).Round(0).Div(pow)
}

var errBadRate = errStr("invalid spot rate")

type errStr string

func (e errStr) Error() string { return string(e) }

package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// FXClient is the fx-hedging integration contract. For non-USD fiat `from`,
// the pricer fetches the fiat→USD leg and combines it with the USD/crypto spot
// to produce the cross pair rate. The default returns an error (no FX service
// configured); an httpFXClient wires the real endpoint.
type FXClient interface {
	// FetchFX returns the fiat→USD rate (e.g. EUR→USD = 1.08) and the hedge-cost
	// markup in bps to add to the spread for pre-hedged tiers.
	FetchFX(ctx context.Context, fiat string) (fxRate float64, hedgeCostBPS int, err error)
}

// noopFXClient returns ErrFXUnavailable.
type noopFXClient struct{}

func (noopFXClient) FetchFX(ctx context.Context, fiat string) (float64, int, error) {
	return 0, 0, ErrFXUnavailable
}

// ErrFXUnavailable is returned when no FX client is configured.
var ErrFXUnavailable = fmt.Errorf("fx service unavailable")

// httpFXClient calls fx-hedging at BaseURL/fx?fiat=EUR.
type httpFXClient struct {
	BaseURL string
	client  *http.Client
}

func newHTTPFXClient(baseURL string) *httpFXClient {
	if baseURL == "" {
		return nil
	}
	return &httpFXClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 2 * time.Second},
	}
}

func (c *httpFXClient) FetchFX(ctx context.Context, fiat string) (float64, int, error) {
	start := time.Now()
	defer func() { globalMetrics.fxLookupSeconds.Observe(time.Since(start).Seconds()) }()
	url := fmt.Sprintf("%s/fx?fiat=%s", c.BaseURL, fiat)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("fx status %d", resp.StatusCode)
	}
	var body struct {
		Rate         float64 `json:"rate"`
		HedgeCostBPS int     `json:"hedge_cost_bps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, 0, err
	}
	return body.Rate, body.HedgeCostBPS, nil
}

// CrossPairQuote computes a quote for a non-USD fiat pair by combining the
// fiat→USD leg (from fx-hedging) with the USD/crypto spot. If no FXClient is
// configured or the fiat is USD, it falls back to the direct spot lookup.
func (p *Pricer) CrossPairQuote(ctx context.Context, from, to string, amount decimal.Decimal, userTier, side string) (ComputeResult, error) {
	if from == "USD" || p.fx == nil {
		return p.Compute(from, to, amount, userTier, side)
	}
	fxRate, hedgeBPS, err := p.fx.FetchFX(ctx, from)
	if err != nil {
		// Fall back to a direct spot lookup (cross pair may be seeded).
		return p.Compute(from, to, amount, userTier, side)
	}
	// Get the USD/crypto spot and apply the FX leg.
	usdRes, err := p.Compute("USD", to, amount, userTier, side)
	if err != nil {
		return ComputeResult{}, err
	}
	fxRateDec := decimal.NewFromFloat(fxRate)
	// Adjust the rate by the fiat→USD conversion.
	cross := usdRes
	cross.Rate = usdRes.Rate.Div(fxRateDec)
	cross.Spot = usdRes.Spot.Div(fxRateDec)
	// Apply hedge-cost markup to the spread for pre-hedged tiers.
	if hedgeBPS > 0 {
		cross.SpreadBPS += hedgeBPS
		spread := decimal.NewFromInt(int64(cross.SpreadBPS)).Div(decimal.NewFromInt(10000))
		switch side {
		case "BUY":
			cross.Rate = cross.Spot.Mul(decimal.NewFromInt(1).Add(spread))
			cross.CryptoAmount = amount.Div(cross.Rate)
		case "SELL":
			cross.Rate = cross.Spot.Mul(decimal.NewFromInt(1).Sub(spread))
			cross.Total = amount.Mul(cross.Rate).Sub(cross.Fee)
		}
		cross.Total = round(cross.Total, 8)
		cross.CryptoAmount = round(cross.CryptoAmount, 8)
	}
	return cross, nil
}

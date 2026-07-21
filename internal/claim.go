package pricing

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ClaimRequest is the body of POST /internal/v1/quotes/:id/claim.
type ClaimRequest struct {
	ClaimedBy string `json:"claimed_by"`
}

// ClaimResult is the outcome of an attempted claim.
type ClaimResult struct {
	Quote  *Quote
	Reason string
	Stale  bool
}

// ClaimService implements the atomic claim flow with expiry and slippage guards.
type ClaimService struct {
	store                *Store
	locks                LockBackend
	spot                 *SpotService
	audit                *AuditLog
	slippageToleranceBPS int
}

// NewClaimService returns a ClaimService.
func NewClaimService(store *Store, locks LockBackend, spot *SpotService, audit *AuditLog, slipBPS int) *ClaimService {
	return &ClaimService{
		store:                store,
		locks:                locks,
		spot:                 spot,
		audit:                audit,
		slippageToleranceBPS: slipBPS,
	}
}

// Claim attempts to atomically claim the quote identified by id on behalf of
// claimedBy. Returns a ClaimResult whose Reason is empty on success.
func (c *ClaimService) Claim(id uuid.UUID, claimedBy string) ClaimResult {
	q := c.store.GetQuote(id)
	if q == nil {
		return ClaimResult{Reason: "missing"}
	}
	if q.Status == StatusCanceled || q.Status == StatusExpired {
		return ClaimResult{Reason: "EXPIRED", Quote: q}
	}
	if q.Status == StatusClaimed {
		return ClaimResult{Reason: "already_claimed", Quote: q}
	}
	if time.Now().UTC().After(q.ExpiresAt) {
		c.store.UpdateQuote(id, func(row *Quote) { row.Status = StatusExpired })
		q.Status = StatusExpired
		c.audit.Append(AuditEvent{Type: "quote.expired", QuoteID: id, UserTier: q.UserTier, SourceVenue: q.SourceVenue})
		return ClaimResult{Reason: "EXPIRED", Quote: q}
	}
	lockVal, ok := c.locks.Claim(lockKey(id))
	if !ok {
		return ClaimResult{Reason: "already_claimed", Quote: q}
	}
	var locked struct {
		Rate decimal.Decimal `json:"rate"`
	}
	_ = json.Unmarshal([]byte(lockVal), &locked)

	curRate, err := c.spot.Get(q.From, q.To)
	if err == nil {
		var base decimal.Decimal
		if curRate.Mid.GreaterThan(decimal.Zero) {
			base = curRate.Mid
		} else {
			base = curRate.Bid.Add(curRate.Ask).Div(decimal.NewFromInt(2))
		}
		if base.GreaterThan(decimal.Zero) && q.LockedRate.GreaterThan(decimal.Zero) {
			diff := base.Sub(q.LockedRate)
			bps := diff.Div(q.LockedRate).Mul(decimal.NewFromInt(10000))
			if bps.LessThan(decimal.Zero) {
				bps = bps.Neg()
			}
			if bps.IntPart() > int64(c.slippageToleranceBPS) {
				c.audit.Append(AuditEvent{
					Type: "quote.slippage_rejected", QuoteID: id, UserTier: q.UserTier,
					SourceVenue: q.SourceVenue, Reason: "slippage_exceeded",
				})
				return ClaimResult{Reason: "slippage_exceeded", Quote: q}
			}
		}
	}
	now := time.Now().UTC()
	updated, ok := c.store.UpdateQuote(id, func(row *Quote) {
		row.Status = StatusClaimed
		row.ClaimedAt = &now
		row.ClaimedBy = claimedBy
	})
	if !ok {
		return ClaimResult{Reason: "missing", Quote: q}
	}
	c.audit.Append(AuditEvent{
		Type: "quote.claimed", QuoteID: id, UserTier: q.UserTier, SourceVenue: q.SourceVenue,
	})
	return ClaimResult{Quote: updated}
}

// Refresh cancels the old quote and produces a freshly computed one.
func (c *ClaimService) Refresh(id uuid.UUID, compute func(*Quote) (*Quote, error)) (*Quote, error) {
	old := c.store.GetQuote(id)
	if old == nil {
		return nil, errNotFound
	}
	c.store.UpdateQuote(id, func(row *Quote) {
		row.Status = StatusCanceled
	})
	c.locks.Del(lockKey(id))
	c.audit.Append(AuditEvent{Type: "quote.refreshed", QuoteID: id, UserTier: old.UserTier, SourceVenue: old.SourceVenue})
	nq, err := compute(old)
	if err != nil {
		return nil, err
	}
	return nq, nil
}

func lockKey(id uuid.UUID) string { return "lock:quote:" + id.String() }

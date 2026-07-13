package main

import (
	"encoding/json"
	"time"
)

// ClaimRequest is the body of POST /internal/v1/quotes/:id/claim.
type ClaimRequest struct {
	ClaimedBy string `json:"claimed_by"`
}

// ClaimResult is the outcome of an attempted claim.
type ClaimResult struct {
	Quote    *Quote
	Reason   string
	Stale    bool
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
		store:                 store,
		locks:                 locks,
		spot:                  spot,
		audit:                 audit,
		slippageToleranceBPS:  slipBPS,
	}
}

// Claim attempts to atomically claim the quote identified by id on behalf of
// claimedBy. Returns a ClaimResult whose Reason is empty on success.
func (c *ClaimService) Claim(id, claimedBy string) ClaimResult {
	q := c.store.GetQuote(id)
	if q == nil {
		return ClaimResult{Reason: "missing"}
	}
	if q.Status == StatusCanceled || q.Status == StatusExpired {
		return ClaimResult{Reason: "expired", Quote: q}
	}
	if q.Status == StatusClaimed {
		return ClaimResult{Reason: "already_claimed", Quote: q}
	}
	if time.Now().UTC().After(q.ExpiresAt) {
		c.store.UpdateQuote(id, func(row *Quote) { row.Status = StatusExpired })
		q.Status = StatusExpired
		c.audit.Append(AuditEvent{Type: "quote.expired", QuoteID: id, UserTier: q.UserTier, SourceVenue: q.SourceVenue})
		return ClaimResult{Reason: "expired", Quote: q}
	}
	lockVal, ok := c.locks.Claim(lockKey(id))
	if !ok {
		return ClaimResult{Reason: "already_claimed", Quote: q}
	}
	var locked struct {
		Rate float64 `json:"rate"`
	}
	_ = json.Unmarshal([]byte(lockVal), &locked)

	curRate, err := c.spot.Get(q.From, q.To)
	if err == nil {
		var base float64
		if curRate.Mid > 0 {
			base = curRate.Mid
		} else {
			base = (curRate.Bid + curRate.Ask) / 2
		}
		if base > 0 && q.LockedRate > 0 {
			diff := base - q.LockedRate
			bps := (diff / q.LockedRate) * 10000
			if bps < 0 {
				bps = -bps
			}
			if int(bps) > c.slippageToleranceBPS {
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
func (c *ClaimService) Refresh(id string, compute func(*Quote) (*Quote, error)) (*Quote, error) {
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

func lockKey(id string) string { return "lock:quote:" + id }
package main

import (
	"crypto/rand"
	"encoding/base32"
	"time"
)

// AuditEvent records a quote lifecycle transition.
type AuditEvent struct {
	Type        string    `json:"type"`
	QuoteID     string    `json:"quote_id"`
	UserTier    string    `json:"user_tier"`
	SourceVenue string    `json:"source_venue"`
	Reason      string    `json:"reason,omitempty"`
	At          time.Time `json:"at"`
}

// AuditLog is an in-memory append-only audit event log.
type AuditLog struct {
	events []AuditEvent
}

// NewAuditLog returns an empty AuditLog.
func NewAuditLog() *AuditLog { return &AuditLog{} }

// Append records an event.
func (a *AuditLog) Append(e AuditEvent) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	a.events = append(a.events, e)
}

// Events returns a copy of all events.
func (a *AuditLog) Events() []AuditEvent {
	out := make([]AuditEvent, len(a.events))
	copy(out, a.events)
	return out
}

// newQuoteID generates a ULID-like identifier: q_<base32 random chars>.
func newQuoteID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return "q_" + s
}

// nowMonotonic returns the current UTC time. Wraps time.Now for tests but
// deliberately uses the real clock to keep the implementation simple.
func nowMonotonic() time.Time { return time.Now().UTC() }
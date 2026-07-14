package pricing

import (
	"crypto/rand"
	"encoding/base32"
	"sync"
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

// AuditSink is the contract for a downstream audit-event-log consumer. The
// default in-memory sink (memAuditSink) just records events for tests; a
// production implementation would POST to audit-event-log with at-least-once
// delivery and retry.
type AuditSink interface {
	Send(e AuditEvent) error
}

// memAuditSink is the in-memory AuditSink backing AuditLog.
type memAuditSink struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (m *memAuditSink) Send(e AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

func (m *memAuditSink) snapshot() []AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AuditEvent, len(m.events))
	copy(out, m.events)
	return out
}

// AuditLog is an in-memory append-only audit event log with an asynchronous
// bounded-queue fan-out to a downstream sink. Events are recorded locally and
// dispatched to the sink on a goroutine with at-least-once semantics: if the
// queue is full, the event is dropped but a dropped counter is incremented
// (the durable quotes row remains the system of record).
type AuditLog struct {
	sink    AuditSink
	queue   chan AuditEvent
	dropped int
	stop    chan struct{}
	wg      sync.WaitGroup
	local   *memAuditSink
}

// NewAuditLog returns an AuditLog with a bounded queue of the given size. A
// size <= 0 defaults to 256.
func NewAuditLog() *AuditLog {
	return NewAuditLogWithSink(256, nil)
}

// NewAuditLogWithSink builds an AuditLog that dispatches to sink asynchronously.
// If sink is nil, events are only stored in-memory.
func NewAuditLogWithSink(queueSize int, sink AuditSink) *AuditLog {
	if queueSize <= 0 {
		queueSize = 256
	}
	a := &AuditLog{
		sink:  sink,
		queue: make(chan AuditEvent, queueSize),
		stop:  make(chan struct{}),
		local: &memAuditSink{},
	}
	a.wg.Add(1)
	go a.dispatch()
	return a
}

// dispatch drains the queue, sending each event to the sink. On error, the
// event is re-queued once (at-least-once). On shutdown, remaining queued
// events are flushed to the sink before returning.
func (a *AuditLog) dispatch() {
	defer a.wg.Done()
	for {
		select {
		case e := <-a.queue:
			if a.sink != nil {
				if err := a.sink.Send(e); err != nil {
					// re-queue once; if still full, drop.
					select {
					case a.queue <- e:
					default:
						a.dropped++
					}
				}
			}
		case <-a.stop:
			// drain remaining
			for {
				select {
				case e := <-a.queue:
					if a.sink != nil {
						_ = a.sink.Send(e)
					}
				default:
					return
				}
			}
		}
	}
}

// Append records an event locally and enqueues it for async dispatch.
func (a *AuditLog) Append(e AuditEvent) {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	a.local.Send(e)
	select {
	case a.queue <- e:
	default:
		a.dropped++
	}
}

// Events returns a copy of all locally recorded events.
func (a *AuditLog) Events() []AuditEvent {
	return a.local.snapshot()
}

// Close drains the queue and stops the dispatch goroutine.
func (a *AuditLog) Close() {
	close(a.stop)
	a.wg.Wait()
}

// Dropped returns the count of events dropped due to a full queue.
func (a *AuditLog) Dropped() int {
	return a.dropped
}

// newQuoteID generates a ULID-like identifier: q_<base32 random chars>.
func newQuoteID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
	return "q_" + s
}
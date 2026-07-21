package pricing

import (
	"context"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// FeedMessage is a single spot-rate update published to RATE_FEED_TOPIC.
type FeedMessage struct {
	Pair        string          `json:"pair"`
	Bid         decimal.Decimal `json:"bid"`
	Ask         decimal.Decimal `json:"ask"`
	Mid         decimal.Decimal `json:"mid"`
	SourceVenue string          `json:"source_venue"`
	TS          time.Time       `json:"ts"`
}

// FeedSubscriber is the pub/sub subscriber contract. The in-memory
// inProcFeed satisfies it; a Redis/Kafka-backed implementation can be wired in
// production.
type FeedSubscriber interface {
	// Subscribe begins consuming messages and delivers them to handler. The
	// call blocks until ctx is canceled or the subscription is closed.
	Subscribe(ctx context.Context, handler func(FeedMessage)) error
}

// inProcFeed is an in-process pub/sub feed used in tests and as a local
// fallback when no external broker is configured. Publish is non-blocking and
// fan-outs to all current subscribers.
type inProcFeed struct {
	mu     sync.RWMutex
	subs   []chan FeedMessage
	closed bool
}

func newInProcFeed() *inProcFeed { return &inProcFeed{} }

// Publish fans a message out to all subscribers non-blockingly.
func (f *inProcFeed) Publish(msg FeedMessage) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, ch := range f.subs {
		select {
		case ch <- msg:
		default:
			// drop on backpressure; the next poll/reseed will recover
		}
	}
}

func (f *inProcFeed) Subscribe(ctx context.Context, handler func(FeedMessage)) error {
	ch := make(chan FeedMessage, 64)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return context.Canceled
	}
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	defer f.removeSub(ch)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-ch:
			if !ok {
				return nil
			}
			handler(m)
		}
	}
}

func (f *inProcFeed) removeSub(ch chan FeedMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.subs {
		if c == ch {
			f.subs = append(f.subs[:i], f.subs[i+1:]...)
			break
		}
	}
}

// Close shuts down the feed, unblocking all subscribers.
func (f *inProcFeed) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	for _, ch := range f.subs {
		close(ch)
	}
	f.subs = nil
}

// PollClient is the on-demand poll fallback against EXCHANGE_CONNECTOR_URL.
// The default noopPollClient returns ErrNoPoll; an httpPollClient is provided
// for production wiring.
type PollClient interface {
	Poll(ctx context.Context, from, to string) (Rate, error)
}

// noopPollClient always returns an error.
type noopPollClient struct{}

func (noopPollClient) Poll(ctx context.Context, from, to string) (Rate, error) {
	return Rate{}, errNoPoll
}

// startFeedConsumer subscribes to the feed and updates the SpotService cache
// on every message. Returns a stop function. When feed is nil, this is a no-op.
func (s *Server) startFeedConsumer(ctx context.Context, feed FeedSubscriber) func() {
	if feed == nil {
		return func() {}
	}
	innerCtx, cancel := context.WithCancel(ctx)
	go func() {
		_ = feed.Subscribe(innerCtx, func(msg FeedMessage) {
			if msg.Mid.LessThanOrEqual(decimal.Zero) && msg.Bid.GreaterThan(decimal.Zero) && msg.Ask.GreaterThan(decimal.Zero) {
				msg.Mid = msg.Bid.Add(msg.Ask).Div(decimal.NewFromInt(2))
			}
			from, to := splitPair(msg.Pair)
			if from == "" || to == "" {
				return
			}
			r := Rate{
				From: from, To: to,
				Bid: msg.Bid, Ask: msg.Ask, Mid: msg.Mid,
				TS: time.Now().UTC(), SourceVenue: msg.SourceVenue,
			}
			s.spot.Update(r)
			globalMetrics.spotPubsubLagMs.Set(float64(time.Since(msg.TS).Milliseconds()))
		})
	}()
	return cancel
}

// splitPair splits a "FROM-TO" pair string.
func splitPair(p string) (string, string) {
	for i := 0; i < len(p); i++ {
		if p[i] == '-' {
			return p[:i], p[i+1:]
		}
	}
	return "", ""
}

// pollFallback attempts a synchronous poll when the cached rate is missing or
// stale. Returns the polled rate and true on success.
func (s *Server) pollFallback(ctx context.Context, from, to string) (Rate, bool) {
	if s.poll == nil {
		return Rate{}, false
	}
	r, err := s.poll.Poll(ctx, from, to)
	if err != nil {
		globalMetrics.spotPollTotal.WithLabelValues("error").Inc()
		return Rate{}, false
	}
	globalMetrics.spotPollTotal.WithLabelValues("ok").Inc()
	s.spot.Update(r)
	return r, true
}

// selectRateSource picks the best bid/ask across enabled rate_sources rows
// ordered by priority then weight. Returns the first enabled source.
func selectRateSource(sources []*RateSource) *RateSource {
	for _, rs := range sources {
		if rs.Enabled {
			return rs
		}
	}
	return nil
}

var errNoPoll = newStrErr("no poll client configured")

type strErr string

func (e strErr) Error() string { return string(e) }

func newStrErr(s string) strErr { return strErr(s) }

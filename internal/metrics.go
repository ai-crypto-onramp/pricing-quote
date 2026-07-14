package pricing

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics holds the Prometheus collectors for the service. Built once at
// startup via newMetrics and registered on the default registry.
type metrics struct {
	// Stage 2 — spot service.
	spotCacheHits    prometheus.Counter
	spotCacheMisses   prometheus.Counter
	spotPollTotal    *prometheus.CounterVec
	spotPubsubLagMs  prometheus.Gauge

	// Stage 3 — fee schedules.
	loadedSchedules prometheus.Gauge

	// Stage 4 — quotes.
	quoteRequestSeconds *prometheus.HistogramVec
	quoteStatusTotal     *prometheus.CounterVec

	// Stage 6 — failover / stale.
	quoteSourceStale prometheus.Counter
	venueFailover    *prometheus.CounterVec

	// Stage 7 — websocket.
	wsActiveConnections prometheus.Gauge
	wsMessagesSent      prometheus.Counter
	wsMessagesDropped   prometheus.Counter
	fxLookupSeconds     prometheus.Histogram

	// Stage 8 — claim outcomes.
	claimTotal *prometheus.CounterVec
}

var globalMetrics = newMetrics()

func newMetrics() *metrics {
	m := &metrics{
		spotCacheHits: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "pricing", Name: "spot_cache_hits",
			Help: "Number of L1 spot cache hits.",
		}),
		spotCacheMisses: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "pricing", Name: "spot_cache_misses",
			Help: "Number of L1 spot cache misses.",
		}),
		spotPollTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pricing", Name: "spot_poll_total",
			Help: "Number of on-demand spot polls, by result.",
		}, []string{"result"}),
		spotPubsubLagMs: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace: "pricing", Name: "spot_pubsub_lag_ms",
			Help: "Observed pub/sub lag in ms.",
		}),
		loadedSchedules: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace: "pricing", Name: "loaded_fee_schedules",
			Help: "Number of fee schedules currently loaded.",
		}),
		quoteRequestSeconds: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "pricing", Name: "quote_request_seconds",
			Help:    "Quote request latency in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"path"}),
		quoteStatusTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pricing", Name: "quote_response_total",
			Help: "Quote responses by HTTP status code.",
		}, []string{"path", "code"}),
		quoteSourceStale: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "pricing", Name: "quote_source_stale",
			Help: "Quotes served from a stale last-good source.",
		}),
		venueFailover: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pricing", Name: "venue_failover_total",
			Help: "Venue failover transitions, by venue.",
		}, []string{"from", "to"}),
		wsActiveConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace: "pricing", Name: "ws_active_connections",
			Help: "Active WebSocket subscriber connections.",
		}),
		wsMessagesSent: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "pricing", Name: "ws_messages_sent",
			Help: "WebSocket frames sent to subscribers.",
		}),
		wsMessagesDropped: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: "pricing", Name: "ws_messages_dropped",
			Help: "WebSocket frames dropped due to backpressure.",
		}),
		fxLookupSeconds: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace: "pricing", Name: "fx_lookup_seconds",
			Help:      "fx-hedging lookup latency in seconds.",
		}),
		claimTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "pricing", Name: "claim_total",
			Help: "Claim outcomes by reason.",
		}, []string{"result"}),
	}
	return m
}

// metricsHandler exposes /metrics.
func metricsHandler() http.Handler { return promhttp.Handler() }

// statusRecorder captures the status code for metrics labeling.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// metricsMiddleware records quote request latency and response status counts
// for the configured paths. Other paths are passed through with latency only.
func metricsMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		path := labeledPath(r.URL.Path)
		globalMetrics.quoteRequestSeconds.WithLabelValues(path).
			Observe(time.Since(start).Seconds())
		globalMetrics.quoteStatusTotal.WithLabelValues(path, itoa(rec.status)).Inc()
	})
}

// labeledPath collapses per-id routes to a template path so metric labels stay
// bounded.
func labeledPath(p string) string {
	if len(p) >= len("/v1/quotes/") && p[:len("/v1/quotes/")] == "/v1/quotes/" {
		rest := p[len("/v1/quotes/"):]
		parts := splitN(rest, '/', 3)
		if len(parts) == 1 && parts[0] != "" {
			return "/v1/quotes/:id"
		}
		if len(parts) == 2 && parts[1] == "refresh" {
			return "/v1/quotes/:id/refresh"
		}
		return "/v1/quotes/*"
	}
	if len(p) >= len("/internal/v1/quotes/") && p[:len("/internal/v1/quotes/")] == "/internal/v1/quotes/" {
		return "/internal/v1/quotes/:id/claim"
	}
	return p
}

// splitN is a stdlib-replacement for strings.SplitN to avoid importing strings
// here.
func splitN(s string, sep byte, n int) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
			if n > 0 && len(out)+1 >= n {
				break
			}
		}
	}
	out = append(out, s[start:])
	return out
}
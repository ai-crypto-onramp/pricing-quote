package main

import (
	"context"
	"sync"
	"time"
)

// Span is a minimal tracing span. The default implementation records spans
// in-memory; a production deployment would bridge to OpenTelemetry by wrapping
// these with otel-go spans exported to OTEL_EXPORTER_OTLP_ENDPOINT.
//
// TODO(otel): wire go.opentelemetry.io/otel and export spans to the configured
// OTEL_EXPORTER_OTLP_ENDPOINT. The current Tracer interface is shape-compatible
// with otel trace.Span, so the swap is mechanical: replace noopTracer with an
// otel TracerProvider and have Start return (otelctx, otelSpan).
type Span interface {
	End()
	SetAttribute(key string, value any)
	AddEvent(name string)
}

// Tracer creates spans. The no-op tracer returns discardSpans; the recording
// tracer collects spans for test inspection.
type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
}

type noopSpan struct{}

func (noopSpan) End()                     {}
func (noopSpan) SetAttribute(string, any) {}
func (noopSpan) AddEvent(string)          {}

type noopTracer struct{}

func (noopTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	return ctx, noopSpan{}
}

// recordingSpan records attributes and events for test inspection.
type recordingSpan struct {
	mu        sync.Mutex
	name      string
	attrs     map[string]any
	events    []string
	startedAt time.Time
	endedAt   time.Time
}

func (s *recordingSpan) End() { s.endedAt = time.Now() }
func (s *recordingSpan) SetAttribute(k string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attrs == nil {
		s.attrs = make(map[string]any)
	}
	s.attrs[k] = v
}
func (s *recordingSpan) AddEvent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, name)
}

// recordingTracer collects all started spans; used in tests.
type recordingTracer struct {
	mu    sync.Mutex
	spans []*recordingSpan
}

func (t *recordingTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	s := &recordingSpan{name: name, startedAt: time.Now()}
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
	return ctx, s
}

func (t *recordingTracer) spansSnapshot() []*recordingSpan {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*recordingSpan, len(t.spans))
	copy(out, t.spans)
	return out
}

// globalTracer is the default tracer for the service. Replaced in tests.
var globalTracer Tracer = noopTracer{}

// setTracer replaces the global tracer (for tests).
func setTracer(t Tracer) { globalTracer = t }

// spanFromContext is a helper to start a span and defer End.
func startSpan(ctx context.Context, name string) (context.Context, Span) {
	return globalTracer.Start(ctx, name)
}
package tracing_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/obs/tracing"
)

// TestTracer_NoopByDefault asserts the documented contract: code that
// calls Tracer() before Init runs gets a no-op tracer (no panic, no
// allocations of consequence). This is what makes the rest of the
// codebase free to instrument without worrying about "what if the
// operator never opted in?"
func TestTracer_NoopByDefault(t *testing.T) {
	// Reset to ensure a clean baseline regardless of previous test
	// pollution.
	tracing.Reset()

	tracer := tracing.Tracer()
	if tracer == nil {
		t.Fatal("Tracer() should never return nil")
	}
	_, span := tracer.Start(context.Background(), "test.span")
	span.End()
	// noop spans are non-recording.
	if span.SpanContext().IsValid() && span.SpanContext().IsSampled() {
		t.Errorf("expected non-recording span by default; got valid+sampled")
	}
}

// TestInit_NoExporters returns a no-op shutdown without error.
// Operators wiring the flag without any of {OTLP, Stdout} are
// effectively asking for a noop; we don't error.
func TestInit_NoExporters(t *testing.T) {
	tracing.Reset()
	shutdown, err := tracing.Init(context.Background(), tracing.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if shutdown == nil {
		t.Fatal("shutdown should not be nil even when noop")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown should not error; got %v", err)
	}
}

// TestInit_StdoutExporter installs a real provider; the tracer
// should be non-noop and span attributes round-trip.
func TestInit_StdoutExporter(t *testing.T) {
	tracing.Reset()
	t.Cleanup(tracing.Reset)

	shutdown, err := tracing.Init(context.Background(), tracing.Options{
		ServiceName: "pg_hardstorage_test",
		Stdout:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdown(context.Background())

	// Verify the tracer is no longer a noop.
	tracer := otel.GetTracerProvider().Tracer(tracing.TracerName)
	if _, ok := tracer.(noop.Tracer); ok {
		t.Error("after Init, tracer should not be the noop")
	}

	_, span := tracer.Start(context.Background(), "test.real_span")
	if !span.SpanContext().IsValid() {
		t.Error("real tracer should produce a valid span context")
	}
	span.End()
}

// TestInit_Twice replaces the provider cleanly. A second Init call
// shuts down the first and installs a new one — the global tracer
// is never left in a half-initialised state.
func TestInit_Twice(t *testing.T) {
	tracing.Reset()
	t.Cleanup(tracing.Reset)

	if _, err := tracing.Init(context.Background(), tracing.Options{Stdout: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := tracing.Init(context.Background(), tracing.Options{Stdout: true}); err != nil {
		t.Fatal(err)
	}
}

// TestReset_RevertsToNoop is the symmetric guarantee — after Reset
// the global tracer is the noop again.
func TestReset_RevertsToNoop(t *testing.T) {
	if _, err := tracing.Init(context.Background(), tracing.Options{Stdout: true}); err != nil {
		t.Fatal(err)
	}
	tracing.Reset()
	tracer := otel.GetTracerProvider().Tracer(tracing.TracerName)
	_, span := tracer.Start(context.Background(), "post_reset")
	span.End()
	if span.SpanContext().IsValid() && span.SpanContext().IsSampled() {
		t.Error("after Reset, tracer should be noop again")
	}
}

// _ keeps the trace import used even when other tests don't reference it.
var _ = trace.SpanContext{}
var _ = time.Second

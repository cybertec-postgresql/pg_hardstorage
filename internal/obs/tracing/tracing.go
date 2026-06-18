// Package tracing wires OpenTelemetry tracing into the agent.
//
// Posture:
//
//   - Default = no-op. Code that calls Tracer().Start() always works;
//     when nothing's configured, OTel's global tracer is a no-op
//     (zero overhead).
//   - Init(ctx, opts) replaces the global tracer with a real one
//     backed by an OTLP/HTTP exporter, a stdout exporter (dev), or
//     both.
//   - Span names follow the SPEC's taxonomy: top-level
//     `pg_hardstorage.backup`, `.restore`, `.wal.archive`, `.verify`,
//     and child spans for the high-value internal stages
//     (chunker.process_file, storage.put_chunk, manifest.commit,
//     pg.backup_start / .backup_stop).
//
// Why we don't instrument everything:
//
//   - Span overhead is small but not zero; high-frequency loops
//     (each chunk PUT inside a 10000-chunk backup) generate spans
//     that drown out the interesting top-level signal in any UI
//     that doesn't aggregate.
//   - The right granularity is "the operator wants to know where
//     time went on this backup." That's covered by the top-level
//     span + the per-stage children. Per-chunk spans are observable
//     via metrics + sampling, not traces.
package tracing

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/cybertec-postgresql/pg_hardstorage/internal/airgap"
)

// TracerName is the instrumentation library name. Stable across
// versions; a span filter looking at instrumentation_library
// finds every span we emit by this string.
const TracerName = "github.com/cybertec-postgresql/pg_hardstorage"

// SchemaSpan is the schema string (informational; OpenTelemetry has
// its own resource schema URLs). Documented here so a future
// might add agent-emitted span attributes that need a schema name.
const SchemaSpan = "pg_hardstorage.span.v1"

// Options configures Init. Zero value disables every exporter
// (so calling Init with Options{} leaves the global tracer at
// noop). Production callers pass an OTLPEndpoint; dev callers pass
// Stdout=true.
type Options struct {
	// ServiceName ends up as the OTel resource's service.name. We
	// default to "pg_hardstorage"; multi-binary deployments (agent
	// + control-plane) override.
	ServiceName string

	// ServiceVersion is the binary's version string. Recorded as
	// service.version on every span.
	ServiceVersion string

	// OTLPEndpoint is the OTLP/HTTP collector URL (e.g.
	// "http://otel-collector:4318"). Empty disables this exporter.
	OTLPEndpoint string

	// OTLPHeaders are added to every OTLP request. Useful for
	// auth tokens (e.g. honeycomb's "x-honeycomb-team").
	OTLPHeaders map[string]string

	// OTLPInsecure disables TLS for the OTLP endpoint. Default
	// false; only flip to true when targeting a sidecar collector
	// on localhost.
	OTLPInsecure bool

	// Stdout, when true, also installs a stdout exporter (useful
	// for dev — see span output without standing up a collector).
	// Production callers leave this false.
	Stdout bool

	// Sampler controls the trace sampler. Empty defaults to the
	// SDK's "parent-based always-sample" — fine for the agent's
	// low span rate. Production fleets with thousands of agents
	// might want "parent-based, ratio 0.1".
	Sampler sdktrace.Sampler
}

// state holds the runtime tracer + shutdown hook. Set by Init,
// consumed by Shutdown.
type state struct {
	tp       *sdktrace.TracerProvider
	shutdown func(context.Context) error
}

var (
	mu      sync.Mutex
	current *state
)

// Init replaces the global tracer with one backed by the configured
// exporters. Returns a Shutdown function that flushes pending spans
// — production callers defer it before the process exits, otherwise
// in-flight spans are lost.
//
// Calling Init twice replaces the previous tracer (after shutting
// down the old one). Tests use this freely; production calls Init
// once at startup.
func Init(ctx context.Context, opts Options) (shutdown func(context.Context) error, err error) {
	if opts.ServiceName == "" {
		opts.ServiceName = "pg_hardstorage"
	}

	exporters := []sdktrace.SpanExporter{}
	if opts.OTLPEndpoint != "" {
		// Air-gap gate: refuse a public OTLP collector endpoint
		// in strict mode.  An on-perimeter collector at loopback
		// or RFC1918 is what air-gapped sites actually run.
		if err := airgap.Default().EndpointAllowed(opts.OTLPEndpoint); err != nil {
			return nil, fmt.Errorf("tracing: %w", err)
		}
		httpOpts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(stripScheme(opts.OTLPEndpoint)),
		}
		if opts.OTLPInsecure || isInsecureEndpoint(opts.OTLPEndpoint) {
			httpOpts = append(httpOpts, otlptracehttp.WithInsecure())
		}
		if len(opts.OTLPHeaders) > 0 {
			httpOpts = append(httpOpts, otlptracehttp.WithHeaders(opts.OTLPHeaders))
		}
		exp, err := otlptrace.New(ctx, otlptracehttp.NewClient(httpOpts...))
		if err != nil {
			return nil, fmt.Errorf("tracing: build OTLP exporter: %w", err)
		}
		exporters = append(exporters, exp)
	}
	if opts.Stdout {
		exp, err := stdouttrace.New(stdouttrace.WithWriter(os.Stderr))
		if err != nil {
			return nil, fmt.Errorf("tracing: build stdout exporter: %w", err)
		}
		exporters = append(exporters, exp)
	}

	if len(exporters) == 0 {
		// Nothing configured — leave global tracer as no-op (the
		// default), and return a no-op shutdown so the caller's
		// `defer shutdown(ctx)` works either way.
		mu.Lock()
		defer mu.Unlock()
		current = &state{shutdown: func(context.Context) error { return nil }}
		return current.shutdown, nil
	}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.ServiceVersion),
	)

	sampler := opts.Sampler
	if sampler == nil {
		sampler = sdktrace.ParentBased(sdktrace.AlwaysSample())
	}

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	}
	for _, exp := range exporters {
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
		))
	}
	tp := sdktrace.NewTracerProvider(tpOpts...)
	otel.SetTracerProvider(tp)

	mu.Lock()
	defer mu.Unlock()
	if current != nil && current.shutdown != nil {
		// Shut down the previous tracer; ignore errors (tests).
		_ = current.shutdown(ctx)
	}
	current = &state{
		tp:       tp,
		shutdown: tp.Shutdown,
	}
	return tp.Shutdown, nil
}

// Tracer returns the package's named tracer. Always safe to call —
// when Init hasn't run, the global tracer is a no-op.
func Tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(TracerName)
}

// Reset reverts the global tracer to a noop. Used by tests + the
// dispatcher's clean-shutdown path so a misbehaving exporter at
// process exit doesn't block the binary.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	if current != nil && current.shutdown != nil {
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = current.shutdown(shCtx)
		cancel()
	}
	otel.SetTracerProvider(noop.NewTracerProvider())
	current = nil
}

// stripScheme normalises "http://host:port" → "host:port" for the
// OTLP/HTTP client (which takes a bare host:port). When the user's
// config omits the scheme, we use the raw value as-is.
func stripScheme(endpoint string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if len(endpoint) > len(prefix) && endpoint[:len(prefix)] == prefix {
			return endpoint[len(prefix):]
		}
	}
	return endpoint
}

// isInsecureEndpoint returns true when the endpoint's scheme is
// `http://`. The OTLP/HTTP exporter defaults to TLS, so we need to
// flip the insecure flag explicitly when the operator wires a
// plaintext localhost collector.
func isInsecureEndpoint(endpoint string) bool {
	const prefix = "http://"
	return len(endpoint) > len(prefix) && endpoint[:len(prefix)] == prefix
}

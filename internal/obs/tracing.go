package obs

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the gateway's tracer. By default it's backed by a real SDK
// TracerProvider with no exporter attached - not otel.Tracer's global
// no-op provider, which was the earlier (wrong) choice here: a no-op span
// has an all-zero, invalid SpanContext, and trace_id is a required
// response field whether or not InitTracing was ever called (every test
// in this repo never calls it). An SDK provider with no processor still
// generates real, valid, random trace/span ids for every span - it just
// has nowhere to send them until InitTracing attaches a real exporter and
// swaps this var for a provider that does.
var Tracer = sdktrace.NewTracerProvider().Tracer("emathp/gateway")

// InitTracing points Tracer at an OTLP/HTTP collector - Jaeger's
// all-in-one image accepts OTLP directly, no separate collector needed.
// endpoint is host:port with no scheme (e.g. "jaeger:4318"), matching
// otlptracehttp's own convention. Returns a shutdown func that flushes
// pending spans; callers should defer it.
//
// Building the exporter doesn't dial anything - otlptracehttp connects
// lazily, on the first export - so a collector that isn't up yet (or
// never comes up) doesn't fail startup or block requests; spans just fail
// to export in the background, logged, not fatal.
func InitTracing(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("obs: build OTLP exporter: %w", err)
	}

	res := resource.NewSchemaless(attribute.String("service.name", "emathp-gateway"))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	Tracer = tp.Tracer("emathp/gateway")

	return tp.Shutdown, nil
}

// TraceIDFrom recovers the current trace id from ctx's span context - ""
// if none was ever attached (e.g. a unit test calling a connector
// directly, with no tracing wired up at all).
func TraceIDFrom(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// DetachTraceContext carries ctx's span context forward onto a fresh
// background context - for the async job goroutine (Cycle 8), which
// deliberately does NOT inherit the originating request's cancellation,
// but still needs every span it starts (e.g. connector.fetch) to nest
// under the SAME real "gateway.query" span handleQuery already started -
// not a synthetic stand-in, since that span genuinely exists and will
// export correctly once the goroutine ends it (see startAsync).
func DetachTraceContext(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
}

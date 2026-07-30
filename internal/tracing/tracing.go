// Package tracing bridges OpenTelemetry span context and the
// controller-runtime context logger. Starting a span through this package
// enriches the logger stored in the context with the active trace and span
// IDs, so every downstream log.FromContext / ctrl.LoggerFrom caller emits
// correlated log lines without any change of its own.
//
// The fields are named trace_id and span_id, matching the OpenTelemetry log
// data model and the derived-field defaults of backends such as Grafana
// Tempo/Loki and Jaeger.
//
// When tracing is not configured the global provider returns non-recording
// spans whose span context is invalid; in that case the logger is left
// untouched and log output is unchanged.
package tracing

import (
	"context"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// TraceIDKey is the log field carrying the active trace ID.
	TraceIDKey = "trace_id"

	// SpanIDKey is the log field carrying the active span ID.
	SpanIDKey = "span_id"
)

// instrumentationName identifies this project as the instrumentation scope of
// the spans it produces.
const instrumentationName = "github.com/llm-d/llm-d-workload-variant-autoscaler"

// baseLoggerKey is the context key under which the logger as it was before
// trace fields were added is kept. Re-deriving from that logger keeps nested
// spans from appending a second trace_id/span_id pair to the same line.
type baseLoggerKey struct{}

// Tracer returns the tracer used for all spans emitted by this project. It
// resolves against the global provider on every call, so it is safe to use
// before the provider is installed.
func Tracer() trace.Tracer {
	return otel.Tracer(instrumentationName)
}

// StartSpan starts a span named name and returns a context carrying both the
// span and a logger enriched with the span's trace and span IDs. The caller
// must end the returned span.
//
// Use it in place of a direct tracer.Start so span creation and logger
// enrichment cannot drift apart:
//
//	ctx, span := tracing.StartSpan(ctx, "wva.reconcile")
//	defer span.End()
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := Tracer().Start(ctx, name, opts...)
	return ContextWithTraceLogger(ctx), span
}

// ContextWithTraceLogger enriches the logger in ctx with the IDs of the span
// ctx already carries. It returns ctx unchanged when there is no valid span
// context, which is the case whenever tracing is disabled. Use it at entry
// points where the span was started elsewhere, such as a context extracted
// from an incoming request; code that starts its own span should call
// StartSpan instead.
func ContextWithTraceLogger(ctx context.Context) context.Context {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return ctx
	}

	base, ok := ctx.Value(baseLoggerKey{}).(logr.Logger)
	if !ok {
		base = log.FromContext(ctx)
		ctx = context.WithValue(ctx, baseLoggerKey{}, base)
	}
	return log.IntoContext(ctx, LoggerWithSpanContext(base, spanCtx))
}

// LoggerWithSpanContext returns logger with the trace and span IDs of spanCtx
// attached. An invalid span context leaves logger unchanged, so no empty
// fields are ever emitted.
func LoggerWithSpanContext(logger logr.Logger, spanCtx trace.SpanContext) logr.Logger {
	if !spanCtx.IsValid() {
		return logger
	}
	return logger.WithValues(
		TraceIDKey, spanCtx.TraceID().String(),
		SpanIDKey, spanCtx.SpanID().String(),
	)
}

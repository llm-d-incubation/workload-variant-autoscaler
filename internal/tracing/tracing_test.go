package tracing

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TestMain installs a recording tracer provider for the whole package so
// StartSpan produces valid span contexts. Cases that exercise the
// tracing-disabled path use an explicit noop tracer instead.
func TestMain(m *testing.M) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	code := m.Run()
	_ = tp.Shutdown(context.Background())
	os.Exit(code)
}

// capturingLogger returns a logger that records every rendered line and the
// slice it records into.
func capturingLogger() (logr.Logger, *[]string) {
	lines := &[]string{}
	logger := funcr.New(func(prefix, args string) {
		*lines = append(*lines, prefix+" "+args)
	}, funcr.Options{})
	return logger, lines
}

// field renders the key/value pair the way funcr does, for substring
// assertions against captured lines.
func field(key, value string) string {
	return `"` + key + `"="` + value + `"`
}

func TestStartSpanEnrichesContextLogger(t *testing.T) {
	logger, lines := capturingLogger()
	ctx := log.IntoContext(context.Background(), logger)

	ctx, span := StartSpan(ctx, "wva.reconcile")
	defer span.End()

	spanCtx := span.SpanContext()
	require.True(t, spanCtx.IsValid(), "expected a valid span context from the recording provider")

	log.FromContext(ctx).Info("reconciling")

	require.Len(t, *lines, 1)
	assert.Contains(t, (*lines)[0], field(TraceIDKey, spanCtx.TraceID().String()))
	assert.Contains(t, (*lines)[0], field(SpanIDKey, spanCtx.SpanID().String()))
}

func TestStartSpanPreservesExistingLoggerValues(t *testing.T) {
	logger, lines := capturingLogger()
	ctx := log.IntoContext(context.Background(), logger.WithValues("variantAutoscaling", "vllm-llama"))

	ctx, span := StartSpan(ctx, "wva.optimize")
	defer span.End()

	log.FromContext(ctx).Info("optimizing")

	require.Len(t, *lines, 1)
	assert.Contains(t, (*lines)[0], field("variantAutoscaling", "vllm-llama"))
	assert.Contains(t, (*lines)[0], field(SpanIDKey, span.SpanContext().SpanID().String()))
}

func TestNestedSpansReplaceTraceFields(t *testing.T) {
	logger, lines := capturingLogger()
	ctx := log.IntoContext(context.Background(), logger)

	ctx, parent := StartSpan(ctx, "wva.reconcile")
	defer parent.End()

	ctx, child := StartSpan(ctx, "wva.collect")
	defer child.End()

	require.NotEqual(t, parent.SpanContext().SpanID(), child.SpanContext().SpanID())

	log.FromContext(ctx).Info("collecting")

	require.Len(t, *lines, 1)
	line := (*lines)[0]

	// The child's IDs win, and the parent's are not left behind as a second
	// pair of fields on the same line.
	assert.Contains(t, line, field(SpanIDKey, child.SpanContext().SpanID().String()))
	assert.NotContains(t, line, field(SpanIDKey, parent.SpanContext().SpanID().String()))
	assert.Equal(t, 1, strings.Count(line, `"`+SpanIDKey+`"`))
	assert.Equal(t, 1, strings.Count(line, `"`+TraceIDKey+`"`))

	// Both spans belong to the same trace.
	assert.Equal(t, parent.SpanContext().TraceID(), child.SpanContext().TraceID())
	assert.Contains(t, line, field(TraceIDKey, child.SpanContext().TraceID().String()))
}

func TestContextWithTraceLoggerIsIdempotent(t *testing.T) {
	logger, lines := capturingLogger()
	ctx := log.IntoContext(context.Background(), logger)

	ctx, span := StartSpan(ctx, "wva.analyze")
	defer span.End()

	ctx = ContextWithTraceLogger(ctx)
	log.FromContext(ctx).Info("analyzing")

	require.Len(t, *lines, 1)
	assert.Equal(t, 1, strings.Count((*lines)[0], `"`+TraceIDKey+`"`))
	assert.Equal(t, 1, strings.Count((*lines)[0], `"`+SpanIDKey+`"`))
}

func TestTracingDisabledLeavesLogsUnchanged(t *testing.T) {
	logger, lines := capturingLogger()
	baseCtx := log.IntoContext(context.Background(), logger)

	// A noop tracer is what the global provider hands out when tracing is
	// not configured: the span context it produces is invalid.
	ctx, span := noop.NewTracerProvider().Tracer("test").Start(baseCtx, "wva.reconcile")
	defer span.End()
	require.False(t, span.SpanContext().IsValid())

	enriched := ContextWithTraceLogger(ctx)
	assert.Equal(t, ctx, enriched, "context must be returned unchanged when there is no valid span")

	log.FromContext(enriched).Info("reconciling")

	require.Len(t, *lines, 1)
	assert.NotContains(t, (*lines)[0], TraceIDKey)
	assert.NotContains(t, (*lines)[0], SpanIDKey)
}

func TestContextWithTraceLoggerWithoutSpan(t *testing.T) {
	logger, lines := capturingLogger()
	ctx := log.IntoContext(context.Background(), logger)

	assert.Equal(t, ctx, ContextWithTraceLogger(ctx))

	log.FromContext(ContextWithTraceLogger(ctx)).Info("no span here")

	require.Len(t, *lines, 1)
	assert.NotContains(t, (*lines)[0], TraceIDKey)
	assert.NotContains(t, (*lines)[0], SpanIDKey)
}

func TestLoggerWithSpanContextInvalid(t *testing.T) {
	logger, lines := capturingLogger()

	LoggerWithSpanContext(logger, trace.SpanContext{}).Info("no fields")

	require.Len(t, *lines, 1)
	assert.NotContains(t, (*lines)[0], TraceIDKey)
	assert.NotContains(t, (*lines)[0], SpanIDKey)
}

func TestLoggerWithSpanContextValid(t *testing.T) {
	logger, lines := capturingLogger()

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	})

	LoggerWithSpanContext(logger, spanCtx).Info("correlated")

	require.Len(t, *lines, 1)
	assert.Contains(t, (*lines)[0], field(TraceIDKey, "4bf92f3577b34da6a3ce929d0e0e4736"))
	assert.Contains(t, (*lines)[0], field(SpanIDKey, "00f067aa0ba902b7"))
}

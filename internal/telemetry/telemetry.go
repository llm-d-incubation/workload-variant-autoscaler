package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"google.golang.org/grpc/credentials"
	ctrl "sigs.k8s.io/controller-runtime"
)

const serviceName = "workload-variant-autoscaler"

var (
	Tracer = otel.Tracer(serviceName)
	Meter  = otel.Meter(serviceName)
	Logger = otelslog.NewLogger(serviceName)
)

// setupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOTelSDK(ctx context.Context, cfg *config.Config) (func(context.Context) error, error) {
	logger := ctrl.LoggerFrom(ctx)
	if cfg.OtelTargetEndpointGrpc() == "" {
		logger.Info("No OTLP gRPC endpoint configured, skipping OpenTelemetry setup")
		return func(context.Context) error { return nil }, nil
	}

	var shutdownFuncs []func(context.Context) error
	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		_ = errors.Join(inErr, shutdown(ctx))
	}

	// Set up propagator.
	prop := newPropagator()
	otel.SetTextMapPropagator(prop)

	// Define comprehensive resource attributes
	resource, err := resource.New(
		ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			//semconv.ServiceVersion("1.0.0"), // Add these metadata as needed
			//semconv.ServiceInstanceID("instance-123"),
			//semconv.DeploymentEnvironment("production"),
			//semconv.ServiceNamespace("namespace"),
		),
		// Detect resource attributes from environment
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
	)
	if err != nil {
		return nil, err
	}

	var tlsConfig *tls.Config
	caCertPath := cfg.OtelCaCertPath()
	if !cfg.OtelInsecureSkipVerify() {
		// Load CA certificate to verify server
		caCert, err := os.ReadFile(caCertPath)
		if err != nil {
			logger.Error(err, "Failed to read CA certificate", "caCertPath", caCertPath)
			return nil, err
		}

		// Create certificate pool and add CA certificate
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			return nil, errors.New("failed to append CA certificate to cert pool")
		}

		// Create TLS configuration
		tlsConfig = &tls.Config{
			RootCAs:    caCertPool,
			MinVersion: tls.VersionTLS13,
		}
	}

	// Set up trace provider.
	tracerProvider, err := newTracerProvider(ctx, cfg, resource, tlsConfig)
	if err != nil {
		handleErr(err)
		return shutdown, err
	}
	shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
	otel.SetTracerProvider(tracerProvider)

	// Set up meter provider.
	meterProvider, err := newMeterProvider() // Fill in and configure as needed
	if err != nil {
		handleErr(err)
		return shutdown, err
	}
	shutdownFuncs = append(shutdownFuncs, meterProvider.Shutdown)
	otel.SetMeterProvider(meterProvider)

	// Set up logger provider.
	loggerProvider, err := newLoggerProvider() // Fill in and configure as needed
	if err != nil {
		handleErr(err)
		return shutdown, err
	}
	shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
	global.SetLoggerProvider(loggerProvider)

	logger.Info("OpenTelemetry setup finished successfully",
		"targetEndpointGrpc", cfg.OtelTargetEndpointGrpc(),
		"otelInsecureSkipVerify", cfg.OtelInsecureSkipVerify(),
		"otelCaCertPath", cfg.OtelCaCertPath())
	return shutdown, err
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func newTracerProvider(ctx context.Context, cfg *config.Config, resource *resource.Resource, tlsConfig *tls.Config) (*trace.TracerProvider, error) {
	// Configure OTLP exporter. For production, use TLS and retry logic.
	var secureOption otlptracegrpc.Option
	if cfg.OtelInsecureSkipVerify() {
		secureOption = otlptracegrpc.WithInsecure()
	} else {
		secureOption = otlptracegrpc.WithTLSCredentials(credentials.NewTLS(tlsConfig))
	}

	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OtelTargetEndpointGrpc()),
		secureOption,
		otlptracegrpc.WithTimeout(5*time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{
			Enabled:         true,
			InitialInterval: 1 * time.Second,
			MaxInterval:     30 * time.Second,
			MaxElapsedTime:  5 * time.Minute,
		}))

	if err != nil {
		return nil, err
	}

	tracerProvider := trace.NewTracerProvider(
		trace.WithBatcher(traceExporter),
		trace.WithResource(resource),
	)
	return tracerProvider, nil
}

func newMeterProvider() (*metric.MeterProvider, error) {
	meterProvider := metric.NewMeterProvider()
	return meterProvider, nil
}

func newLoggerProvider() (*log.LoggerProvider, error) {
	loggerProvider := log.NewLoggerProvider()
	return loggerProvider, nil
}

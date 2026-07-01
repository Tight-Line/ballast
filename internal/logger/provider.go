package logger

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// ProviderConfig carries the configuration for SetupLoggerProvider. It mirrors
// the OTLP fields of the metrics package's Config so the log and metric signals
// share one collector endpoint.
type ProviderConfig struct {
	// OTLPEndpoint is the OTLP collector address (e.g. "localhost:4317"). Leave
	// empty to disable OTLP log export.
	OTLPEndpoint string

	// OTLPProtocol is "grpc" (default) or "http/protobuf".
	OTLPProtocol string

	// OTLPInsecure disables TLS for the OTLP connection.
	OTLPInsecure bool
}

// SetupLoggerProvider creates an OTel LoggerProvider that pushes log records to
// an OTLP collector via a batch processor. The returned shutdown function
// flushes and stops the processor; call it before process exit. When
// OTLPEndpoint is empty it returns a nil provider and a no-op shutdown so the
// caller can fall back to stdout-only logging.
func SetupLoggerProvider(ctx context.Context, cfg ProviderConfig) (*sdklog.LoggerProvider, func(context.Context) error, error) {
	if cfg.OTLPEndpoint == "" {
		return nil, func(context.Context) error { return nil }, nil
	}

	// Build resource: hardcoded defaults, then env vars win (OTEL_SERVICE_NAME,
	// OTEL_RESOURCE_ATTRIBUTES). Matches the metrics provider so logs and metrics
	// carry identical resource attributes. The Helm chart always sets those env vars.
	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(
			attribute.String("service.name", "ballast"),
			attribute.String("service.version", "dev"),
			attribute.String("service.namespace", "tightlinesoftware.com"),
		),
		sdkresource.WithFromEnv(),
	)
	if err != nil { // coverage:ignore - only fails for malformed OTEL_RESOURCE_ATTRIBUTES
		return nil, nil, fmt.Errorf("creating OTel resource: %w", err)
	}

	exp, err := buildOTLPLogExporter(ctx, cfg)
	if err != nil { // coverage:ignore - OTLP exporters connect lazily; construction-time errors require broken gRPC/HTTP setup
		return nil, nil, fmt.Errorf("creating OTLP log exporter: %w", err)
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	return provider, provider.Shutdown, nil
}

func buildOTLPLogExporter(ctx context.Context, cfg ProviderConfig) (sdklog.Exporter, error) {
	switch cfg.OTLPProtocol {
	case "http/protobuf":
		httpOpts := []otlploghttp.Option{otlploghttp.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			httpOpts = append(httpOpts, otlploghttp.WithInsecure())
		}
		return otlploghttp.New(ctx, httpOpts...)
	default: // "grpc"
		grpcOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			grpcOpts = append(grpcOpts, otlploggrpc.WithInsecure())
		}
		return otlploggrpc.New(ctx, grpcOpts...)
	}
}

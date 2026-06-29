/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metrics

import (
	"context"
	"fmt"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Config carries the configuration for SetupProvider.
type Config struct {
	// PrometheusRegisterer, when non-nil, registers OTel instruments into that
	// registerer. Pass prometheus.DefaultRegisterer to serve them on the existing
	// /metrics endpoint that controller-runtime exposes.
	PrometheusRegisterer promclient.Registerer

	// OTLPEndpoint is the OTLP collector address (e.g. "localhost:4317"). Leave
	// empty to disable OTLP push export.
	OTLPEndpoint string

	// OTLPProtocol is "grpc" (default) or "http/protobuf".
	OTLPProtocol string

	// OTLPInterval is the push interval for the OTLP reader. Defaults to 30s.
	OTLPInterval time.Duration

	// OTLPInsecure disables TLS for the OTLP connection.
	OTLPInsecure bool
}

// SetupProvider creates an OTel MeterProvider with readers for each enabled
// export path. The returned shutdown function flushes and stops all readers;
// call it before process exit. Returns a no-reader provider when both Prometheus
// and OTLP are disabled.
func SetupProvider(ctx context.Context, cfg Config) (*sdkmetric.MeterProvider, func(context.Context) error, error) {
	var opts []sdkmetric.Option

	if cfg.PrometheusRegisterer != nil {
		exp, err := promexporter.New(promexporter.WithRegisterer(cfg.PrometheusRegisterer))
		if err != nil { // coverage:ignore - promexporter.New only fails for nil registerer, checked above
			return nil, nil, fmt.Errorf("creating Prometheus exporter: %w", err)
		}
		opts = append(opts, sdkmetric.WithReader(exp))
	}

	if cfg.OTLPEndpoint != "" {
		interval := cfg.OTLPInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}

		exp, err := buildOTLPExporter(ctx, cfg)
		if err != nil { // coverage:ignore - OTLP exporters connect lazily; construction-time errors require broken gRPC/HTTP setup
			return nil, nil, fmt.Errorf("creating OTLP exporter: %w", err)
		}

		reader := sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))
		opts = append(opts, sdkmetric.WithReader(reader))
	}

	provider := sdkmetric.NewMeterProvider(opts...)
	return provider, provider.Shutdown, nil
}

func buildOTLPExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	switch cfg.OTLPProtocol {
	case "http/protobuf":
		httpOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			httpOpts = append(httpOpts, otlpmetrichttp.WithInsecure())
		}
		return otlpmetrichttp.New(ctx, httpOpts...)
	default: // "grpc"
		grpcOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			grpcOpts = append(grpcOpts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, grpcOpts...)
	}
}

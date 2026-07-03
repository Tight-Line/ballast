/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metrics

import (
	"context"
	"fmt"
	"strings"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	prombridge "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

// Config carries the configuration for SetupProvider.
type Config struct {
	// PrometheusRegisterer, when non-nil, registers OTel instruments into that
	// registerer. Pass controller-runtime's metrics.Registry to serve them on the
	// /metrics endpoint it exposes; the client_golang DefaultRegisterer is NOT
	// served there.
	PrometheusRegisterer promclient.Registerer

	// PrometheusGatherer, when non-nil, bridges every metric family it gathers
	// into the OTLP export stream (no effect when OTLPEndpoint is empty). Pass
	// controller-runtime's metrics.Registry to ship the workqueue, reconcile,
	// client-go, and process/Go-runtime metrics to the OTLP collector alongside
	// the ballast.* instruments.
	PrometheusGatherer promclient.Gatherer

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
	// Build resource: hardcoded defaults, then env vars win (OTEL_SERVICE_NAME,
	// OTEL_RESOURCE_ATTRIBUTES). The Helm chart always sets those env vars.
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

	var opts []sdkmetric.Option
	opts = append(opts, sdkmetric.WithResource(res))

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

		readerOpts := []sdkmetric.PeriodicReaderOption{sdkmetric.WithInterval(interval)}
		if cfg.PrometheusGatherer != nil {
			// The bridge is attached only to the OTLP reader, never to the
			// Prometheus exporter: the gathered families already live in a
			// Prometheus registry, so re-registering them would duplicate every
			// series on /metrics. The gatherer is filtered because when the
			// Prometheus path is also enabled, the exporter above mirrors the
			// native ballast.* instruments into the same registry; bridging
			// those back out would ship every ballast metric to the collector
			// twice under two spellings (ballast.foo and ballast_foo).
			gatherer := filteredGatherer{
				inner:        cfg.PrometheusGatherer,
				dropPrefixes: []string{"ballast_", "otel_scope_", "target_info"},
			}
			readerOpts = append(readerOpts, sdkmetric.WithProducer(
				prombridge.NewMetricProducer(prombridge.WithGatherer(gatherer))))
		}
		reader := sdkmetric.NewPeriodicReader(exp, readerOpts...)
		opts = append(opts, sdkmetric.WithReader(reader))
	}

	provider := sdkmetric.NewMeterProvider(opts...)
	return provider, provider.Shutdown, nil
}

// filteredGatherer wraps a prometheus.Gatherer and drops metric families whose
// name starts with any of dropPrefixes. It exists to keep instruments that are
// natively OTel out of the Prometheus→OTLP bridge (see SetupProvider).
type filteredGatherer struct {
	inner        promclient.Gatherer
	dropPrefixes []string
}

func (g filteredGatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.inner.Gather()
	if err != nil {
		return nil, err
	}
	kept := families[:0]
	for _, mf := range families {
		if !g.dropped(mf.GetName()) {
			kept = append(kept, mf)
		}
	}
	return kept, nil
}

func (g filteredGatherer) dropped(name string) bool {
	for _, p := range g.dropPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// deltaTemporality selects delta temporality for monotonic counters and
// histograms, cumulative for everything else. With cumulative temporality a
// counter series whose attribute set increments exactly once (e.g. one
// ballast.resize.applied per pod, which then sits in cooldown) is born already
// at its final value: the backend's increase/rate needs two samples to compute
// a difference, so the increment never renders on dashboards. Delta exports
// carry each interval's increment directly, so bursts inside a series' first
// export window (typically right after operator startup) chart correctly.
func deltaTemporality(ik sdkmetric.InstrumentKind) metricdata.Temporality {
	switch ik {
	case sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindHistogram:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}

func buildOTLPExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	switch cfg.OTLPProtocol {
	case "http/protobuf":
		httpOpts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(cfg.OTLPEndpoint),
			otlpmetrichttp.WithTemporalitySelector(deltaTemporality),
		}
		if cfg.OTLPInsecure {
			httpOpts = append(httpOpts, otlpmetrichttp.WithInsecure())
		}
		return otlpmetrichttp.New(ctx, httpOpts...)
	default: // "grpc"
		grpcOpts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlpmetricgrpc.WithTemporalitySelector(deltaTemporality),
		}
		if cfg.OTLPInsecure {
			grpcOpts = append(grpcOpts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, grpcOpts...)
	}
}

/*
Copyright 2026 Tight Line LLC.

Licensed under the MIT License. See LICENSE for the full text.
*/

package metrics_test

import (
	"context"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/tight-line/ballast/internal/metrics"
)

func newTestRecorder(t *testing.T) (*metrics.Recorder, *promclient.Registry) {
	t.Helper()
	reg := promclient.NewRegistry()
	exp, err := promexporter.New(promexporter.WithRegisterer(reg))
	if err != nil {
		t.Fatalf("creating prometheus exporter: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exp))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	rec, err := metrics.NewRecorder(provider)
	if err != nil {
		t.Fatalf("creating recorder: %v", err)
	}
	return rec, reg
}

func gatherCounter(t *testing.T, reg *promclient.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			if len(mf.GetMetric()) > 0 {
				return mf.GetMetric()[0].GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gatherGauge(t *testing.T, reg *promclient.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			if len(mf.GetMetric()) > 0 {
				return mf.GetMetric()[0].GetGauge().GetValue()
			}
		}
	}
	return 0
}

func TestRecorder_NilSafe(t *testing.T) {
	var rec *metrics.Recorder
	ctx := context.Background()
	rec.SampleCollected(ctx, "src", "cpu", "app", "hash")
	rec.FetchError(ctx, "src", "hash")
	rec.ProfileThresholdMet(ctx, "prof", "policy")
	rec.PodProcessed(ctx, "created", "default", "prof")
	rec.WorkloadProfileCreated(ctx, "prof")
	rec.WorkloadProfilePurged(ctx, "prof")
	rec.ResizeApplied(ctx, "prof", "policy", "default")
	rec.ResizeFailed(ctx, "prof", "policy", "default")
	rec.ResizeSkipped(ctx, "cooldown", "prof")
	rec.WebhookMutation(ctx, "mutated", "default", "prof")
	rec.KillSwitchTransition(ctx, "activated")
	rec.SetKillSwitchActive(true, "test")
}

func TestRecorder_SampleCollected(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.SampleCollected(ctx, "k8s", "cpu", "app", "abc123")
	rec.SampleCollected(ctx, "k8s", "cpu", "app", "abc123")

	got := gatherCounter(t, reg, "ballast_samples_collected_total")
	if got != 2 {
		t.Errorf("ballast_samples_collected_total = %v, want 2", got)
	}
}

func TestRecorder_FetchError(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.FetchError(ctx, "k8s", "abc123")

	got := gatherCounter(t, reg, "ballast_fetch_errors_total")
	if got != 1 {
		t.Errorf("ballast_fetch_errors_total = %v, want 1", got)
	}
}

func TestRecorder_ProfileThresholdMet(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ProfileThresholdMet(ctx, "frontend--web", "default")

	got := gatherCounter(t, reg, "ballast_profiles_threshold_met_total")
	if got != 1 {
		t.Errorf("ballast_profiles_threshold_met_total = %v, want 1", got)
	}
}

func TestRecorder_PodProcessed(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.PodProcessed(ctx, "created", "default", "frontend--web")

	got := gatherCounter(t, reg, "ballast_pods_processed_total")
	if got != 1 {
		t.Errorf("ballast_pods_processed_total = %v, want 1", got)
	}
}

func TestRecorder_WorkloadProfileCreated(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WorkloadProfileCreated(ctx, "frontend--web")

	got := gatherCounter(t, reg, "ballast_workload_profiles_created_total")
	if got != 1 {
		t.Errorf("ballast_workload_profiles_created_total = %v, want 1", got)
	}
}

func TestRecorder_WorkloadProfilePurged(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WorkloadProfilePurged(ctx, "frontend--web")

	got := gatherCounter(t, reg, "ballast_workload_profiles_purged_total")
	if got != 1 {
		t.Errorf("ballast_workload_profiles_purged_total = %v, want 1", got)
	}
}

func TestRecorder_ResizeApplied(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ResizeApplied(ctx, "frontend--web", "default", "staging")

	got := gatherCounter(t, reg, "ballast_resize_applied_total")
	if got != 1 {
		t.Errorf("ballast_resize_applied_total = %v, want 1", got)
	}
}

func TestRecorder_ResizeFailed(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ResizeFailed(ctx, "frontend--web", "default", "staging")

	got := gatherCounter(t, reg, "ballast_resize_failed_total")
	if got != 1 {
		t.Errorf("ballast_resize_failed_total = %v, want 1", got)
	}
}

func TestRecorder_ResizeSkipped(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ResizeSkipped(ctx, "cooldown", "frontend--web")

	got := gatherCounter(t, reg, "ballast_resize_skipped_total")
	if got != 1 {
		t.Errorf("ballast_resize_skipped_total = %v, want 1", got)
	}
}

func TestRecorder_WebhookMutation(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WebhookMutation(ctx, "mutated", "default", "frontend--web")

	got := gatherCounter(t, reg, "ballast_webhook_mutations_total")
	if got != 1 {
		t.Errorf("ballast_webhook_mutations_total = %v, want 1", got)
	}
}

func TestRecorder_KillSwitchTransition(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.KillSwitchTransition(ctx, "activated")

	got := gatherCounter(t, reg, "ballast_kill_switch_transitions_total")
	if got != 1 {
		t.Errorf("ballast_kill_switch_transitions_total = %v, want 1", got)
	}
}

func TestRecorder_KillSwitchActive_Gauge(t *testing.T) {
	rec, reg := newTestRecorder(t)

	rec.SetKillSwitchActive(true, "ConfigMap ballast-kill-switch")
	got := gatherGauge(t, reg, "ballast_kill_switch_active")
	if got != 1 {
		t.Errorf("ballast_kill_switch_active = %v after activate, want 1", got)
	}

	rec.SetKillSwitchActive(false, "")
	got = gatherGauge(t, reg, "ballast_kill_switch_active")
	if got != 0 {
		t.Errorf("ballast_kill_switch_active = %v after deactivate, want 0", got)
	}
}

func TestSetupProvider_NoOp(t *testing.T) {
	ctx := context.Background()
	provider, shutdown, err := metrics.SetupProvider(ctx, metrics.Config{})
	if err != nil {
		t.Fatalf("SetupProvider: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	rec, err := metrics.NewRecorder(provider)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.SampleCollected(ctx, "k8s", "cpu", "app", "hash")
}

// shortCtx returns a context that times out quickly, for use in shutdown calls
// against OTLP providers that have no backing server.
func shortCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 100*time.Millisecond)
}

func TestSetupProvider_OTLPGrpc(t *testing.T) {
	ctx := context.Background()
	provider, shutdown, err := metrics.SetupProvider(ctx, metrics.Config{
		OTLPEndpoint: "localhost:4317",
		OTLPProtocol: "grpc",
		OTLPInterval: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("SetupProvider grpc: %v", err)
	}
	t.Cleanup(func() { sctx, cancel := shortCtx(t); defer cancel(); _ = shutdown(sctx) })
	rec, err := metrics.NewRecorder(provider)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.SampleCollected(ctx, "k8s", "cpu", "app", "hash")
}

func TestSetupProvider_OTLPDefaultInterval(t *testing.T) {
	ctx := context.Background()
	// OTLPInterval=0 triggers the default-30s interval path.
	_, shutdown, err := metrics.SetupProvider(ctx, metrics.Config{
		OTLPEndpoint: "localhost:4317",
		OTLPInterval: 0,
	})
	if err != nil {
		t.Fatalf("SetupProvider default interval: %v", err)
	}
	t.Cleanup(func() { sctx, cancel := shortCtx(t); defer cancel(); _ = shutdown(sctx) })
}

func TestSetupProvider_OTLPHttp(t *testing.T) {
	ctx := context.Background()
	_, shutdown, err := metrics.SetupProvider(ctx, metrics.Config{
		OTLPEndpoint: "localhost:4318",
		OTLPProtocol: "http/protobuf",
		OTLPInterval: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("SetupProvider http: %v", err)
	}
	t.Cleanup(func() { sctx, cancel := shortCtx(t); defer cancel(); _ = shutdown(sctx) })
}

func TestSetupProvider_OTLPInsecure(t *testing.T) {
	ctx := context.Background()
	// grpc insecure
	_, s1, err := metrics.SetupProvider(ctx, metrics.Config{
		OTLPEndpoint: "localhost:4317",
		OTLPInsecure: true,
		OTLPInterval: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("SetupProvider grpc insecure: %v", err)
	}
	sctx1, cancel1 := shortCtx(t)
	defer cancel1()
	_ = s1(sctx1)

	// http insecure
	_, s2, err := metrics.SetupProvider(ctx, metrics.Config{
		OTLPEndpoint: "localhost:4318",
		OTLPProtocol: "http/protobuf",
		OTLPInsecure: true,
		OTLPInterval: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("SetupProvider http insecure: %v", err)
	}
	t.Cleanup(func() { sctx, cancel := shortCtx(t); defer cancel(); _ = s2(sctx) })
}

func TestSetupProvider_Prometheus(t *testing.T) {
	ctx := context.Background()
	reg := promclient.NewRegistry()
	provider, shutdown, err := metrics.SetupProvider(ctx, metrics.Config{
		PrometheusRegisterer: reg,
	})
	if err != nil {
		t.Fatalf("SetupProvider: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	rec, err := metrics.NewRecorder(provider)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	rec.SampleCollected(ctx, "k8s", "cpu", "app", "hash")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	found := false
	for _, mf := range mfs {
		if mf.GetName() == "ballast_samples_collected_total" {
			found = true
		}
	}
	if !found {
		t.Error("ballast_samples_collected_total not found after SetupProvider with Prometheus")
	}
}

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

// labelSet is a name->value map of one metric sample's labels.
type labelSet map[string]string

// gatherSeries returns every sample of the named metric as (labels, value) pairs.
// It reads counter or gauge values, whichever is populated.
func gatherSeries(t *testing.T, reg *promclient.Registry, name string) []struct {
	labels labelSet
	value  float64
} {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []struct {
		labels labelSet
		value  float64
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			ls := make(labelSet, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				ls[lp.GetName()] = lp.GetValue()
			}
			val := m.GetCounter().GetValue()
			if m.GetGauge() != nil {
				val = m.GetGauge().GetValue()
			}
			out = append(out, struct {
				labels labelSet
				value  float64
			}{labels: ls, value: val})
		}
	}
	return out
}

// firstLabels returns the label set of the first sample of the named metric.
func firstLabels(t *testing.T, reg *promclient.Registry, name string) labelSet {
	t.Helper()
	series := gatherSeries(t, reg, name)
	if len(series) == 0 {
		t.Fatalf("no series found for metric %q", name)
	}
	return series[0].labels
}

// biz is an identity-tuple label map used across attribute-mapping tests.
func bizLabels() map[string]string {
	return map[string]string{
		"example.com/business-unit": "payments",
		"app.kubernetes.io/name":    "web",
	}
}

func TestRecorder_NilSafe(t *testing.T) {
	var rec *metrics.Recorder
	ctx := context.Background()
	id := metrics.ProfileID{Name: "prof", Labels: map[string]string{"app.kubernetes.io/name": "web"}}
	rec.SampleCollected(ctx, "src", "cpu", "app", id)
	rec.FetchError(ctx, "src", id)
	rec.ProfileThresholdMet(ctx, id, "policy")
	rec.PodProcessed(ctx, "created", "default", id)
	rec.WorkloadProfileCreated(ctx, id)
	rec.WorkloadProfilePurged(ctx, id)
	rec.ResizeApplied(ctx, id, "policy", "default")
	rec.ResizeFailed(ctx, id, "policy", "default")
	rec.ResizeSkipped(ctx, "cooldown", id)
	rec.WebhookMutation(ctx, "mutated", "default", id)
	rec.KillSwitchTransition(ctx, "activated")
	rec.SetKillSwitchActive(true, "test")
	if err := rec.RegisterProfileGauge(nil); err != nil {
		t.Errorf("RegisterProfileGauge on nil recorder = %v, want nil", err)
	}
}

func TestRecorder_SampleCollected(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	id := metrics.ProfileID{Name: "payments--web", Labels: bizLabels()}
	rec.SampleCollected(ctx, "k8s", "cpu", "app", id)
	rec.SampleCollected(ctx, "k8s", "cpu", "app", id)

	got := gatherCounter(t, reg, "ballast_samples_collected_total")
	if got != 2 {
		t.Errorf("ballast_samples_collected_total = %v, want 2", got)
	}

	// samples.collected now carries the readable profile name and tuple attributes.
	ls := firstLabels(t, reg, "ballast_samples_collected_total")
	if ls["profile"] != "payments--web" {
		t.Errorf("profile attr = %q, want payments--web", ls["profile"])
	}
	if ls["business_unit"] != "payments" {
		t.Errorf("business_unit attr = %q, want payments", ls["business_unit"])
	}
	if ls["name"] != "web" {
		t.Errorf("name attr = %q, want web", ls["name"])
	}
	if ls["source"] != "k8s" || ls["resource"] != "cpu" || ls["container"] != "app" {
		t.Errorf("source/resource/container attrs = %q/%q/%q", ls["source"], ls["resource"], ls["container"])
	}
}

func TestRecorder_FetchError(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.FetchError(ctx, "k8s", metrics.ProfileID{Name: "payments--web", Labels: bizLabels()})

	got := gatherCounter(t, reg, "ballast_fetch_errors_total")
	if got != 1 {
		t.Errorf("ballast_fetch_errors_total = %v, want 1", got)
	}

	// fetch.errors now carries the readable profile name and tuple attributes.
	ls := firstLabels(t, reg, "ballast_fetch_errors_total")
	if ls["profile"] != "payments--web" {
		t.Errorf("profile attr = %q, want payments--web", ls["profile"])
	}
	if ls["business_unit"] != "payments" || ls["name"] != "web" {
		t.Errorf("tuple attrs = business_unit:%q name:%q", ls["business_unit"], ls["name"])
	}
	if ls["source"] != "k8s" {
		t.Errorf("source attr = %q, want k8s", ls["source"])
	}
}

func TestRecorder_ProfileThresholdMet(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ProfileThresholdMet(ctx, metrics.ProfileID{Name: "frontend--web"}, "default")

	got := gatherCounter(t, reg, "ballast_profiles_threshold_met_total")
	if got != 1 {
		t.Errorf("ballast_profiles_threshold_met_total = %v, want 1", got)
	}
}

func TestRecorder_PodProcessed(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.PodProcessed(ctx, "created", "default", metrics.ProfileID{Name: "frontend--web"})

	got := gatherCounter(t, reg, "ballast_pods_processed_total")
	if got != 1 {
		t.Errorf("ballast_pods_processed_total = %v, want 1", got)
	}
}

func TestRecorder_WorkloadProfileCreated(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WorkloadProfileCreated(ctx, metrics.ProfileID{Name: "frontend--web"})

	got := gatherCounter(t, reg, "ballast_workload_profiles_created_total")
	if got != 1 {
		t.Errorf("ballast_workload_profiles_created_total = %v, want 1", got)
	}
}

func TestRecorder_WorkloadProfilePurged(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WorkloadProfilePurged(ctx, metrics.ProfileID{Name: "frontend--web"})

	got := gatherCounter(t, reg, "ballast_workload_profiles_purged_total")
	if got != 1 {
		t.Errorf("ballast_workload_profiles_purged_total = %v, want 1", got)
	}
}

func TestRecorder_ResizeApplied(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ResizeApplied(ctx, metrics.ProfileID{Name: "frontend--web"}, "default", "staging")

	got := gatherCounter(t, reg, "ballast_resize_applied_total")
	if got != 1 {
		t.Errorf("ballast_resize_applied_total = %v, want 1", got)
	}
}

func TestRecorder_ResizeFailed(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ResizeFailed(ctx, metrics.ProfileID{Name: "frontend--web"}, "default", "staging")

	got := gatherCounter(t, reg, "ballast_resize_failed_total")
	if got != 1 {
		t.Errorf("ballast_resize_failed_total = %v, want 1", got)
	}
}

func TestRecorder_ResizeSkipped(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.ResizeSkipped(ctx, "cooldown", metrics.ProfileID{Name: "frontend--web"})

	got := gatherCounter(t, reg, "ballast_resize_skipped_total")
	if got != 1 {
		t.Errorf("ballast_resize_skipped_total = %v, want 1", got)
	}
}

func TestRecorder_WebhookMutation(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WebhookMutation(ctx, "mutated", "default", metrics.ProfileID{Name: "frontend--web"})

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
	rec.SampleCollected(ctx, "k8s", "cpu", "app", metrics.ProfileID{Name: "prof"})
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
	rec.SampleCollected(ctx, "k8s", "cpu", "app", metrics.ProfileID{Name: "prof"})
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
	rec.SampleCollected(ctx, "k8s", "cpu", "app", metrics.ProfileID{Name: "prof"})

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

// TestRecorder_TupleAttrs_SuffixCollision asserts that when two identity-tuple labels
// share a suffix (the segment after the last '/'), both fall back to their sanitized
// fully-qualified key so neither attribute is dropped.
func TestRecorder_TupleAttrs_SuffixCollision(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	id := metrics.ProfileID{
		Name: "collide",
		Labels: map[string]string{
			"a.example.com/x": "one",
			"b.example.com/x": "two",
		},
	}
	rec.WorkloadProfileCreated(ctx, id)

	ls := firstLabels(t, reg, "ballast_workload_profiles_created_total")
	// Suffix "x" collides, so both keys sanitize to their full form.
	if ls["a_example_com_x"] != "one" {
		t.Errorf("a_example_com_x attr = %q, want one", ls["a_example_com_x"])
	}
	if ls["b_example_com_x"] != "two" {
		t.Errorf("b_example_com_x attr = %q, want two", ls["b_example_com_x"])
	}
	// The bare suffix must NOT be present (it would be ambiguous).
	if _, ok := ls["x"]; ok {
		t.Errorf("bare suffix attr %q should not be present on collision", "x")
	}
}

// TestRecorder_TupleAttrs_NoSlashKey asserts a label key with no '/' is used whole
// as the attribute key (after sanitization).
func TestRecorder_TupleAttrs_NoSlashKey(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	id := metrics.ProfileID{Name: "noslash", Labels: map[string]string{"team": "core"}}
	rec.WorkloadProfileCreated(ctx, id)

	ls := firstLabels(t, reg, "ballast_workload_profiles_created_total")
	if ls["team"] != "core" {
		t.Errorf("team attr = %q, want core", ls["team"])
	}
}

// TestRecorder_ZeroProfileID asserts a zero ProfileID contributes no profile or
// tuple attributes, only the metric's own attributes.
func TestRecorder_ZeroProfileID(t *testing.T) {
	rec, reg := newTestRecorder(t)
	ctx := context.Background()

	rec.WebhookMutation(ctx, "kill_switch", "default", metrics.ProfileID{})

	ls := firstLabels(t, reg, "ballast_webhook_mutations_total")
	if _, ok := ls["profile"]; ok {
		t.Errorf("profile attr present on zero ProfileID: %q", ls["profile"])
	}
	if ls["result"] != "kill_switch" || ls["namespace"] != "default" {
		t.Errorf("result/namespace attrs = %q/%q", ls["result"], ls["namespace"])
	}
	// Only the OTel-required target_info/scope labels plus result+namespace should exist;
	// no suffix-derived tuple attribute keys.
	for _, forbidden := range []string{"business_unit", "name", "x"} {
		if _, ok := ls[forbidden]; ok {
			t.Errorf("unexpected tuple attr %q on zero ProfileID", forbidden)
		}
	}
}

// fakeLister is a test double for metrics.ProfileLister.
type fakeLister struct {
	snaps []metrics.ProfileSnapshot
	err   error
}

func (f fakeLister) ListProfiles(_ context.Context) ([]metrics.ProfileSnapshot, error) {
	return f.snaps, f.err
}

func TestRecorder_RegisterProfileGauge(t *testing.T) {
	rec, reg := newTestRecorder(t)

	lister := fakeLister{snaps: []metrics.ProfileSnapshot{
		{Labels: map[string]string{"example.com/business-unit": "payments"}, Ready: true},
		{Labels: map[string]string{"example.com/business-unit": "search"}, Ready: false},
	}}
	if err := rec.RegisterProfileGauge(lister); err != nil {
		t.Fatalf("RegisterProfileGauge: %v", err)
	}

	series := gatherSeries(t, reg, "ballast_profiles")
	if len(series) != 2 {
		t.Fatalf("ballast_profiles series count = %d, want 2", len(series))
	}

	byState := make(map[string]labelSet)
	for _, s := range series {
		if s.value != 1 {
			t.Errorf("ballast_profiles value = %v, want 1 (state=%q)", s.value, s.labels["state"])
		}
		byState[s.labels["state"]] = s.labels
	}

	ready, ok := byState["ready"]
	if !ok {
		t.Fatal("no ballast_profiles series with state=ready")
	}
	if ready["business_unit"] != "payments" {
		t.Errorf("ready business_unit = %q, want payments", ready["business_unit"])
	}

	accruing, ok := byState["accruing"]
	if !ok {
		t.Fatal("no ballast_profiles series with state=accruing")
	}
	if accruing["business_unit"] != "search" {
		t.Errorf("accruing business_unit = %q, want search", accruing["business_unit"])
	}
}

func TestRecorder_RegisterProfileGauge_ListerError(t *testing.T) {
	rec, reg := newTestRecorder(t)

	if err := rec.RegisterProfileGauge(fakeLister{err: errListerBoom}); err != nil {
		t.Fatalf("RegisterProfileGauge: %v", err)
	}

	// Gather triggers the callback; the lister error propagates so no series is emitted.
	if got := gatherGauge(t, reg, "ballast_profiles"); got != 0 {
		t.Errorf("ballast_profiles = %v after lister error, want 0 (no series)", got)
	}
}

var errListerBoom = errBoom("lister boom")

type errBoom string

func (e errBoom) Error() string { return string(e) }
